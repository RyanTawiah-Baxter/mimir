// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/distributor/distributor.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package distributor

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/limiter"
	"github.com/grafana/dskit/ring"
	ring_client "github.com/grafana/dskit/ring/client"
	"github.com/grafana/dskit/services"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/relabel"
	"github.com/prometheus/prometheus/scrape"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/instrument"
	"github.com/weaveworks/common/user"
	"go.uber.org/atomic"
	"golang.org/x/sync/errgroup"

	ingester_client "github.com/grafana/mimir/pkg/ingester/client"
	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/prom1/storage/metric"
	"github.com/grafana/mimir/pkg/tenant"
	"github.com/grafana/mimir/pkg/util"
	"github.com/grafana/mimir/pkg/util/extract"
	util_log "github.com/grafana/mimir/pkg/util/log"
	util_math "github.com/grafana/mimir/pkg/util/math"
	"github.com/grafana/mimir/pkg/util/validation"
)

var (
	supportedShardingStrategies = []string{util.ShardingStrategyDefault, util.ShardingStrategyShuffle}

	// Validation errors.
	errInvalidShardingStrategy = errors.New("invalid sharding strategy")
	errInvalidTenantShardSize  = errors.New("invalid tenant shard size, the value must be greater than 0")

	// Distributor instance limits errors.
	errTooManyInflightPushRequests    = errors.New("too many inflight push requests in distributor")
	errMaxSamplesPushRateLimitReached = errors.New("distributor's samples push rate limit reached")
)

const (
	typeSamples  = "samples"
	typeMetadata = "metadata"

	instanceIngestionRateTickInterval = time.Second
)

// Distributor is a storage.SampleAppender and a client.Querier which
// forwards appends and queries to individual ingesters.
type Distributor struct {
	services.Service

	cfg           Config
	log           log.Logger
	ingestersRing ring.ReadRing
	ingesterPool  *ring_client.Pool
	limits        *validation.Overrides

	// The global rate limiter requires a distributors ring to count
	// the number of healthy instances
	distributorsLifeCycler *ring.Lifecycler
	distributorsRing       *ring.Ring

	// For handling HA replicas.
	HATracker *haTracker

	// Per-user rate limiter.
	ingestionRateLimiter *limiter.RateLimiter

	// Manager for subservices (HA Tracker, distributor ring and client pool)
	subservices        *services.Manager
	subservicesWatcher *services.FailureWatcher

	activeUsers *util.ActiveUsersCleanupService

	ingestionRate        *util_math.EwmaRate
	inflightPushRequests atomic.Int64

	// Metrics
	queryDuration                    *instrument.HistogramCollector
	receivedSamples                  *prometheus.CounterVec
	receivedExemplars                *prometheus.CounterVec
	receivedMetadata                 *prometheus.CounterVec
	incomingSamples                  *prometheus.CounterVec
	incomingExemplars                *prometheus.CounterVec
	incomingMetadata                 *prometheus.CounterVec
	nonHASamples                     *prometheus.CounterVec
	dedupedSamples                   *prometheus.CounterVec
	labelsHistogram                  prometheus.Histogram
	ingesterAppends                  *prometheus.CounterVec
	ingesterAppendFailures           *prometheus.CounterVec
	ingesterQueries                  *prometheus.CounterVec
	ingesterQueryFailures            *prometheus.CounterVec
	replicationFactor                prometheus.Gauge
	latestSeenSampleTimestampPerUser *prometheus.GaugeVec
}

// Config contains the configuration required to
// create a Distributor
type Config struct {
	PoolConfig PoolConfig `yaml:"pool"`

	HATrackerConfig HATrackerConfig `yaml:"ha_tracker"`

	MaxRecvMsgSize  int           `yaml:"max_recv_msg_size"`
	RemoteTimeout   time.Duration `yaml:"remote_timeout"`
	ExtraQueryDelay time.Duration `yaml:"extra_queue_delay"`

	ShardingStrategy string `yaml:"sharding_strategy"`
	ShardByAllLabels bool   `yaml:"shard_by_all_labels"`
	ExtendWrites     bool   `yaml:"extend_writes"`

	// Distributors ring
	DistributorRing RingConfig `yaml:"ring"`

	// for testing and for extending the ingester by adding calls to the client
	IngesterClientFactory ring_client.PoolFactory `yaml:"-"`

	// when true the distributor does not validate the label name, Mimir doesn't directly use
	// this (and should never use it) but this feature is used by other projects built on top of it
	SkipLabelNameValidation bool `yaml:"-"`

	// This config is dynamically injected because defined in the querier config.
	ShuffleShardingLookbackPeriod time.Duration `yaml:"-"`

	// Limits for distributor
	InstanceLimits InstanceLimits `yaml:"instance_limits"`
}

