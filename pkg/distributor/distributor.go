package distributor

import (
	"context"
	"flag"
	"fmt"
	"math"
	"net/http"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gogo/status"
	"github.com/prometheus/otlptranslator"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc/codes"

	"github.com/grafana/dskit/httpgrpc"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/limiter"
	"github.com/grafana/dskit/ring"
	ring_client "github.com/grafana/dskit/ring/client"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/tenant"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/atomic"

	"github.com/grafana/loki/v3/pkg/analytics"
	"github.com/grafana/loki/v3/pkg/compactor/retention"
	"github.com/grafana/loki/v3/pkg/distributor/clientpool"
	"github.com/grafana/loki/v3/pkg/distributor/shardstreams"
	"github.com/grafana/loki/v3/pkg/distributor/writefailures"
	"github.com/grafana/loki/v3/pkg/ingester"
	ingester_client "github.com/grafana/loki/v3/pkg/ingester/client"
	"github.com/grafana/loki/v3/pkg/kafka"
	kafka_client "github.com/grafana/loki/v3/pkg/kafka/client"
	limits_frontend "github.com/grafana/loki/v3/pkg/limits/frontend"
	limits_frontend_client "github.com/grafana/loki/v3/pkg/limits/frontend/client"
	"github.com/grafana/loki/v3/pkg/loghttp/push"
	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/grafana/loki/v3/pkg/runtime"
	"github.com/grafana/loki/v3/pkg/util"
	"github.com/grafana/loki/v3/pkg/util/constants"
	util_log "github.com/grafana/loki/v3/pkg/util/log"
	lokiring "github.com/grafana/loki/v3/pkg/util/ring"
	"github.com/grafana/loki/v3/pkg/validation"
)

const (
	ringKey = "distributor"

	ringAutoForgetUnhealthyPeriods = 2

	timeShardLabel = "__time_shard__"
)

var (
	maxLabelCacheSize = 100000
	rfStats           = analytics.NewInt("distributor_replication_factor")

	// the rune error replacement is rejected by Prometheus hence replacing them with space.
	removeInvalidUtf = func(r rune) rune {
		if r == utf8.RuneError {
			return 32 // rune value for space
		}
		return r
	}
)

// Config for a Distributor.
type Config struct {
	// Distributors ring
	DistributorRing RingConfig `yaml:"ring,omitempty"`
	PushWorkerCount int        `yaml:"push_worker_count"`

	// Request parser
	MaxRecvMsgSize int `yaml:"max_recv_msg_size"`

	// For testing.
	factory ring_client.PoolFactory `yaml:"-"`

	// RateStore customizes the rate storing used by stream sharding.
	RateStore RateStoreConfig `yaml:"rate_store"`

	// WriteFailuresLoggingCfg customizes write failures logging behavior.
	WriteFailuresLogging writefailures.Cfg `yaml:"write_failures_logging" doc:"description=Customize the logging of write failures."`

	OTLPConfig push.GlobalOTLPConfig `yaml:"otlp_config"`

	KafkaEnabled              bool `yaml:"kafka_writes_enabled"`
	IngesterEnabled           bool `yaml:"ingester_writes_enabled"`
	IngestLimitsEnabled       bool `yaml:"ingest_limits_enabled"`
	IngestLimitsDryRunEnabled bool `yaml:"ingest_limits_dry_run_enabled"`

	KafkaConfig kafka.Config `yaml:"-"`

	// TODO: cleanup config
	TenantTopic TenantTopicConfig `yaml:"tenant_topic" category:"experimental"`
}

// RegisterFlags registers distributor-related flags.
func (cfg *Config) RegisterFlags(fs *flag.FlagSet) {
	cfg.OTLPConfig.RegisterFlags(fs)
	cfg.DistributorRing.RegisterFlags(fs)
	cfg.RateStore.RegisterFlagsWithPrefix("distributor.rate-store", fs)
	cfg.WriteFailuresLogging.RegisterFlagsWithPrefix("distributor.write-failures-logging", fs)
	cfg.TenantTopic.RegisterFlags(fs)
	fs.IntVar(&cfg.MaxRecvMsgSize, "distributor.max-recv-msg-size", 100<<20, "The maximum size of a received message.")
	fs.IntVar(&cfg.PushWorkerCount, "distributor.push-worker-count", 256, "Number of workers to push batches to ingesters.")
	fs.BoolVar(&cfg.KafkaEnabled, "distributor.kafka-writes-enabled", false, "Enable writes to Kafka during Push requests.")
	fs.BoolVar(&cfg.IngesterEnabled, "distributor.ingester-writes-enabled", true, "Enable writes to Ingesters during Push requests. Defaults to true.")
	fs.BoolVar(&cfg.IngestLimitsEnabled, "distributor.ingest-limits-enabled", false, "Enable checking limits against the ingest-limits service. Defaults to false.")
	fs.BoolVar(&cfg.IngestLimitsDryRunEnabled, "distributor.ingest-limits-dry-run-enabled", false, "Enable dry-run mode where limits are checked the ingest-limits service, but not enforced. Defaults to false.")
}

func (cfg *Config) Validate() error {
	if !cfg.KafkaEnabled && !cfg.IngesterEnabled {
		return fmt.Errorf("at least one of kafka and ingestor writes must be enabled")
	}
	if err := cfg.TenantTopic.Validate(); err != nil {
		return errors.Wrap(err, "validating tenant topic config")
	}
	return nil
}

// RateStore manages the ingestion rate of streams, populated by data fetched from ingesters.
type RateStore interface {
	RateFor(tenantID string, streamHash uint64) (int64, float64)
}

type KafkaProducer interface {
	ProduceSync(ctx context.Context, records []*kgo.Record) kgo.ProduceResults
	Close()
}

// Distributor coordinates replicates and distribution of log streams.
type Distributor struct {
	services.Service

	cfg              Config
	ingesterCfg      ingester.Config
	logger           log.Logger
	clientCfg        ingester_client.Config
	tenantConfigs    *runtime.TenantConfigs
	tenantsRetention *retention.TenantsRetention
	ingestersRing    ring.ReadRing
	validator        *Validator
	ingesterClients  *ring_client.Pool
	tee              Tee

	rateStore    RateStore
	shardTracker *ShardTracker

	// The global rate limiter requires a distributors ring to count
	// the number of healthy instances.
	distributorsLifecycler *ring.BasicLifecycler
	distributorsRing       *ring.Ring
	healthyInstancesCount  *atomic.Uint32

	rateLimitStrat string

	subservices        *services.Manager
	subservicesWatcher *services.FailureWatcher
	// Per-user rate limiter.
	ingestionRateLimiter *limiter.RateLimiter
	labelCache           *lru.Cache[string, labelData]

	// Push failures rate limiter.
	writeFailuresManager *writefailures.Manager

	RequestParserWrapper push.RequestParserWrapper

	// metrics
	ingesterAppends                       *prometheus.CounterVec
	ingesterAppendTimeouts                *prometheus.CounterVec
	replicationFactor                     prometheus.Gauge
	streamShardCount                      prometheus.Counter
	tenantPushSanitizedStructuredMetadata *prometheus.CounterVec

	usageTracker   push.UsageTracker
	ingesterTasks  chan pushIngesterTask
	ingesterTaskWg sync.WaitGroup

	// Will succeed usage tracker in future.
	ingestLimits *ingestLimits

	// kafka
	kafkaWriter   KafkaProducer
	partitionRing ring.PartitionRingReader

	// The number of partitions for the stream metadata topic. Unlike stream
	// records, where entries are sharded over just the active partitions,
	// stream metadata is sharded over all partitions, and all partitions
	// are consumed.
	numMetadataPartitions int

	// kafka metrics
	kafkaAppends           *prometheus.CounterVec
	kafkaWriteBytesTotal   prometheus.Counter
	kafkaWriteLatency      prometheus.Histogram
	kafkaRecordsPerRequest prometheus.Histogram
}

// New a distributor creates.
func New(
	cfg Config,
	ingesterCfg ingester.Config,
	clientCfg ingester_client.Config,
	configs *runtime.TenantConfigs,
	ingestersRing ring.ReadRing,
	partitionRing ring.PartitionRingReader,
	overrides Limits,
	registerer prometheus.Registerer,
	metricsNamespace string,
	tee Tee,
	usageTracker push.UsageTracker,
	limitsFrontendCfg limits_frontend_client.Config,
	limitsFrontendRing ring.ReadRing,
	numMetadataPartitions int,
	logger log.Logger,
) (*Distributor, error) {
	ingesterClientFactory := cfg.factory
	if ingesterClientFactory == nil {
		ingesterClientFactory = ring_client.PoolAddrFunc(func(addr string) (ring_client.PoolClient, error) {
			return ingester_client.New(clientCfg, addr)
		})
	}

	internalIngesterClientFactory := func(addr string) (ring_client.PoolClient, error) {
		internalCfg := clientCfg
		internalCfg.Internal = true
		return ingester_client.New(internalCfg, addr)
	}

	validator, err := NewValidator(overrides, usageTracker)
	if err != nil {
		return nil, err
	}

	limitsFrontendClientFactory := limits_frontend_client.NewPoolFactory(limitsFrontendCfg)
	limitsFrontendClientPool := limits_frontend_client.NewPool(
		limits_frontend.RingName,
		limitsFrontendCfg.PoolConfig,
		limitsFrontendRing,
		limitsFrontendClientFactory,
		logger,
	)
	limitsFrontendClient := newIngestLimitsFrontendRingClient(
		limitsFrontendRing,
		limitsFrontendClientPool,
	)

	// Create the configured ingestion rate limit strategy (local or global).
	var ingestionRateStrategy limiter.RateLimiterStrategy
	var distributorsLifecycler *ring.BasicLifecycler
	var distributorsRing *ring.Ring

	var servs []services.Service

	rateLimitStrat := validation.LocalIngestionRateStrategy
	labelCache, err := lru.New[string, labelData](maxLabelCacheSize)
	if err != nil {
		return nil, err
	}

	if partitionRing == nil && cfg.KafkaEnabled {
		return nil, fmt.Errorf("partition ring is required for kafka writes")
	}

	var kafkaWriter KafkaProducer
	if cfg.KafkaEnabled {
		kafkaClient, err := kafka_client.NewWriterClient("distributor", cfg.KafkaConfig, 20, logger, registerer)
		if err != nil {
			return nil, fmt.Errorf("failed to start kafka client: %w", err)
		}
		kafkaWriter = kafka_client.NewProducer("distributor", kafkaClient, cfg.KafkaConfig.ProducerMaxBufferedBytes,
			prometheus.WrapRegistererWithPrefix("loki_", registerer))

		// TODO: cleanup/make independent of whether we write kafka as primary?
		if cfg.TenantTopic.Enabled {
			w, err := NewTenantTopicWriter(cfg.TenantTopic, kafkaClient, overrides, registerer, logger)
			if err != nil {
				return nil, fmt.Errorf("failed to start tenant topic tee: %w", err)
			}

			tee = WrapTee(tee, w)
		}
	}

	d := &Distributor{
		cfg:                   cfg,
		ingesterCfg:           ingesterCfg,
		logger:                logger,
		clientCfg:             clientCfg,
		tenantConfigs:         configs,
		tenantsRetention:      retention.NewTenantsRetention(overrides),
		ingestersRing:         ingestersRing,
		validator:             validator,
		ingesterClients:       clientpool.NewPool("ingester", clientCfg.PoolConfig, ingestersRing, ingesterClientFactory, logger, metricsNamespace),
		labelCache:            labelCache,
		shardTracker:          NewShardTracker(),
		healthyInstancesCount: atomic.NewUint32(0),
		rateLimitStrat:        rateLimitStrat,
		tee:                   tee,
		usageTracker:          usageTracker,
		ingesterTasks:         make(chan pushIngesterTask),
		ingesterAppends: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Name:      "distributor_ingester_appends_total",
			Help:      "The total number of batch appends sent to ingesters.",
		}, []string{"ingester"}),
		ingesterAppendTimeouts: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Name:      "distributor_ingester_append_timeouts_total",
			Help:      "The total number of failed batch appends sent to ingesters due to timeouts.",
		}, []string{"ingester"}),
		replicationFactor: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Namespace: constants.Loki,
			Name:      "distributor_replication_factor",
			Help:      "The configured replication factor.",
		}),
		streamShardCount: promauto.With(registerer).NewCounter(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Name:      "stream_sharding_count",
			Help:      "Total number of times the distributor has sharded streams",
		}),
		tenantPushSanitizedStructuredMetadata: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Name:      "distributor_push_structured_metadata_sanitized_total",
			Help:      "The total number of times we've had to sanitize structured metadata (names or values) at ingestion time per tenant.",
		}, []string{"tenant", "format"}),
		kafkaAppends: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Name:      "distributor_kafka_appends_total",
			Help:      "The total number of appends sent to kafka ingest path.",
		}, []string{"partition", "status"}),
		kafkaWriteLatency: promauto.With(registerer).NewHistogram(prometheus.HistogramOpts{
			Namespace:                       constants.Loki,
			Name:                            "distributor_kafka_latency_seconds",
			Help:                            "Latency to write an incoming request to the ingest storage.",
			NativeHistogramBucketFactor:     1.1,
			NativeHistogramMinResetDuration: 1 * time.Hour,
			NativeHistogramMaxBucketNumber:  100,
			Buckets:                         prometheus.DefBuckets,
		}),
		kafkaWriteBytesTotal: promauto.With(registerer).NewCounter(prometheus.CounterOpts{
			Namespace: constants.Loki,
			Name:      "distributor_kafka_sent_bytes_total",
			Help:      "Total number of bytes sent to the ingest storage.",
		}),
		kafkaRecordsPerRequest: promauto.With(registerer).NewHistogram(prometheus.HistogramOpts{
			Namespace: constants.Loki,
			Name:      "distributor_kafka_records_per_write_request",
			Help:      "The number of records a single per-partition write request has been split into.",
			Buckets:   prometheus.ExponentialBuckets(1, 2, 8),
		}),
		writeFailuresManager:  writefailures.NewManager(logger, registerer, cfg.WriteFailuresLogging, configs, "distributor"),
		kafkaWriter:           kafkaWriter,
		partitionRing:         partitionRing,
		ingestLimits:          newIngestLimits(limitsFrontendClient, registerer),
		numMetadataPartitions: numMetadataPartitions,
	}

	if overrides.IngestionRateStrategy() == validation.GlobalIngestionRateStrategy {
		d.rateLimitStrat = validation.GlobalIngestionRateStrategy

		distributorsRing, distributorsLifecycler, err = newRingAndLifecycler(cfg.DistributorRing, d.healthyInstancesCount, logger, registerer, metricsNamespace)
		if err != nil {
			return nil, err
		}

		servs = append(servs, distributorsLifecycler, distributorsRing)

		ingestionRateStrategy = newGlobalIngestionRateStrategy(overrides, d)
	} else {
		ingestionRateStrategy = newLocalIngestionRateStrategy(overrides)
	}

	d.ingestionRateLimiter = limiter.NewRateLimiter(ingestionRateStrategy, 10*time.Second)
	d.distributorsRing = distributorsRing
	d.distributorsLifecycler = distributorsLifecycler

	d.replicationFactor.Set(float64(ingestersRing.ReplicationFactor()))
	rfStats.Set(int64(ingestersRing.ReplicationFactor()))

	rs := NewRateStore(
		d.cfg.RateStore,
		ingestersRing,
		clientpool.NewPool(
			"rate-store",
			clientCfg.PoolConfig,
			ingestersRing,
			ring_client.PoolAddrFunc(internalIngesterClientFactory),
			logger,
			metricsNamespace,
		),
		overrides,
		registerer,
	)
	d.rateStore = rs

	servs = append(servs, d.ingesterClients, rs)
	d.subservices, err = services.NewManager(servs...)
	if err != nil {
		return nil, errors.Wrap(err, "services manager")
	}
	d.subservicesWatcher = services.NewFailureWatcher()
	d.subservicesWatcher.WatchManager(d.subservices)
	d.Service = services.NewBasicService(d.starting, d.running, d.stopping)

	return d, nil
}