type InstanceLimits struct {
	MaxIngestionRate        float64 `yaml:"max_ingestion_rate"`
	MaxInflightPushRequests int     `yaml:"max_inflight_push_requests"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.PoolConfig.RegisterFlags(f)
	cfg.HATrackerConfig.RegisterFlags(f)
	cfg.DistributorRing.RegisterFlags(f)

	f.IntVar(&cfg.MaxRecvMsgSize, "distributor.max-recv-msg-size", 100<<20, "remote_write API max receive message size (bytes).")
	f.DurationVar(&cfg.RemoteTimeout, "distributor.remote-timeout", 2*time.Second, "Timeout for downstream ingesters.")
	f.DurationVar(&cfg.ExtraQueryDelay, "distributor.extra-query-delay", 0, "Time to wait before sending more than the minimum successful query requests.")
	f.BoolVar(&cfg.ShardByAllLabels, "distributor.shard-by-all-labels", false, "Distribute samples based on all labels, as opposed to solely by user and metric name.")
	f.StringVar(&cfg.ShardingStrategy, "distributor.sharding-strategy", util.ShardingStrategyDefault, fmt.Sprintf("The sharding strategy to use. Supported values are: %s.", strings.Join(supportedShardingStrategies, ", ")))
	f.BoolVar(&cfg.ExtendWrites, "distributor.extend-writes", true, "Try writing to an additional ingester in the presence of an ingester not in the ACTIVE state. It is useful to disable this along with -ingester.unregister-on-shutdown=false in order to not spread samples to extra ingesters during rolling restarts with consistent naming.")

	f.Float64Var(&cfg.InstanceLimits.MaxIngestionRate, "distributor.instance-limits.max-ingestion-rate", 0, "Max ingestion rate (samples/sec) that this distributor will accept. This limit is per-distributor, not per-tenant. Additional push requests will be rejected. Current ingestion rate is computed as exponentially weighted moving average, updated every second. 0 = unlimited.")
	f.IntVar(&cfg.InstanceLimits.MaxInflightPushRequests, "distributor.instance-limits.max-inflight-push-requests", 0, "Max inflight push requests that this distributor can handle. This limit is per-distributor, not per-tenant. Additional requests will be rejected. 0 = unlimited.")
}

// Validate config and returns error on failure
func (cfg *Config) Validate(limits validation.Limits) error {
	if !util.StringsContain(supportedShardingStrategies, cfg.ShardingStrategy) {
		return errInvalidShardingStrategy
	}

	if cfg.ShardingStrategy == util.ShardingStrategyShuffle && limits.IngestionTenantShardSize <= 0 {
		return errInvalidTenantShardSize
	}

	return cfg.HATrackerConfig.Validate()
}

const (
	instanceLimitsMetric     = "cortex_distributor_instance_limits"
	instanceLimitsMetricHelp = "Instance limits used by this distributor." // Must be same for all registrations.
	limitLabel               = "limit"
)

// New constructs a new Distributor
func New(cfg Config, clientConfig ingester_client.Config, limits *validation.Overrides, ingestersRing ring.ReadRing, canJoinDistributorsRing bool, reg prometheus.Registerer, log log.Logger) (*Distributor, error) {
	if cfg.IngesterClientFactory == nil {
		cfg.IngesterClientFactory = func(addr string) (ring_client.PoolClient, error) {
			return ingester_client.MakeIngesterClient(addr, clientConfig)
		}
	}

	cfg.PoolConfig.RemoteTimeout = cfg.RemoteTimeout

	haTracker, err := newHATracker(cfg.HATrackerConfig, limits, reg, log)
	if err != nil {
		return nil, err
	}

	subservices := []services.Service(nil)
	subservices = append(subservices, haTracker)

	// Create the configured ingestion rate limit strategy (local or global). In case
	// it's an internal dependency and can't join the distributors ring, we skip rate
	// limiting.
	var ingestionRateStrategy limiter.RateLimiterStrategy
	var distributorsLifeCycler *ring.Lifecycler
	var distributorsRing *ring.Ring

	if !canJoinDistributorsRing {
		ingestionRateStrategy = newInfiniteIngestionRateStrategy()
	} else if limits.IngestionRateStrategy() == validation.GlobalIngestionRateStrategy {
		distributorsLifeCycler, err = ring.NewLifecycler(cfg.DistributorRing.ToLifecyclerConfig(), nil, "distributor", ring.DistributorRingKey, true, log, prometheus.WrapRegistererWithPrefix("cortex_", reg))
		if err != nil {
			return nil, err
		}

		distributorsRing, err = ring.New(cfg.DistributorRing.ToRingConfig(), "distributor", ring.DistributorRingKey, log, prometheus.WrapRegistererWithPrefix("cortex_", reg))
		if err != nil {
			return nil, errors.Wrap(err, "failed to initialize distributors' ring client")
		}
		subservices = append(subservices, distributorsLifeCycler, distributorsRing)

		ingestionRateStrategy = newGlobalIngestionRateStrategy(limits, distributorsLifeCycler)
	} else {
		ingestionRateStrategy = newLocalIngestionRateStrategy(limits)
	}

	d := &Distributor{
		cfg:                    cfg,
		log:                    log,
		ingestersRing:          ingestersRing,
		ingesterPool:           NewPool(cfg.PoolConfig, ingestersRing, cfg.IngesterClientFactory, log),
		distributorsLifeCycler: distributorsLifeCycler,
		distributorsRing:       distributorsRing,
		limits:                 limits,
		ingestionRateLimiter:   limiter.NewRateLimiter(ingestionRateStrategy, 10*time.Second),
		HATracker:              haTracker,
		ingestionRate:          util_math.NewEWMARate(0.2, instanceIngestionRateTickInterval),

		queryDuration: instrument.NewHistogramCollector(promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "cortex",
			Name:      "distributor_query_duration_seconds",
			Help:      "Time spent executing expression and exemplar queries.",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 30},
		}, []string{"method", "status_code"})),
		receivedSamples: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_received_samples_total",
			Help:      "The total number of received samples, excluding rejected and deduped samples.",
		}, []string{"user"}),
		receivedExemplars: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_received_exemplars_total",
			Help:      "The total number of received exemplars, excluding rejected and deduped exemplars.",
		}, []string{"user"}),
		receivedMetadata: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_received_metadata_total",
			Help:      "The total number of received metadata, excluding rejected.",
		}, []string{"user"}),
		incomingSamples: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_samples_in_total",
			Help:      "The total number of samples that have come in to the distributor, including rejected or deduped samples.",
		}, []string{"user"}),
		incomingExemplars: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_exemplars_in_total",
			Help:      "The total number of exemplars that have come in to the distributor, including rejected or deduped exemplars.",
		}, []string{"user"}),
		incomingMetadata: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_metadata_in_total",
			Help:      "The total number of metadata the have come in to the distributor, including rejected.",
		}, []string{"user"}),
		nonHASamples: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_non_ha_samples_received_total",
			Help:      "The total number of received samples for a user that has HA tracking turned on, but the sample didn't contain both HA labels.",
		}, []string{"user"}),
		dedupedSamples: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_deduped_samples_total",
			Help:      "The total number of deduplicated samples.",
		}, []string{"user", "cluster"}),
		labelsHistogram: promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
			Namespace: "cortex",
			Name:      "labels_per_sample",
			Help:      "Number of labels per sample.",
			Buckets:   []float64{5, 10, 15, 20, 25},
		}),
		ingesterAppends: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_ingester_appends_total",
			Help:      "The total number of batch appends sent to ingesters.",
		}, []string{"ingester", "type"}),
		ingesterAppendFailures: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_ingester_append_failures_total",
			Help:      "The total number of failed batch appends sent to ingesters.",
		}, []string{"ingester", "type"}),
		ingesterQueries: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_ingester_queries_total",
			Help:      "The total number of queries sent to ingesters.",
		}, []string{"ingester"}),
		ingesterQueryFailures: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "distributor_ingester_query_failures_total",
			Help:      "The total number of failed queries sent to ingesters.",
		}, []string{"ingester"}),
		replicationFactor: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Namespace: "cortex",
			Name:      "distributor_replication_factor",
			Help:      "The configured replication factor.",
		}),
		latestSeenSampleTimestampPerUser: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Name: "cortex_distributor_latest_seen_sample_timestamp_seconds",
			Help: "Unix timestamp of latest received sample per user.",
		}, []string{"user"}),
	}

	promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Name:        instanceLimitsMetric,
		Help:        instanceLimitsMetricHelp,
		ConstLabels: map[string]string{limitLabel: "max_inflight_push_requests"},
	}).Set(float64(cfg.InstanceLimits.MaxInflightPushRequests))
	promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Name:        instanceLimitsMetric,
		Help:        instanceLimitsMetricHelp,
		ConstLabels: map[string]string{limitLabel: "max_ingestion_rate"},
	}).Set(cfg.InstanceLimits.MaxIngestionRate)

	promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cortex_distributor_inflight_push_requests",
		Help: "Current number of inflight push requests in distributor.",
	}, func() float64 {
		return float64(d.inflightPushRequests.Load())
	})
	promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cortex_distributor_ingestion_rate_samples_per_second",
		Help: "Current ingestion rate in samples/sec that distributor is using to limit access.",
	}, func() float64 {
		return d.ingestionRate.Rate()
	})

	d.replicationFactor.Set(float64(ingestersRing.ReplicationFactor()))
	d.activeUsers = util.NewActiveUsersCleanupWithDefaultValues(d.cleanupInactiveUser)

	subservices = append(subservices, d.ingesterPool, d.activeUsers)
	d.subservices, err = services.NewManager(subservices...)
	if err != nil {
		return nil, err
	}
	d.subservicesWatcher = services.NewFailureWatcher()
	d.subservicesWatcher.WatchManager(d.subservices)

	d.Service = services.NewBasicService(d.starting, d.running, d.stopping)
	return d, nil
}

func (d *Distributor) starting(ctx context.Context) error {
	if d.cfg.InstanceLimits != (InstanceLimits{}) {
		util_log.WarnExperimentalUse("distributor instance limits")
	}

	// Only report success if all sub-services start properly
	return services.StartManagerAndAwaitHealthy(ctx, d.subservices)
}

func (d *Distributor) running(ctx context.Context) error {
	ingestionRateTicker := time.NewTicker(instanceIngestionRateTickInterval)
	defer ingestionRateTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ingestionRateTicker.C:
			d.ingestionRate.Tick()

		case err := <-d.subservicesWatcher.Chan():
			return errors.Wrap(err, "distributor subservice failed")
		}
	}
}

func (d *Distributor) cleanupInactiveUser(userID string) {
	d.ingestersRing.CleanupShuffleShardCache(userID)

	d.HATracker.cleanupHATrackerMetricsForUser(userID)

	d.receivedSamples.DeleteLabelValues(userID)
	d.receivedExemplars.DeleteLabelValues(userID)
	d.receivedMetadata.DeleteLabelValues(userID)
	d.incomingSamples.DeleteLabelValues(userID)
	d.incomingExemplars.DeleteLabelValues(userID)
	d.incomingMetadata.DeleteLabelValues(userID)
	d.nonHASamples.DeleteLabelValues(userID)
	d.latestSeenSampleTimestampPerUser.DeleteLabelValues(userID)

	if err := util.DeleteMatchingLabels(d.dedupedSamples, map[string]string{"user": userID}); err != nil {
		level.Warn(d.log).Log("msg", "failed to remove cortex_distributor_deduped_samples_total metric for user", "user", userID, "err", err)
	}

	validation.DeletePerUserValidationMetrics(userID, d.log)
}

// Called after distributor is asked to stop via StopAsync.
func (d *Distributor) stopping(_ error) error {
	return services.StopManagerAndAwaitStopped(context.Background(), d.subservices)
}

func (d *Distributor) tokenForLabels(userID string, labels []mimirpb.LabelAdapter) (uint32, error) {
	if d.cfg.ShardByAllLabels {
		return shardByAllLabels(userID, labels), nil
	}

	unsafeMetricName, err := extract.UnsafeMetricNameFromLabelAdapters(labels)
	if err != nil {
		return 0, err
	}
	return shardByMetricName(userID, unsafeMetricName), nil
}

func (d *Distributor) tokenForMetadata(userID string, metricName string) uint32 {
	if d.cfg.ShardByAllLabels {
		return shardByMetricName(userID, metricName)
	}

	return shardByUser(userID)
}

// shardByMetricName returns the token for the given metric. The provided metricName
// is guaranteed to not be retained.
func shardByMetricName(userID string, metricName string) uint32 {
	h := shardByUser(userID)
	h = ingester_client.HashAdd32(h, metricName)
	return h
}

func shardByUser(userID string) uint32 {
	h := ingester_client.HashNew32()
	h = ingester_client.HashAdd32(h, userID)
	return h
}

// This function generates different values for different order of same labels.
func shardByAllLabels(userID string, labels []mimirpb.LabelAdapter) uint32 {
	h := shardByUser(userID)
	for _, label := range labels {
		h = ingester_client.HashAdd32(h, label.Name)
		h = ingester_client.HashAdd32(h, label.Value)
	}
	return h
}

// Remove the label labelname from a slice of LabelPairs if it exists.
func removeLabel(labelName string, labels *[]mimirpb.LabelAdapter) {
	for i := 0; i < len(*labels); i++ {
		pair := (*labels)[i]
		if pair.Name == labelName {
			*labels = append((*labels)[:i], (*labels)[i+1:]...)
			return
		}
	}
}

// Returns a boolean that indicates whether or not we want to remove the replica label going forward,
// and an error that indicates whether we want to accept samples based on the cluster/replica found in ts.
// nil for the error means accept the sample.
func (d *Distributor) checkSample(ctx context.Context, userID, cluster, replica string) (removeReplicaLabel bool, _ error) {
	// If the sample doesn't have either HA label, accept it.
	// At the moment we want to accept these samples by default.
	if cluster == "" || replica == "" {
		return false, nil
	}

	// If replica label is too long, don't use it. We accept the sample here, but it will fail validation later anyway.
	if len(replica) > d.limits.MaxLabelValueLength(userID) {
		return false, nil
	}

	// At this point we know we have both HA labels, we should lookup
	// the cluster/instance here to see if we want to accept this sample.
	err := d.HATracker.checkReplica(ctx, userID, cluster, replica, time.Now())
	// checkReplica should only have returned an error if there was a real error talking to Consul, or if the replica labels don't match.
	if err != nil { // Don't accept the sample.
		return false, err
	}
	return true, nil
}

// Validates a single series from a write request.
// The returned error may retain the series labels.
func (d *Distributor) validateSeries(ts mimirpb.PreallocTimeseries, userID string, skipLabelNameValidation bool) error {
	if err := validation.ValidateLabels(d.limits, userID, ts.Labels, skipLabelNameValidation); err != nil {
		return err
	}

	for _, s := range ts.Samples {
		if err := validation.ValidateSample(d.limits, userID, ts.Labels, s); err != nil {
			return err
		}
	}

	for _, e := range ts.Exemplars {
		if err := validation.ValidateExemplar(userID, ts.Labels, e); err != nil {
			// An exemplar validation error prevents ingesting samples
			// in the same series object. However because the current Prometheus
			// remote write implementation only populates one or the other,
			// there never will be any.
			return err
		}
	}

	return nil
}

// Push implements client.IngesterServer
func (d *Distributor) Push(ctx context.Context, req *mimirpb.WriteRequest) (*mimirpb.WriteResponse, error) {
	return d.PushWithCleanup(ctx, req, func() { mimirpb.ReuseSlice(req.Timeseries) })
}

// PushWithCleanup takes a WriteRequest and distributes it to ingesters using the ring.
// Strings in `req` may be pointers into the gRPC buffer which will be reused, so must be copied if retained.
func (d *Distributor) PushWithCleanup(ctx context.Context, req *mimirpb.WriteRequest, callerCleanup func()) (*mimirpb.WriteResponse, error) {
	// We will report *this* request in the error too.
	inflight := d.inflightPushRequests.Inc()

	// Decrement counter after all ingester calls have finished or been cancelled.
	cleanup := func() {
		callerCleanup()
		d.inflightPushRequests.Dec()
	}
	cleanupInDefer := true
	defer func() {
		if cleanupInDefer {
			cleanup()
		}
	}()

	userID, err := tenant.TenantID(ctx)
	if err != nil {
		return nil, err
	}
	span := opentracing.SpanFromContext(ctx)
	if span != nil {
		span.SetTag("organization", userID)
	}

	if d.cfg.InstanceLimits.MaxInflightPushRequests > 0 && inflight > int64(d.cfg.InstanceLimits.MaxInflightPushRequests) {
		return nil, errTooManyInflightPushRequests
	}

	if d.cfg.InstanceLimits.MaxIngestionRate > 0 {
		if rate := d.ingestionRate.Rate(); rate >= d.cfg.InstanceLimits.MaxIngestionRate {
			return nil, errMaxSamplesPushRateLimitReached
		}
	}

	now := time.Now()
	d.activeUsers.UpdateUserTimestamp(userID, now)

	source := util.GetSourceIPsFromOutgoingCtx(ctx)

	var firstPartialErr error
	removeReplica := false

	numSamples := 0
	numExemplars := 0
	for _, ts := range req.Timeseries {
		numSamples += len(ts.Samples)
		numExemplars += len(ts.Exemplars)
	}
	// Count the total samples in, prior to validation or deduplication, for comparison with other metrics.
	d.incomingSamples.WithLabelValues(userID).Add(float64(numSamples))
	d.incomingExemplars.WithLabelValues(userID).Add(float64(numExemplars))
	// Count the total number of metadata in.
	d.incomingMetadata.WithLabelValues(userID).Add(float64(len(req.Metadata)))

	// A WriteRequest can only contain series or metadata but not both. This might change in the future.
	// For each timeseries or samples, we compute a hash to distribute across ingesters;
	// check each sample/metadata and discard if outside limits.
	validatedTimeseries := make([]mimirpb.PreallocTimeseries, 0, len(req.Timeseries))
	validatedMetadata := make([]*mimirpb.MetricMetadata, 0, len(req.Metadata))
	metadataKeys := make([]uint32, 0, len(req.Metadata))
	seriesKeys := make([]uint32, 0, len(req.Timeseries))
	validatedSamples := 0
	validatedExemplars := 0

	if d.limits.AcceptHASamples(userID) && len(req.Timeseries) > 0 {
		cluster, replica := findHALabels(d.limits.HAReplicaLabel(userID), d.limits.HAClusterLabel(userID), req.Timeseries[0].Labels)
		// Make a copy of these, since they may be retained as labels on our metrics, e.g. dedupedSamples.
		cluster, replica = copyString(cluster), copyString(replica)
		if span != nil {
			span.SetTag("cluster", cluster)
			span.SetTag("replica", replica)
		}
		removeReplica, err = d.checkSample(ctx, userID, cluster, replica)
		if err != nil {
			if errors.Is(err, replicasNotMatchError{}) {
				// These samples have been deduped.
				d.dedupedSamples.WithLabelValues(userID, cluster).Add(float64(numSamples))
				return nil, httpgrpc.Errorf(http.StatusAccepted, "%s", err)
			}

			if errors.Is(err, tooManyClustersError{}) {
				validation.DiscardedSamples.WithLabelValues(validation.TooManyHAClusters, userID).Add(float64(numSamples))
				return nil, httpgrpc.Errorf(http.StatusBadRequest, "%s", err)
			}

			return nil, err
		}
		// If there wasn't an error but removeReplica is false that means we didn't find both HA labels.
		if !removeReplica {
			d.nonHASamples.WithLabelValues(userID).Add(float64(numSamples))
		}
	}

	latestSampleTimestampMs := int64(0)
	defer func() {
		// Update this metric even in case of errors.
		if latestSampleTimestampMs > 0 {
			d.latestSeenSampleTimestampPerUser.WithLabelValues(userID).Set(float64(latestSampleTimestampMs) / 1000)
		}
	}()

	// For each timeseries, compute a hash to distribute across ingesters;
	// check each sample and discard if outside limits.
	for _, ts := range req.Timeseries {
		// Use timestamp of latest sample in the series. If samples for series are not ordered, metric for user may be wrong.
		if len(ts.Samples) > 0 {
			latestSampleTimestampMs = util_math.Max64(latestSampleTimestampMs, ts.Samples[len(ts.Samples)-1].TimestampMs)
		}

		if mrc := d.limits.MetricRelabelConfigs(userID); len(mrc) > 0 {
			l := relabel.Process(mimirpb.FromLabelAdaptersToLabels(ts.Labels), mrc...)
			ts.Labels = mimirpb.FromLabelsToLabelAdapters(l)
		}

		// If we found both the cluster and replica labels, we only want to include the cluster label when
		// storing series in Mimir. If we kept the replica label we would end up with another series for the same
		// series we're trying to dedupe when HA tracking moves over to a different replica.
		if removeReplica {
			removeLabel(d.limits.HAReplicaLabel(userID), &ts.Labels)
		}

		for _, labelName := range d.limits.DropLabels(userID) {
			removeLabel(labelName, &ts.Labels)
		}

		if len(ts.Labels) == 0 {
			continue
		}

		// We rely on sorted labels in different places:
		// 1) When computing token for labels, and sharding by all labels. Here different order of labels returns
		// different tokens, which is bad.
		// 2) In validation code, when checking for duplicate label names. As duplicate label names are rejected
		// later in the validation phase, we ignore them here.
		sortLabelsIfNeeded(ts.Labels)

		// Generate the sharding token based on the series labels without the HA replica
		// label and dropped labels (if any)
		key, err := d.tokenForLabels(userID, ts.Labels)
		if err != nil {
			return nil, err
		}

		d.labelsHistogram.Observe(float64(len(ts.Labels)))

		skipLabelNameValidation := d.cfg.SkipLabelNameValidation || req.GetSkipLabelNameValidation()
		validationErr := d.validateSeries(ts, userID, skipLabelNameValidation)

		// Errors in validation are considered non-fatal, as one series in a request may contain
		// invalid data but all the remaining series could be perfectly valid.
		if validationErr != nil {
			if firstPartialErr == nil {
				// The series labels may be retained by validationErr but that's not a problem for this
				// use case because we format it calling Error() and then we discard it.
				firstPartialErr = httpgrpc.Errorf(http.StatusBadRequest, validationErr.Error())
			}
			continue
		}

		seriesKeys = append(seriesKeys, key)
		validatedTimeseries = append(validatedTimeseries, ts)
		validatedSamples += len(ts.Samples)
		validatedExemplars += len(ts.Exemplars)
	}

	for _, m := range req.Metadata {
		err := validation.ValidateMetadata(d.limits, userID, m)
		if err != nil {
			if firstPartialErr == nil {
				firstPartialErr = err
			}

			continue
		}

		metadataKeys = append(metadataKeys, d.tokenForMetadata(userID, m.MetricFamilyName))
		validatedMetadata = append(validatedMetadata, m)
	}

	d.receivedSamples.WithLabelValues(userID).Add(float64(validatedSamples))
	d.receivedExemplars.WithLabelValues(userID).Add((float64(validatedExemplars)))
	d.receivedMetadata.WithLabelValues(userID).Add(float64(len(validatedMetadata)))

	if len(seriesKeys) == 0 && len(metadataKeys) == 0 {
		return &mimirpb.WriteResponse{}, firstPartialErr
	}

	totalN := validatedSamples + validatedExemplars + len(validatedMetadata)
	if !d.ingestionRateLimiter.AllowN(now, userID, totalN) {
		validation.DiscardedSamples.WithLabelValues(validation.RateLimited, userID).Add(float64(validatedSamples))
		validation.DiscardedExemplars.WithLabelValues(validation.RateLimited, userID).Add(float64(validatedExemplars))
		validation.DiscardedMetadata.WithLabelValues(validation.RateLimited, userID).Add(float64(len(validatedMetadata)))
		// Return a 429 here to tell the client it is going too fast.
		// Client may discard the data or slow down and re-send.
		// Prometheus v2.26 added a remote-write option 'retry_on_http_429'.
		return nil, httpgrpc.Errorf(http.StatusTooManyRequests, "ingestion rate limit (%v) exceeded while adding %d samples and %d metadata", d.ingestionRateLimiter.Limit(now, userID), validatedSamples, len(validatedMetadata))
	}

	// totalN included samples and metadata. Ingester follows this pattern when computing its ingestion rate.
	d.ingestionRate.Add(int64(totalN))

	subRing := d.ingestersRing

	// Obtain a subring if required.
	if d.cfg.ShardingStrategy == util.ShardingStrategyShuffle {
		subRing = d.ingestersRing.ShuffleShard(userID, d.limits.IngestionTenantShardSize(userID))
	}

	// Use a background context to make sure all ingesters get samples even if we return early
	localCtx, cancel := context.WithTimeout(context.Background(), d.cfg.RemoteTimeout)
	localCtx = user.InjectOrgID(localCtx, userID)
	// Get clientIP(s) from Context and add it to localCtx
	localCtx = util.AddSourceIPsToOutgoingContext(localCtx, source)
	if sp := opentracing.SpanFromContext(ctx); sp != nil {
		localCtx = opentracing.ContextWithSpan(localCtx, sp)
	}

	keys := append(seriesKeys, metadataKeys...)
	initialMetadataIndex := len(seriesKeys)

	op := ring.WriteNoExtend
	if d.cfg.ExtendWrites {
		op = ring.Write
	}

	// we must not re-use buffers now until all DoBatch goroutines have finished,
	// so set this flag false and pass cleanup() to DoBatch.
	cleanupInDefer = false

	err = ring.DoBatch(ctx, op, subRing, keys, func(ingester ring.InstanceDesc, indexes []int) error {
		timeseries := make([]mimirpb.PreallocTimeseries, 0, len(indexes))
		var metadata []*mimirpb.MetricMetadata

		for _, i := range indexes {
			if i >= initialMetadataIndex {
				metadata = append(metadata, validatedMetadata[i-initialMetadataIndex])
			} else {
				timeseries = append(timeseries, validatedTimeseries[i])
			}
		}

		return d.send(localCtx, ingester, timeseries, metadata, req.Source)
	}, func() { cleanup(); cancel() })
	if err != nil {
		return nil, err
	}
	return &mimirpb.WriteResponse{}, firstPartialErr
}

func copyString(s string) string {
	return string([]byte(s))
}

func sortLabelsIfNeeded(labels []mimirpb.LabelAdapter) {
	// no need to run sort.Slice, if labels are already sorted, which is most of the time.
	// we can avoid extra memory allocations (mostly interface-related) this way.
	sorted := true
	last := ""
	for _, l := range labels {
		if last > l.Name {
			sorted = false
			break
		}
		last = l.Name
	}

	if sorted {
		return
	}

	sort.Slice(labels, func(i, j int) bool {
		return labels[i].Name < labels[j].Name
	})
}

func (d *Distributor) send(ctx context.Context, ingester ring.InstanceDesc, timeseries []mimirpb.PreallocTimeseries, metadata []*mimirpb.MetricMetadata, source mimirpb.WriteRequest_SourceEnum) error {
	h, err := d.ingesterPool.GetClientFor(ingester.Addr)
	if err != nil {
		return err
	}
	c := h.(ingester_client.IngesterClient)

	req := mimirpb.WriteRequest{
		Timeseries: timeseries,
		Metadata:   metadata,
		Source:     source,
	}
	_, err = c.Push(ctx, &req)

	if len(metadata) > 0 {
		d.ingesterAppends.WithLabelValues(ingester.Addr, typeMetadata).Inc()
		if err != nil {
			d.ingesterAppendFailures.WithLabelValues(ingester.Addr, typeMetadata).Inc()
		}
	}
	if len(timeseries) > 0 {
		d.ingesterAppends.WithLabelValues(ingester.Addr, typeSamples).Inc()
		if err != nil {
			d.ingesterAppendFailures.WithLabelValues(ingester.Addr, typeSamples).Inc()
		}
	}

	return err
}

// ForReplicationSet runs f, in parallel, for all ingesters in the input replication set.
func (d *Distributor) ForReplicationSet(ctx context.Context, replicationSet ring.ReplicationSet, f func(context.Context, ingester_client.IngesterClient) (interface{}, error)) ([]interface{}, error) {
	return replicationSet.Do(ctx, d.cfg.ExtraQueryDelay, func(ctx context.Context, ing *ring.InstanceDesc) (interface{}, error) {
		client, err := d.ingesterPool.GetClientFor(ing.Addr)
		if err != nil {
			return nil, err
		}

		return f(ctx, client.(ingester_client.IngesterClient))
	})
}

// LabelValuesForLabelName returns all of the label values that are associated with a given label name.
func (d *Distributor) LabelValuesForLabelName(ctx context.Context, from, to model.Time, labelName model.LabelName, matchers ...*labels.Matcher) ([]string, error) {
	replicationSet, err := d.GetIngestersForMetadata(ctx)
	if err != nil {
		return nil, err
	}

	req, err := ingester_client.ToLabelValuesRequest(labelName, from, to, matchers)
	if err != nil {
		return nil, err
	}

	resps, err := d.ForReplicationSet(ctx, replicationSet, func(ctx context.Context, client ingester_client.IngesterClient) (interface{}, error) {
		return client.LabelValues(ctx, req)
	})
	if err != nil {
		return nil, err
	}

	valueSet := map[string]struct{}{}
	for _, resp := range resps {
		for _, v := range resp.(*ingester_client.LabelValuesResponse).LabelValues {
			valueSet[v] = struct{}{}
		}
	}

	values := make([]string, 0, len(valueSet))
	for v := range valueSet {
		values = append(values, v)
	}

	// We need the values returned to be sorted.
	sort.Strings(values)

	return values, nil
}

// LabelNamesAndValues query ingesters for label names and values and returns labels with distinct list of values.
func (d *Distributor) LabelNamesAndValues(ctx context.Context, matchers []*labels.Matcher) (*ingester_client.LabelNamesAndValuesResponse, error) {
	replicationSet, err := d.GetIngestersForMetadata(ctx)
	if err != nil {
		return nil, err
	}

	req, err := toLabelNamesCardinalityRequest(matchers)
	if err != nil {
		return nil, err
	}
	userID, err := tenant.TenantID(ctx)
	if err != nil {
		return nil, err
	}
	sizeLimitBytes := d.limits.LabelNamesAndValuesResultsMaxSizeBytes(userID)
	merger := &labelNamesAndValuesResponseMerger{result: map[string]map[string]struct{}{}, sizeLimitBytes: sizeLimitBytes}
	_, err = d.ForReplicationSet(ctx, replicationSet, func(ctx context.Context, client ingester_client.IngesterClient) (interface{}, error) {
		stream, err := client.LabelNamesAndValues(ctx, req)
		if err != nil {
			return nil, err
		}
		defer stream.CloseSend() //nolint:errcheck
		return nil, merger.collectResponses(stream)
	})
	if err != nil {
		return nil, err
	}
	return merger.toLabelNamesAndValuesResponses(), nil
}

type labelNamesAndValuesResponseMerger struct {
	lock             sync.Mutex
	result           map[string]map[string]struct{}
	sizeLimitBytes   int
	currentSizeBytes int
}

func toLabelNamesCardinalityRequest(matchers []*labels.Matcher) (*ingester_client.LabelNamesAndValuesRequest, error) {
	matchersProto, err := ingester_client.ToLabelMatchers(matchers)
	if err != nil {
		return nil, err
	}
	return &ingester_client.LabelNamesAndValuesRequest{Matchers: matchersProto}, nil
}

// toLabelNamesAndValuesResponses converts map with distinct label values to `ingester_client.LabelNamesAndValuesResponse`.
func (m *labelNamesAndValuesResponseMerger) toLabelNamesAndValuesResponses() *ingester_client.LabelNamesAndValuesResponse {
	responses := make([]*ingester_client.LabelValues, 0, len(m.result))
	for name, values := range m.result {
		labelValues := make([]string, 0, len(values))
		for val := range values {
			labelValues = append(labelValues, val)
		}
		responses = append(responses, &ingester_client.LabelValues{
			LabelName: name,
			Values:    labelValues,
		})
	}
	return &ingester_client.LabelNamesAndValuesResponse{Items: responses}
}

// collectResponses listens for the stream and once the message is received, puts labels and values to the map with distinct label values.
func (m *labelNamesAndValuesResponseMerger) collectResponses(stream ingester_client.Ingester_LabelNamesAndValuesClient) error {
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		err = m.putItemsToMap(message)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *labelNamesAndValuesResponseMerger) putItemsToMap(message *ingester_client.LabelNamesAndValuesResponse) error {
	m.lock.Lock()
	defer m.lock.Unlock()
	for _, item := range message.Items {
		values, exists := m.result[item.LabelName]
		if !exists {
			m.currentSizeBytes += len(item.LabelName)
			values = make(map[string]struct{}, len(item.Values))
			m.result[item.LabelName] = values
		}
		for _, val := range item.Values {
			if _, valueExists := values[val]; !valueExists {
				m.currentSizeBytes += len(val)
				if m.currentSizeBytes > m.sizeLimitBytes {
					return fmt.Errorf("size of distinct label names and values is greater than %v bytes", m.sizeLimitBytes)
				}
				values[val] = struct{}{}
			}
		}
	}
	return nil
}

// LabelValuesCardinality performs the following two operations in parallel:
//  * queries ingesters for label values cardinality of a set of labelNames
//  * queries ingesters for user stats to get the ingester's series head count
func (d *Distributor) LabelValuesCardinality(ctx context.Context, labelNames []model.LabelName, matchers []*labels.Matcher) (uint64, *ingester_client.LabelValuesCardinalityResponse, error) {
	var totalSeries uint64
	var labelValuesCardinalityResponse *ingester_client.LabelValuesCardinalityResponse

	userID, err := tenant.TenantID(ctx)
	if err != nil {
		return 0, nil, err
	}

	lbNamesLimit := d.limits.LabelValuesMaxCardinalityLabelNamesPerRequest(userID)
	if len(labelNames) > lbNamesLimit {
		return 0, nil, httpgrpc.Errorf(http.StatusBadRequest, "label values cardinality request label names limit (limit: %d actual: %d) exceeded", lbNamesLimit, len(labelNames))
	}

	// Run labelValuesCardinality and UserStats methods in parallel
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		response, err := d.labelValuesCardinality(ctx, labelNames, matchers)
		if err == nil {
			labelValuesCardinalityResponse = response
		}
		return err
	})
	group.Go(func() error {
		response, err := d.UserStats(ctx)
		if err == nil {
			totalSeries = response.NumSeries
		}
		return err
	})
	if err := group.Wait(); err != nil {
		return 0, nil, err
	}
	return totalSeries, labelValuesCardinalityResponse, nil
}

// labelValuesCardinality queries ingesters for label values cardinality of a set of labelNames
// Returns a LabelValuesCardinalityResponse where each item contains an exclusive label name and associated label values
func (d *Distributor) labelValuesCardinality(ctx context.Context, labelNames []model.LabelName, matchers []*labels.Matcher) (*ingester_client.LabelValuesCardinalityResponse, error) {
	replicationSet, err := d.GetIngestersForMetadata(ctx)
	if err != nil {
		return nil, err
	}

	// Make sure we get a successful response from all the ingesters
	replicationSet.MaxErrors = 0

	cardinalityConcurrentMap := &labelValuesCardinalityConcurrentMap{
		cardinalityMap: map[string]map[string]uint64{},
	}

	labelValuesReq, err := toLabelValuesCardinalityRequest(labelNames, matchers)
	if err != nil {
		return nil, err
	}

	_, err = d.ForReplicationSet(ctx, replicationSet, func(ctx context.Context, client ingester_client.IngesterClient) (interface{}, error) {
		stream, err := client.LabelValuesCardinality(ctx, labelValuesReq)
		if err != nil {
			return nil, err
		}
		defer func() { _ = stream.CloseSend() }()

		return nil, cardinalityConcurrentMap.processLabelValuesCardinalityMessages(stream)
	})
	if err != nil {
		return nil, err
	}

	cardinalityItems := make([]*ingester_client.LabelValueSeriesCount, 0, len(labelNames))

	// Adjust label values' series count based on the ingester's replication factor
	for labelName, labelValueSeriesCountMap := range cardinalityConcurrentMap.cardinalityMap {
		for labelValue, seriesCount := range labelValueSeriesCountMap {
			labelValueSeriesCountMap[labelValue] = seriesCount / uint64(d.ingestersRing.ReplicationFactor())
		}
		cardinalityItems = append(cardinalityItems, &ingester_client.LabelValueSeriesCount{
			LabelName:        labelName,
			LabelValueSeries: labelValueSeriesCountMap,
		})
	}

	cardinalityResponse := &ingester_client.LabelValuesCardinalityResponse{
		Items: cardinalityItems,
	}

	return cardinalityResponse, nil
}

func toLabelValuesCardinalityRequest(labelNames []model.LabelName, matchers []*labels.Matcher) (*ingester_client.LabelValuesCardinalityRequest, error) {
	matchersProto, err := ingester_client.ToLabelMatchers(matchers)
	if err != nil {
		return nil, err
	}
	labelNamesStr := make([]string, 0, len(labelNames))
	for _, labelName := range labelNames {
		labelNamesStr = append(labelNamesStr, string(labelName))
	}
	return &ingester_client.LabelValuesCardinalityRequest{LabelNames: labelNamesStr, Matchers: matchersProto}, nil
}

type labelValuesCardinalityConcurrentMap struct {
	cardinalityMap map[string]map[string]uint64
	lock           sync.Mutex
}

func (cm *labelValuesCardinalityConcurrentMap) processLabelValuesCardinalityMessages(
	stream ingester_client.Ingester_LabelValuesCardinalityClient) error {
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		cm.processLabelValuesCardinalityMessage(message)
	}
	return nil
}

/*
 * Build a map from all the responses received from all the ingesters.
 * Each label name will represent a key on the cardinalityMap which will have as value a second map, containing
 * as key the label_value and value the respective series_count. This series_count will represent the cumulative result
 * of all (label_name, label_value) tuples from all ingesters.
 *
 * Map: (label_name -> (label_value -> series_count))
 *
 * This method is called per each LabelValuesCardinalityResponse consumed from each ingester
 */
func (cm *labelValuesCardinalityConcurrentMap) processLabelValuesCardinalityMessage(
	message *ingester_client.LabelValuesCardinalityResponse) {

	cm.lock.Lock()
	defer cm.lock.Unlock()

	for _, item := range message.Items {
		if _, exists := cm.cardinalityMap[item.LabelName]; !exists {
			// Label name nonexistent
			cm.cardinalityMap[item.LabelName] = map[string]uint64{}
		}
		for labelValue, seriesCount := range item.LabelValueSeries {
			// Label name existent
			cm.cardinalityMap[item.LabelName][labelValue] += seriesCount
		}
	}
}

// LabelNames returns all of the label names.
func (d *Distributor) LabelNames(ctx context.Context, from, to model.Time, matchers ...*labels.Matcher) ([]string, error) {
	replicationSet, err := d.GetIngestersForMetadata(ctx)
	if err != nil {
		return nil, err
	}

	req, err := ingester_client.ToLabelNamesRequest(from, to, matchers)
	if err != nil {
		return nil, err
	}

	resps, err := d.ForReplicationSet(ctx, replicationSet, func(ctx context.Context, client ingester_client.IngesterClient) (interface{}, error) {
		return client.LabelNames(ctx, req)
	})
	if err != nil {
		return nil, err
	}

	valueSet := map[string]struct{}{}
	for _, resp := range resps {
		for _, v := range resp.(*ingester_client.LabelNamesResponse).LabelNames {
			valueSet[v] = struct{}{}
		}
	}

	values := make([]string, 0, len(valueSet))
	for v := range valueSet {
		values = append(values, v)
	}

	sort.Strings(values)

	return values, nil
}

// MetricsForLabelMatchers gets the metrics that match said matchers
func (d *Distributor) MetricsForLabelMatchers(ctx context.Context, from, through model.Time, matchers ...*labels.Matcher) ([]metric.Metric, error) {
	replicationSet, err := d.GetIngestersForMetadata(ctx)
	if err != nil {
		return nil, err
	}

	req, err := ingester_client.ToMetricsForLabelMatchersRequest(from, through, matchers)
	if err != nil {
		return nil, err
	}

	resps, err := d.ForReplicationSet(ctx, replicationSet, func(ctx context.Context, client ingester_client.IngesterClient) (interface{}, error) {
		return client.MetricsForLabelMatchers(ctx, req)
	})
	if err != nil {
		return nil, err
	}

	metrics := map[model.Fingerprint]model.Metric{}
	for _, resp := range resps {
		ms := ingester_client.FromMetricsForLabelMatchersResponse(resp.(*ingester_client.MetricsForLabelMatchersResponse))
		for _, m := range ms {
			metrics[m.Fingerprint()] = m
		}
	}

	result := make([]metric.Metric, 0, len(metrics))
	for _, m := range metrics {
		result = append(result, metric.Metric{
			Metric: m,
		})
	}
	return result, nil
}

// MetricsMetadata returns all metric metadata of a user.
func (d *Distributor) MetricsMetadata(ctx context.Context) ([]scrape.MetricMetadata, error) {
	replicationSet, err := d.GetIngestersForMetadata(ctx)
	if err != nil {
		return nil, err
	}

	req := &ingester_client.MetricsMetadataRequest{}
	// TODO(gotjosh): We only need to look in all the ingesters if shardByAllLabels is enabled.
	resps, err := d.ForReplicationSet(ctx, replicationSet, func(ctx context.Context, client ingester_client.IngesterClient) (interface{}, error) {
		return client.MetricsMetadata(ctx, req)
	})
	if err != nil {
		return nil, err
	}

	result := []scrape.MetricMetadata{}
	dedupTracker := map[mimirpb.MetricMetadata]struct{}{}
	for _, resp := range resps {
		r := resp.(*ingester_client.MetricsMetadataResponse)
		for _, m := range r.Metadata {
			// Given we look across all ingesters - dedup the metadata.
			_, ok := dedupTracker[*m]
			if ok {
				continue
			}
			dedupTracker[*m] = struct{}{}

			result = append(result, scrape.MetricMetadata{
				Metric: m.MetricFamilyName,
				Help:   m.Help,
				Unit:   m.Unit,
				Type:   mimirpb.MetricMetadataMetricTypeToMetricType(m.GetType()),
			})
		}
	}

	return result, nil
}

// UserStats returns statistics about the current user.
func (d *Distributor) UserStats(ctx context.Context) (*UserStats, error) {
	replicationSet, err := d.GetIngestersForMetadata(ctx)
	if err != nil {
		return nil, err
	}

	// Make sure we get a successful response from all of them.
	replicationSet.MaxErrors = 0

	req := &ingester_client.UserStatsRequest{}
	resps, err := d.ForReplicationSet(ctx, replicationSet, func(ctx context.Context, client ingester_client.IngesterClient) (interface{}, error) {
		return client.UserStats(ctx, req)
	})
	if err != nil {
		return nil, err
	}

	totalStats := &UserStats{}
	for _, resp := range resps {
		r := resp.(*ingester_client.UserStatsResponse)
		totalStats.IngestionRate += r.IngestionRate
		totalStats.APIIngestionRate += r.ApiIngestionRate
		totalStats.RuleIngestionRate += r.RuleIngestionRate
		totalStats.NumSeries += r.NumSeries
	}

	totalStats.IngestionRate /= float64(d.ingestersRing.ReplicationFactor())
	totalStats.NumSeries /= uint64(d.ingestersRing.ReplicationFactor())

	return totalStats, nil
}

// UserIDStats models ingestion statistics for one user, including the user ID
type UserIDStats struct {
	UserID string `json:"userID"`
	UserStats
}

// AllUserStats returns statistics about all users.
// Note it does not divide by the ReplicationFactor like UserStats()
func (d *Distributor) AllUserStats(ctx context.Context) ([]UserIDStats, error) {
	// Add up by user, across all responses from ingesters
	perUserTotals := make(map[string]UserStats)

	req := &ingester_client.UserStatsRequest{}
	ctx = user.InjectOrgID(ctx, "1") // fake: ingester insists on having an org ID
	// Not using d.ForReplicationSet(), so we can fail after first error.
	replicationSet, err := d.ingestersRing.GetAllHealthy(ring.Read)
	if err != nil {
		return nil, err
	}
	for _, ingester := range replicationSet.Instances {
		client, err := d.ingesterPool.GetClientFor(ingester.Addr)
		if err != nil {
			return nil, err
		}
		resp, err := client.(ingester_client.IngesterClient).AllUserStats(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, u := range resp.Stats {
			s := perUserTotals[u.UserId]
			s.IngestionRate += u.Data.IngestionRate
			s.APIIngestionRate += u.Data.ApiIngestionRate
			s.RuleIngestionRate += u.Data.RuleIngestionRate
			s.NumSeries += u.Data.NumSeries
			perUserTotals[u.UserId] = s
		}
	}

	// Turn aggregated map into a slice for return
	response := make([]UserIDStats, 0, len(perUserTotals))
	for id, stats := range perUserTotals {
		response = append(response, UserIDStats{
			UserID: id,
			UserStats: UserStats{
				IngestionRate:     stats.IngestionRate,
				APIIngestionRate:  stats.APIIngestionRate,
				RuleIngestionRate: stats.RuleIngestionRate,
				NumSeries:         stats.NumSeries,
			},
		})
	}

	return response, nil
}

func (d *Distributor) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if d.distributorsRing != nil {
		d.distributorsRing.ServeHTTP(w, req)
	} else {
		ringNotEnabledPage := `
			<!DOCTYPE html>
			<html>
				<head>
					<meta charset="UTF-8">
					<title>Distributor Status</title>
				</head>
				<body>
					<h1>Distributor Status</h1>
					<p>Distributor is not running with global limits enabled</p>
				</body>
			</html>`
		util.WriteHTMLResponse(w, ringNotEnabledPage)
	}
}