func (d *Distributor) starting(ctx context.Context) error {
	return services.StartManagerAndAwaitHealthy(ctx, d.subservices)
}

func (d *Distributor) running(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		d.ingesterTaskWg.Wait()
	}()
	d.ingesterTaskWg.Add(d.cfg.PushWorkerCount)
	for i := 0; i < d.cfg.PushWorkerCount; i++ {
		go d.pushIngesterWorker(ctx)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-d.subservicesWatcher.Chan():
		return errors.Wrap(err, "distributor subservice failed")
	}
}

func (d *Distributor) stopping(_ error) error {
	if d.kafkaWriter != nil {
		d.kafkaWriter.Close()
	}
	return services.StopManagerAndAwaitStopped(context.Background(), d.subservices)
}

type KeyedStream struct {
	HashKey        uint32
	HashKeyNoShard uint64
	Stream         logproto.Stream
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
type streamTracker struct {
	KeyedStream
	minSuccess  int
	maxFailures int
	succeeded   atomic.Int32
	failed      atomic.Int32
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
type pushTracker struct {
	streamsPending atomic.Int32
	streamsFailed  atomic.Int32
	done           chan struct{}
	err            chan error
}

// doneWithResult records the result of a stream push.
// If err is nil, the stream push is considered successful.
// If err is not nil, the stream push is considered failed.
func (p *pushTracker) doneWithResult(err error) {
	if err == nil {
		if p.streamsPending.Dec() == 0 {
			p.done <- struct{}{}
		}
	} else {
		if p.streamsFailed.Inc() == 1 {
			p.err <- err
		}
	}
}

func (d *Distributor) waitSimulatedLatency(ctx context.Context, tenantID string, start time.Time) {
	latency := d.validator.Limits.SimulatedPushLatency(tenantID)
	if latency > 0 {
		// All requests must wait at least the simulated latency. However,
		// we want to avoid adding additional latency on top of slow requests
		// that already took longer then the simulated latency.
		wait := latency - time.Since(start)
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return // The client canceled the request.
			}
		}
	}
}

func (d *Distributor) Push(ctx context.Context, req *logproto.PushRequest) (*logproto.PushResponse, error) {
	tenantID, err := tenant.TenantID(ctx)
	if err != nil {
		return nil, err
	}
	return d.PushWithResolver(ctx, req, newRequestScopedStreamResolver(tenantID, d.validator.Limits, d.logger), constants.Loki)
}

// Push a set of streams.
// The returned error is the last one seen.
func (d *Distributor) PushWithResolver(ctx context.Context, req *logproto.PushRequest, streamResolver *requestScopedStreamResolver, format string) (*logproto.PushResponse, error) {
	tenantID, err := tenant.TenantID(ctx)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	defer d.waitSimulatedLatency(ctx, tenantID, start)

	// Return early if request does not contain any streams
	if len(req.Streams) == 0 {
		return &logproto.PushResponse{}, httpgrpc.Errorf(http.StatusUnprocessableEntity, validation.MissingStreamsErrorMsg)
	}

	// First we flatten out the request into a list of samples.
	// We use the heuristic of 1 sample per TS to size the array.
	// We also work out the hash value at the same time.
	streams := make([]KeyedStream, 0, len(req.Streams))

	var validationErrors util.GroupedErrors

	now := time.Now()
	validationContext := d.validator.getValidationContextForTime(now, tenantID)
	fieldDetector := newFieldDetector(validationContext)
	shouldDiscoverLevels := fieldDetector.shouldDiscoverLogLevels()
	shouldDiscoverGenericFields := fieldDetector.shouldDiscoverGenericFields()

	shardStreamsCfg := d.validator.Limits.ShardStreams(tenantID)
	maybeShardByRate := func(stream logproto.Stream, pushSize int) {
		if shardStreamsCfg.Enabled {
			streams = append(streams, d.shardStream(stream, pushSize, tenantID)...)
			return
		}
		streams = append(streams, KeyedStream{
			HashKey:        lokiring.TokenFor(tenantID, stream.Labels),
			HashKeyNoShard: stream.Hash,
			Stream:         stream,
		})
	}

	maybeShardStreams := func(stream logproto.Stream, labels labels.Labels, pushSize int) {
		if !shardStreamsCfg.TimeShardingEnabled {
			maybeShardByRate(stream, pushSize)
			return
		}

		ignoreRecentFrom := now.Add(-shardStreamsCfg.TimeShardingIgnoreRecent)
		streamsByTime, ok := shardStreamByTime(stream, labels, d.ingesterCfg.MaxChunkAge/2, ignoreRecentFrom)
		if !ok {
			maybeShardByRate(stream, pushSize)
			return
		}

		for _, ts := range streamsByTime {
			maybeShardByRate(ts.Stream, ts.linesTotalLen)
		}
	}

	var ingestionBlockedError error

	func() {
		sp := trace.SpanFromContext(ctx)
		sp.AddEvent("start to validate request")
		defer sp.AddEvent("finished to validate request")

		for _, stream := range req.Streams {
			// Return early if stream does not contain any entries
			if len(stream.Entries) == 0 {
				continue
			}

			// Truncate first so subsequent steps have consistent line lengths
			d.truncateLines(validationContext, &stream)

			var lbs labels.Labels
			var retentionHours, policy string
			lbs, stream.Labels, stream.Hash, retentionHours, policy, err = d.parseStreamLabels(validationContext, stream.Labels, stream, streamResolver, format)
			if err != nil {
				d.writeFailuresManager.Log(tenantID, err)
				validationErrors.Add(err)
				discardedBytes := util.EntriesTotalSize(stream.Entries)
				d.validator.reportDiscardedDataWithTracker(ctx, validation.InvalidLabels, validationContext, lbs, retentionHours, policy, discardedBytes, len(stream.Entries), format)
				continue
			}

			if !d.validator.IsInternalStream(lbs) {
				if missing, lbsMissing := d.missingEnforcedLabels(lbs, tenantID, policy); missing {
					err := fmt.Errorf(validation.MissingEnforcedLabelsErrorMsg, strings.Join(lbsMissing, ","), tenantID, stream.Labels)
					d.writeFailuresManager.Log(tenantID, err)
					validationErrors.Add(err)
					discardedBytes := util.EntriesTotalSize(stream.Entries)
					d.validator.reportDiscardedDataWithTracker(ctx, validation.MissingEnforcedLabels, validationContext, lbs, retentionHours, policy, discardedBytes, len(stream.Entries), format)
					continue
				}
			}

			if block, statusCode, reason, err := d.validator.ShouldBlockIngestion(validationContext, now, policy); block {
				d.writeFailuresManager.Log(tenantID, err)
				discardedBytes := util.EntriesTotalSize(stream.Entries)
				d.validator.reportDiscardedDataWithTracker(ctx, reason, validationContext, lbs, retentionHours, policy, discardedBytes, len(stream.Entries), format)

				// If the status code is 200, return no error.
				// Note that we still log the error and increment the metrics.
				if statusCode == http.StatusOK {
					continue
				}

				// return an error but do not add it to validationErrors
				// otherwise client will get a 400 and will log it.
				ingestionBlockedError = httpgrpc.Errorf(statusCode, "%s", err.Error())
				continue
			}

			n := 0
			pushSize := 0
			prevTs := stream.Entries[0].Timestamp

			labelNamer := otlptranslator.LabelNamer{}
			for _, entry := range stream.Entries {
				if err := d.validator.ValidateEntry(ctx, validationContext, lbs, entry, retentionHours, policy, format); err != nil {
					d.writeFailuresManager.Log(tenantID, err)
					validationErrors.Add(err)
					continue
				}

				var normalized string
				structuredMetadata := logproto.FromLabelAdaptersToLabels(entry.StructuredMetadata)

				normalizedBuilder := labels.NewBuilder(structuredMetadata)

				for _, lbl := range entry.StructuredMetadata {
					normalized = labelNamer.Build(lbl.Name)
					if normalized != lbl.Name {
						// Swap the name with the normalized one.
						normalizedBuilder.Del(lbl.Name)
						normalizedBuilder.Set(normalized, lbl.Value)

						d.tenantPushSanitizedStructuredMetadata.WithLabelValues(tenantID, format).Inc()
					}
					if strings.ContainsRune(lbl.Value, utf8.RuneError) {
						normalizedBuilder.Set(normalized, strings.Map(removeInvalidUtf, lbl.Value))
						d.tenantPushSanitizedStructuredMetadata.WithLabelValues(tenantID, format).Inc()
					}
				}

				// Update structured metadata with normalized labels. We also need to
				// update the original stream to reflect the changes.
				structuredMetadata = normalizedBuilder.Labels()
				entry.StructuredMetadata = logproto.CopyToLabelAdapters(entry.StructuredMetadata, structuredMetadata)

				if shouldDiscoverLevels {
					pprof.Do(ctx, pprof.Labels("action", "discover_log_level"), func(_ context.Context) {
						logLevel, ok := fieldDetector.extractLogLevel(lbs, structuredMetadata, entry)
						if ok {
							entry.StructuredMetadata = append(entry.StructuredMetadata, logLevel)
						}
					})
				}
				if shouldDiscoverGenericFields {
					pprof.Do(ctx, pprof.Labels("action", "discover_generic_fields"), func(_ context.Context) {
						for field, hints := range fieldDetector.validationContext.discoverGenericFields {
							extracted, ok := fieldDetector.extractGenericField(field, hints, lbs, structuredMetadata, entry)
							if ok {
								entry.StructuredMetadata = append(entry.StructuredMetadata, extracted)
							}
						}
					})
				}
				stream.Entries[n] = entry

				// If configured for this tenant, increment duplicate timestamps. Note, this is imperfect
				// since Loki will accept out of order writes it doesn't account for separate
				// pushes with overlapping time ranges having entries with duplicate timestamps

				if validationContext.incrementDuplicateTimestamps && n != 0 {
					// Traditional logic for Loki is that 2 lines with the same timestamp and
					// exact same content will be de-duplicated, (i.e. only one will be stored, others dropped)
					// To maintain this behavior, only increment the timestamp if the log content is different
					if stream.Entries[n-1].Line != entry.Line && (entry.Timestamp == prevTs || entry.Timestamp == stream.Entries[n-1].Timestamp) {
						stream.Entries[n].Timestamp = stream.Entries[n-1].Timestamp.Add(1 * time.Nanosecond)
					} else {
						prevTs = entry.Timestamp
					}
				}

				n++
				validationContext.validationMetrics.compute(entry, retentionHours, policy)
				pushSize += len(entry.Line)
			}
			stream.Entries = stream.Entries[:n]
			if len(stream.Entries) == 0 {
				// Empty stream after validating all the entries
				continue
			}

			maybeShardStreams(stream, lbs, pushSize)
		}
	}()

	var validationErr error
	if validationErrors.Err() != nil {
		validationErr = httpgrpc.Errorf(http.StatusBadRequest, "%s", validationErrors.Error())
	} else if ingestionBlockedError != nil {
		// Any validation error takes precedence over the status code and error message for blocked ingestion.
		validationErr = ingestionBlockedError
	}

	// Return early if none of the streams contained entries
	if len(streams) == 0 {
		return &logproto.PushResponse{}, validationErr
	}

	if !d.ingestionRateLimiter.AllowN(now, tenantID, validationContext.validationMetrics.aggregatedPushStats.lineSize) {
		d.trackDiscardedData(ctx, req, validationContext, tenantID, validationContext.validationMetrics, validation.RateLimited, streamResolver, format)

		err = fmt.Errorf(validation.RateLimitedErrorMsg, tenantID, int(d.ingestionRateLimiter.Limit(now, tenantID)), validationContext.validationMetrics.aggregatedPushStats.lineCount, validationContext.validationMetrics.aggregatedPushStats.lineSize)
		d.writeFailuresManager.Log(tenantID, err)
		// Return a 429 to indicate to the client they are being rate limited
		return nil, httpgrpc.Errorf(http.StatusTooManyRequests, "%s", err.Error())
	}

	// These limits are checked after the ingestion rate limit as this
	// is how it works in ingesters.
	if d.cfg.IngestLimitsEnabled {
		accepted, err := d.ingestLimits.EnforceLimits(ctx, tenantID, streams)
		if err == nil && !d.cfg.IngestLimitsDryRunEnabled {
			if len(accepted) == 0 {
				// All streams were rejected, the request should be failed.
				return nil, httpgrpc.Error(http.StatusTooManyRequests, "request exceeded limits")
			}
			streams = accepted
		}
	}

	// Nil check for performance reasons, to avoid dynamic lookup and/or no-op
	// function calls that cannot be inlined.
	if d.tee != nil {
		d.tee.Duplicate(tenantID, streams)
	}

	const maxExpectedReplicationSet = 5 // typical replication factor 3 plus one for inactive plus one for luck
	var descs [maxExpectedReplicationSet]ring.InstanceDesc

	tracker := pushTracker{
		done: make(chan struct{}, 1), // buffer avoids blocking if caller terminates - sendSamples() only sends once on each
		err:  make(chan error, 1),
	}
	streamsToWrite := 0
	if d.cfg.IngesterEnabled {
		streamsToWrite += len(streams)
	}
	if d.cfg.KafkaEnabled {
		streamsToWrite += len(streams)
	}
	// We must correctly set streamsPending before beginning any writes to ensure we don't have a race between finishing all of one path before starting the other.
	tracker.streamsPending.Store(int32(streamsToWrite))

	if d.cfg.KafkaEnabled {
		subring, err := d.partitionRing.PartitionRing().ShuffleShard(tenantID, d.validator.IngestionPartitionsTenantShardSize(tenantID))
		if err != nil {
			return nil, err
		}
		// We don't need to create a new context like the ingester writes, because we don't return unless all writes have succeeded.
		d.sendStreamsToKafka(ctx, streams, tenantID, &tracker, subring)
	}

	if d.cfg.IngesterEnabled {
		streamTrackers := make([]streamTracker, len(streams))
		streamsByIngester := map[string][]*streamTracker{}
		ingesterDescs := map[string]ring.InstanceDesc{}

		if err := func() error {
			sp := trace.SpanFromContext(ctx)
			sp.AddEvent("started to query ingesters ring")
			defer sp.AddEvent("finished to query ingesters ring")

			for i, stream := range streams {
				replicationSet, err := d.ingestersRing.Get(stream.HashKey, ring.WriteNoExtend, descs[:0], nil, nil)
				if err != nil {
					return err
				}

				streamTrackers[i] = streamTracker{
					KeyedStream: stream,
					minSuccess:  len(replicationSet.Instances) - replicationSet.MaxErrors,
					maxFailures: replicationSet.MaxErrors,
				}
				for _, ingester := range replicationSet.Instances {
					streamsByIngester[ingester.Addr] = append(streamsByIngester[ingester.Addr], &streamTrackers[i])
					ingesterDescs[ingester.Addr] = ingester
				}
			}
			return nil
		}(); err != nil {
			return nil, err
		}

		for ingester, streams := range streamsByIngester {
			func(ingester ring.InstanceDesc, samples []*streamTracker) {
				// Clone the context using WithoutCancel, which is not canceled when parent is canceled.
				// This is to make sure all ingesters get samples even if we return early
				localCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.clientCfg.RemoteTimeout)
				sp := trace.SpanFromContext(ctx)
				localCtx = trace.ContextWithSpan(localCtx, sp)

				select {
				case <-ctx.Done():
					cancel()
					return
				case d.ingesterTasks <- pushIngesterTask{
					ingester:      ingester,
					streamTracker: samples,
					pushTracker:   &tracker,
					ctx:           localCtx,
					cancel:        cancel,
				}:
					return
				}
			}(ingesterDescs[ingester], streams)
		}
	}

	select {
	case err := <-tracker.err:
		return nil, err
	case <-tracker.done:
		return &logproto.PushResponse{}, validationErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// missingEnforcedLabels returns true if the stream is missing any of the required labels.
//
// It also returns the first label that is missing if any (for the case of multiple labels missing).
func (d *Distributor) missingEnforcedLabels(lbs labels.Labels, tenantID string, policy string) (bool, []string) {
	perPolicyEnforcedLabels := d.validator.Limits.PolicyEnforcedLabels(tenantID, policy)
	tenantEnforcedLabels := d.validator.Limits.EnforcedLabels(tenantID)

	requiredLbs := append(tenantEnforcedLabels, perPolicyEnforcedLabels...)
	if len(requiredLbs) == 0 {
		// no enforced labels configured.
		return false, []string{}
	}

	// Use a map to deduplicate the required labels. Duplicates may happen if the same label is configured
	// in both the per-tenant and per-policy enforced labels.
	seen := make(map[string]struct{})
	missingLbs := []string{}

	for _, lb := range requiredLbs {
		if _, ok := seen[lb]; ok {
			continue
		}

		seen[lb] = struct{}{}
		if !lbs.Has(lb) {
			missingLbs = append(missingLbs, lb)
		}
	}

	return len(missingLbs) > 0, missingLbs
}

func (d *Distributor) trackDiscardedData(
	ctx context.Context,
	req *logproto.PushRequest,
	validationContext validationContext,
	tenantID string,
	validationMetrics validationMetrics,
	reason string,
	streamResolver push.StreamResolver,
	format string,
) {
	for policy, retentionToStats := range validationMetrics.policyPushStats {
		for retentionHours, stats := range retentionToStats {
			validation.DiscardedSamples.WithLabelValues(reason, tenantID, retentionHours, policy, format).Add(float64(stats.lineCount))
			validation.DiscardedBytes.WithLabelValues(reason, tenantID, retentionHours, policy, format).Add(float64(stats.lineSize))
		}
	}

	if d.usageTracker != nil {
		for _, stream := range req.Streams {
			lbs, _, _, _, _, err := d.parseStreamLabels(validationContext, stream.Labels, stream, streamResolver, format)
			if err != nil {
				continue
			}

			discardedStreamBytes := util.EntriesTotalSize(stream.Entries)

			if d.usageTracker != nil {
				d.usageTracker.DiscardedBytesAdd(ctx, tenantID, reason, lbs, float64(discardedStreamBytes), format)
			}
		}
	}
}

type streamWithTimeShard struct {
	logproto.Stream
	linesTotalLen int
}

// This should shard the stream into multiple sub-streams based on the log
// timestamps, but with no new alocations for the log entries. It will sort them
// in-place in the given stream object (so it may modify it!) and reference
// sub-slices of the same stream.Entries slice.
//
// If the second result is false, it means that either there were no logs in the
// stream, or all of the logs in the stream occurred after the given value of
// ignoreLogsFrom, so there was no need to shard - the original `streams` value
// can be used. However, due to the in-place logs sorting by their timestamp, it
// might still have been reordered.
func shardStreamByTime(stream logproto.Stream, lbls labels.Labels, timeShardLen time.Duration, ignoreLogsFrom time.Time) ([]streamWithTimeShard, bool) {
	entries := stream.Entries
	entriesLen := len(entries)
	if entriesLen == 0 {
		return nil, false
	}

	slices.SortStableFunc(entries, func(a, b logproto.Entry) int { return a.Timestamp.Compare(b.Timestamp) })

	// Shortcut to do no work if all of the logs are recent
	if entries[0].Timestamp.After(ignoreLogsFrom) {
		return nil, false
	}

	result := make([]streamWithTimeShard, 0, (entries[entriesLen-1].Timestamp.Sub(entries[0].Timestamp)/timeShardLen)+1)
	labelBuilder := labels.NewBuilder(lbls)

	startIdx := 0
	for startIdx < entriesLen && entries[startIdx].Timestamp.Before(ignoreLogsFrom) /* the index is changed below */ {
		timeShardStart := entries[startIdx].Timestamp.Truncate(timeShardLen)
		timeShardEnd := timeShardStart.Add(timeShardLen)

		timeShardCutoff := timeShardEnd
		if timeShardCutoff.After(ignoreLogsFrom) {
			// If the time_sharding_ignore_recent is in the middle of this
			// shard, we need to cut off the logs at that point.
			timeShardCutoff = ignoreLogsFrom
		}

		endIdx := startIdx + 1
		linesTotalLen := len(entries[startIdx].Line)
		for ; endIdx < entriesLen && entries[endIdx].Timestamp.Before(timeShardCutoff); endIdx++ {
			linesTotalLen += len(entries[endIdx].Line)
		}

		shardLbls := labelBuilder.Set(timeShardLabel, fmt.Sprintf("%d_%d", timeShardStart.Unix(), timeShardEnd.Unix())).Labels()
		result = append(result, streamWithTimeShard{
			Stream: logproto.Stream{
				Labels:  shardLbls.String(),
				Hash:    labels.StableHash(shardLbls),
				Entries: stream.Entries[startIdx:endIdx],
			},
			linesTotalLen: linesTotalLen,
		})

		startIdx = endIdx
	}

	if startIdx == entriesLen {
		// We do not have any remaining entries
		return result, true
	}

	// Append one last shard with all of the logs without a time shard
	logsWithoutTimeShardLen := 0
	for i := startIdx; i < entriesLen; i++ {
		logsWithoutTimeShardLen += len(entries[i].Line)
	}

	return append(result, streamWithTimeShard{
		Stream: logproto.Stream{
			Labels:  stream.Labels,
			Hash:    stream.Hash,
			Entries: stream.Entries[startIdx:entriesLen],
		},
		linesTotalLen: logsWithoutTimeShardLen,
	}), true
}

// shardStream shards (divides) the given stream into N smaller streams, where
// N is the sharding size for the given stream. shardSteam returns the smaller
// streams and their associated keys for hashing to ingesters.
//
// The number of shards is limited by the number of entries.
func (d *Distributor) shardStream(stream logproto.Stream, pushSize int, tenantID string) []KeyedStream {
	shardStreamsCfg := d.validator.Limits.ShardStreams(tenantID)
	logger := log.With(util_log.WithUserID(tenantID, d.logger), "stream", stream.Labels)
	shardCount := d.shardCountFor(logger, &stream, pushSize, tenantID, shardStreamsCfg)

	if shardCount <= 1 {
		return []KeyedStream{{HashKey: lokiring.TokenFor(tenantID, stream.Labels), HashKeyNoShard: stream.Hash, Stream: stream}}
	}

	d.streamShardCount.Inc()
	if shardStreamsCfg.LoggingEnabled {
		level.Info(logger).Log("msg", "sharding request", "shard_count", shardCount)
	}

	return d.divideEntriesBetweenShards(tenantID, shardCount, shardStreamsCfg, stream)
}

func (d *Distributor) divideEntriesBetweenShards(tenantID string, totalShards int, shardStreamsCfg shardstreams.Config, stream logproto.Stream) []KeyedStream {
	derivedStreams := d.createShards(stream, totalShards, tenantID, shardStreamsCfg)

	for i := 0; i < len(stream.Entries); i++ {
		streamIndex := i % len(derivedStreams)
		entries := append(derivedStreams[streamIndex].Stream.Entries, stream.Entries[i])
		derivedStreams[streamIndex].Stream.Entries = entries
	}

	return derivedStreams
}

func (d *Distributor) createShards(stream logproto.Stream, totalShards int, tenantID string, shardStreamsCfg shardstreams.Config) []KeyedStream {
	var (
		streamLabels   = labelTemplate(stream.Labels, d.logger)
		streamPattern  = streamLabels.String()
		derivedStreams = make([]KeyedStream, 0, totalShards)

		streamCount = streamCount(totalShards, stream)
	)

	if totalShards <= 0 {
		level.Error(d.logger).Log("msg", "attempt to create shard with zeroed total shards", "org_id", tenantID, "stream", stream.Labels, "entries_len", len(stream.Entries))
		return derivedStreams
	}

	entriesPerShard := int(math.Ceil(float64(len(stream.Entries)) / float64(totalShards)))
	startShard := d.shardTracker.LastShardNum(tenantID, stream.Hash)
	for i := 0; i < streamCount; i++ {
		shardNum := (startShard + i) % totalShards
		shard := d.createShard(streamLabels, streamPattern, shardNum, entriesPerShard)

		derivedStreams = append(derivedStreams, KeyedStream{
			HashKey:        lokiring.TokenFor(tenantID, shard.Labels),
			HashKeyNoShard: stream.Hash,
			Stream:         shard,
		})

		if shardStreamsCfg.LoggingEnabled {
			level.Info(d.logger).Log("msg", "stream derived from sharding", "src-stream", stream.Labels, "derived-stream", shard.Labels)
		}
	}
	d.shardTracker.SetLastShardNum(tenantID, stream.Hash, startShard+streamCount)

	return derivedStreams
}

func streamCount(totalShards int, stream logproto.Stream) int {
	if len(stream.Entries) < totalShards {
		return len(stream.Entries)
	}
	return totalShards
}

// labelTemplate returns a label set that includes the dummy label to be replaced
// To avoid allocations, this slice is reused when we know the stream value
func labelTemplate(lbls string, logger log.Logger) labels.Labels {
	baseLbls, err := syntax.ParseLabels(lbls)
	if err != nil {
		level.Error(logger).Log("msg", "couldn't extract labels from stream", "stream", lbls)
		return labels.EmptyLabels()
	}

	builder := labels.NewBuilder(baseLbls)
	builder.Set(ingester.ShardLbName, ingester.ShardLbPlaceholder)
	return builder.Labels()
}

func (d *Distributor) createShard(lbls labels.Labels, streamPattern string, shardNumber, numOfEntries int) logproto.Stream {
	shardLabel := strconv.Itoa(shardNumber)

	builder := labels.NewBuilder(lbls)
	if lbls.Has(ingester.ShardLbName) {
		builder.Set(ingester.ShardLbName, shardLabel)
	}
	lbls = builder.Labels()

	return logproto.Stream{
		Labels:  strings.Replace(streamPattern, ingester.ShardLbPlaceholder, shardLabel, 1),
		Hash:    labels.StableHash(lbls),
		Entries: make([]logproto.Entry, 0, numOfEntries),
	}
}

// maxT returns the highest between two given timestamps.
func maxT(t1, t2 time.Time) time.Time {
	if t1.Before(t2) {
		return t2
	}

	return t1
}

func (d *Distributor) truncateLines(vContext validationContext, stream *logproto.Stream) {
	if !vContext.maxLineSizeTruncate {
		return
	}

	suffix := vContext.maxLineSizeTruncateIdentifier

	var truncatedSamples, truncatedBytes int
	for i, e := range stream.Entries {
		if maxSize := vContext.maxLineSize; maxSize != 0 && len(e.Line) > maxSize {
			truncateTo := maxSize - len(suffix)
			if truncateTo <= 0 {
				continue
			}

			stream.Entries[i].Line = e.Line[:truncateTo] + suffix

			truncatedSamples++
			truncatedBytes += len(e.Line) - truncateTo
		}
	}

	validation.MutatedSamples.WithLabelValues(validation.LineTooLong, vContext.userID).Add(float64(truncatedSamples))
	validation.MutatedBytes.WithLabelValues(validation.LineTooLong, vContext.userID).Add(float64(truncatedBytes))
}

type pushIngesterTask struct {
	streamTracker []*streamTracker
	pushTracker   *pushTracker
	ingester      ring.InstanceDesc
	ctx           context.Context
	cancel        context.CancelFunc
}

func (d *Distributor) pushIngesterWorker(ctx context.Context) {
	defer d.ingesterTaskWg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-d.ingesterTasks:
			d.sendStreams(task)
		}
	}
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
func (d *Distributor) sendStreams(task pushIngesterTask) {
	defer task.cancel()
	err := d.sendStreamsErr(task.ctx, task.ingester, task.streamTracker)

	// If we succeed, decrement each stream's pending count by one.
	// If we reach the required number of successful puts on this stream, then
	// decrement the number of pending streams by one.
	// If we successfully push all streams to min success ingesters, wake up the
	// waiting rpc so it can return early. Similarly, track the number of errors,
	// and if it exceeds maxFailures shortcut the waiting rpc.
	//
	// The use of atomic increments here guarantees only a single sendStreams
	// goroutine will write to either channel.
	for i := range task.streamTracker {
		if err != nil {
			if task.streamTracker[i].failed.Inc() <= int32(task.streamTracker[i].maxFailures) {
				continue
			}
			task.pushTracker.doneWithResult(err)
		} else {
			if task.streamTracker[i].succeeded.Inc() != int32(task.streamTracker[i].minSuccess) {
				continue
			}
			task.pushTracker.doneWithResult(nil)
		}
	}
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
func (d *Distributor) sendStreamsErr(ctx context.Context, ingester ring.InstanceDesc, streams []*streamTracker) error {
	c, err := d.ingesterClients.GetClientFor(ingester.Addr)
	if err != nil {
		return err
	}

	req := &logproto.PushRequest{
		Streams: make([]logproto.Stream, len(streams)),
	}
	for i, s := range streams {
		req.Streams[i] = s.Stream
	}

	_, err = c.(logproto.PusherClient).Push(ctx, req)
	d.ingesterAppends.WithLabelValues(ingester.Addr).Inc()
	if err != nil {
		if e, ok := status.FromError(err); ok {
			switch e.Code() {
			case codes.DeadlineExceeded:
				d.ingesterAppendTimeouts.WithLabelValues(ingester.Addr).Inc()
			}
		}
	}
	return err
}

func (d *Distributor) sendStreamsToKafka(ctx context.Context, streams []KeyedStream, tenant string, tracker *pushTracker, subring *ring.PartitionRing) {
	for _, s := range streams {
		go func(s KeyedStream) {
			err := d.sendStreamToKafka(ctx, s, tenant, subring)
			if err != nil {
				err = fmt.Errorf("failed to write stream to kafka: %w", err)
			}
			tracker.doneWithResult(err)
		}(s)
	}
}

func (d *Distributor) sendStreamToKafka(ctx context.Context, stream KeyedStream, tenant string, subring *ring.PartitionRing) error {
	if len(stream.Stream.Entries) == 0 {
		return nil
	}

	// The distributor writes stream records to one of the active partitions
	// in the partition ring. The number of active partitions is equal to the
	// number of ingesters.
	streamPartitionID, err := subring.ActivePartitionForKey(stream.HashKey)
	if err != nil {
		d.kafkaAppends.WithLabelValues("kafka", "fail").Inc()
		return fmt.Errorf("failed to find active partition for stream: %w", err)
	}
	startTime := time.Now()
	records, err := kafka.Encode(
		streamPartitionID,
		tenant,
		stream.Stream,
		d.cfg.KafkaConfig.ProducerMaxRecordSizeBytes,
	)
	if err != nil {
		d.kafkaAppends.WithLabelValues(
			fmt.Sprintf("partition_%d", streamPartitionID),
			"fail",
		).Inc()
		return fmt.Errorf("failed to marshal write request to records: %w", err)
	}

	d.kafkaRecordsPerRequest.Observe(float64(len(records)))

	produceResults := d.kafkaWriter.ProduceSync(ctx, records)

	if count, sizeBytes := successfulProduceRecordsStats(produceResults); count > 0 {
		d.kafkaWriteLatency.Observe(time.Since(startTime).Seconds())
		d.kafkaWriteBytesTotal.Add(float64(sizeBytes))
	}

	var finalErr error
	for _, result := range produceResults {
		if result.Err != nil {
			d.kafkaAppends.WithLabelValues(fmt.Sprintf("partition_%d", streamPartitionID), "fail").Inc()
			finalErr = result.Err
		} else {
			d.kafkaAppends.WithLabelValues(fmt.Sprintf("partition_%d", streamPartitionID), "success").Inc()
		}
	}

	return finalErr
}

func successfulProduceRecordsStats(results kgo.ProduceResults) (count, sizeBytes int) {
	for _, res := range results {
		if res.Err == nil && res.Record != nil {
			count++
			sizeBytes += len(res.Record.Value)
		}
	}

	return
}

type labelData struct {
	ls   labels.Labels
	hash uint64
}

// parseStreamLabels parses stream labels using a request-scoped policy resolver
func (d *Distributor) parseStreamLabels(vContext validationContext, key string, stream logproto.Stream, streamResolver push.StreamResolver, format string) (labels.Labels, string, uint64, string, string, error) {
	if val, ok := d.labelCache.Get(key); ok {
		retentionHours := streamResolver.RetentionHoursFor(val.ls)
		policy := streamResolver.PolicyFor(val.ls)
		return val.ls, val.ls.String(), val.hash, retentionHours, policy, nil
	}

	ls, err := syntax.ParseLabels(key)
	if err != nil {
		retentionHours := d.tenantsRetention.RetentionHoursFor(vContext.userID, labels.EmptyLabels())
		// TODO: check for global policy.
		return labels.EmptyLabels(), "", 0, retentionHours, "", fmt.Errorf(validation.InvalidLabelsErrorMsg, key, err)
	}

	policy := streamResolver.PolicyFor(ls)
	retentionHours := d.tenantsRetention.RetentionHoursFor(vContext.userID, ls)

	if err := d.validator.ValidateLabels(vContext, ls, stream, retentionHours, policy, format); err != nil {
		return labels.EmptyLabels(), "", 0, retentionHours, policy, err
	}

	lsHash := labels.StableHash(ls)

	d.labelCache.Add(key, labelData{ls, lsHash})
	return ls, ls.String(), lsHash, retentionHours, policy, nil
}

// shardCountFor returns the right number of shards to be used by the given stream.
//
// It first checks if the number of shards is present in the shard store. If it isn't it will calculate it
// based on the rate stored in the rate store and will store the new evaluated number of shards.
//
// desiredRate is expected to be given in bytes.
func (d *Distributor) shardCountFor(logger log.Logger, stream *logproto.Stream, pushSize int, tenantID string, streamShardcfg shardstreams.Config) int {
	if streamShardcfg.DesiredRate.Val() <= 0 {
		if streamShardcfg.LoggingEnabled {
			level.Error(logger).Log("msg", "invalid desired rate", "desired_rate", streamShardcfg.DesiredRate.String())
		}
		return 1
	}

	rate, pushRate := d.rateStore.RateFor(tenantID, stream.Hash)
	if pushRate == 0 {
		// first push, don't shard until the rate is understood
		return 1
	}

	if pushRate > 1 {
		// High throughput. Let stream sharding do its job and
		// don't attempt to amortize the push size over the
		// real rate
		pushRate = 1
	}

	shards := calculateShards(rate, int(float64(pushSize)*pushRate), streamShardcfg.DesiredRate.Val())
	if shards == 0 {
		// 1 shard is enough for the given stream.
		return 1
	}

	return shards
}

func calculateShards(rate int64, pushSize, desiredRate int) int {
	shards := float64(rate+int64(pushSize)) / float64(desiredRate)
	if shards <= 1 {
		return 1
	}
	return int(math.Ceil(shards))
}

func calculateStreamSizes(stream logproto.Stream) (uint64, uint64) {
	var entriesSize, structuredMetadataSize uint64
	for _, entry := range stream.Entries {
		entriesSize += uint64(len(entry.Line))
		structuredMetadataSize += uint64(util.StructuredMetadataSize(entry.StructuredMetadata))
	}
	return entriesSize, structuredMetadataSize
}

// newRingAndLifecycler creates a new distributor ring and lifecycler with all required lifecycler delegates
func newRingAndLifecycler(cfg RingConfig, instanceCount *atomic.Uint32, logger log.Logger, reg prometheus.Registerer, metricsNamespace string) (*ring.Ring, *ring.BasicLifecycler, error) {
	kvStore, err := kv.NewClient(cfg.KVStore, ring.GetCodec(), kv.RegistererWithKVName(reg, "distributor-lifecycler"), logger)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to initialize distributors' KV store")
	}

	lifecyclerCfg, err := cfg.ToBasicLifecyclerConfig(logger)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to build distributors' lifecycler config")
	}

	var delegate ring.BasicLifecyclerDelegate
	delegate = ring.NewInstanceRegisterDelegate(ring.ACTIVE, 1)
	delegate = newHealthyInstanceDelegate(instanceCount, cfg.HeartbeatTimeout, delegate)
	delegate = ring.NewLeaveOnStoppingDelegate(delegate, logger)
	delegate = ring.NewAutoForgetDelegate(ringAutoForgetUnhealthyPeriods*cfg.HeartbeatTimeout, delegate, logger)

	distributorsLifecycler, err := ring.NewBasicLifecycler(lifecyclerCfg, "distributor", ringKey, kvStore, delegate, logger, prometheus.WrapRegistererWithPrefix(metricsNamespace+"_", reg))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to initialize distributors' lifecycler")
	}

	distributorsRing, err := ring.New(cfg.ToRingConfig(), "distributor", ringKey, logger, prometheus.WrapRegistererWithPrefix(metricsNamespace+"_", reg))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to initialize distributors' ring client")
	}

	return distributorsRing, distributorsLifecycler, nil
}

// HealthyInstancesCount implements the ReadLifecycler interface.
//
// We use a ring lifecycler delegate to count the number of members of the
// ring. The count is then used to enforce rate limiting correctly for each
// distributor. $EFFECTIVE_RATE_LIMIT = $GLOBAL_RATE_LIMIT / $NUM_INSTANCES.
func (d *Distributor) HealthyInstancesCount() int {
	return int(d.healthyInstancesCount.Load())
}

type requestScopedStreamResolver struct {
	userID               string
	policyStreamMappings validation.PolicyStreamMapping
	retention            *retention.TenantRetentionSnapshot

	logger log.Logger
}

func newRequestScopedStreamResolver(userID string, overrides Limits, logger log.Logger) *requestScopedStreamResolver {
	return &requestScopedStreamResolver{
		userID:               userID,
		policyStreamMappings: overrides.PoliciesStreamMapping(userID),
		retention:            retention.NewTenantRetentionSnapshot(overrides, userID),
		logger:               logger,
	}
}

func (r requestScopedStreamResolver) RetentionPeriodFor(lbs labels.Labels) time.Duration {
	return r.retention.RetentionPeriodFor(lbs)
}

func (r requestScopedStreamResolver) RetentionHoursFor(lbs labels.Labels) string {
	return r.retention.RetentionHoursFor(lbs)
}

func (r requestScopedStreamResolver) PolicyFor(lbs labels.Labels) string {
	policies := r.policyStreamMappings.PolicyFor(lbs)

	var policy string
	if len(policies) > 0 {
		policy = policies[0]
		if len(policies) > 1 {
			level.Warn(r.logger).Log(
				"msg", "multiple policies matched for the same stream",
				"org_id", r.userID,
				"stream", lbs.String(),
				"policy", policy,
				"policies", strings.Join(policies, ","),
				"insight", "true",
			)
		}
	}

	return policy
}
