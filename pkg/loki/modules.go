package loki

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/dns"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/kv/codec"
	"github.com/grafana/dskit/kv/memberlist"
	"github.com/grafana/dskit/middleware"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/runtimeconfig"
	"github.com/grafana/dskit/server"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/user"
	gerrors "github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/common/model"
	"github.com/thanos-io/objstore"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/grafana/loki/v3/pkg/analytics"
	blockbuilder "github.com/grafana/loki/v3/pkg/blockbuilder/builder"
	blockscheduler "github.com/grafana/loki/v3/pkg/blockbuilder/scheduler"
	blocktypes "github.com/grafana/loki/v3/pkg/blockbuilder/types"
	blockprotos "github.com/grafana/loki/v3/pkg/blockbuilder/types/proto"
	"github.com/grafana/loki/v3/pkg/bloombuild/builder"
	"github.com/grafana/loki/v3/pkg/bloombuild/planner"
	bloomprotos "github.com/grafana/loki/v3/pkg/bloombuild/protos"
	"github.com/grafana/loki/v3/pkg/bloomgateway"
	"github.com/grafana/loki/v3/pkg/compactor"
	compactorclient "github.com/grafana/loki/v3/pkg/compactor/client"
	"github.com/grafana/loki/v3/pkg/compactor/client/grpc"
	"github.com/grafana/loki/v3/pkg/compactor/deletion"
	"github.com/grafana/loki/v3/pkg/compactor/generationnumber"
	"github.com/grafana/loki/v3/pkg/dataobj/consumer"
	"github.com/grafana/loki/v3/pkg/dataobj/explorer"
	dataobjindex "github.com/grafana/loki/v3/pkg/dataobj/index"
	"github.com/grafana/loki/v3/pkg/dataobj/metastore"
	dataobjquerier "github.com/grafana/loki/v3/pkg/dataobj/querier"
	"github.com/grafana/loki/v3/pkg/distributor"
	"github.com/grafana/loki/v3/pkg/indexgateway"
	"github.com/grafana/loki/v3/pkg/ingester"
	"github.com/grafana/loki/v3/pkg/kafka/partition"
	"github.com/grafana/loki/v3/pkg/limits"
	limits_frontend "github.com/grafana/loki/v3/pkg/limits/frontend"
	limitsproto "github.com/grafana/loki/v3/pkg/limits/proto"
	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logql"
	"github.com/grafana/loki/v3/pkg/logqlmodel/stats"
	"github.com/grafana/loki/v3/pkg/lokifrontend/frontend"
	"github.com/grafana/loki/v3/pkg/lokifrontend/frontend/transport"
	"github.com/grafana/loki/v3/pkg/lokifrontend/frontend/v1/frontendv1pb"
	"github.com/grafana/loki/v3/pkg/lokifrontend/frontend/v2/frontendv2pb"
	"github.com/grafana/loki/v3/pkg/pattern"
	"github.com/grafana/loki/v3/pkg/querier"
	"github.com/grafana/loki/v3/pkg/querier/queryrange"
	"github.com/grafana/loki/v3/pkg/querier/queryrange/queryrangebase"
	"github.com/grafana/loki/v3/pkg/querier/tail"
	"github.com/grafana/loki/v3/pkg/ruler"
	base_ruler "github.com/grafana/loki/v3/pkg/ruler/base"
	"github.com/grafana/loki/v3/pkg/ruler/rulestore/local"
	"github.com/grafana/loki/v3/pkg/runtime"
	"github.com/grafana/loki/v3/pkg/scheduler"
	"github.com/grafana/loki/v3/pkg/scheduler/schedulerpb"
	"github.com/grafana/loki/v3/pkg/storage"
	"github.com/grafana/loki/v3/pkg/storage/bucket"
	"github.com/grafana/loki/v3/pkg/storage/chunk/cache"
	"github.com/grafana/loki/v3/pkg/storage/chunk/client"
	chunk_util "github.com/grafana/loki/v3/pkg/storage/chunk/client/util"
	"github.com/grafana/loki/v3/pkg/storage/config"
	"github.com/grafana/loki/v3/pkg/storage/stores/series/index"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/bloomshipper"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/indexshipper"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/indexshipper/boltdb"
	boltdbcompactor "github.com/grafana/loki/v3/pkg/storage/stores/shipper/indexshipper/boltdb/compactor"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/indexshipper/tsdb"
	"github.com/grafana/loki/v3/pkg/storage/types"
	"github.com/grafana/loki/v3/pkg/ui"
	"github.com/grafana/loki/v3/pkg/util/constants"
	"github.com/grafana/loki/v3/pkg/util/httpreq"
	"github.com/grafana/loki/v3/pkg/util/limiter"
	util_log "github.com/grafana/loki/v3/pkg/util/log"
	"github.com/grafana/loki/v3/pkg/util/mempool"
	"github.com/grafana/loki/v3/pkg/util/querylimits"
	lokiring "github.com/grafana/loki/v3/pkg/util/ring"
	serverutil "github.com/grafana/loki/v3/pkg/util/server"
	"github.com/grafana/loki/v3/pkg/validation"
)

const maxChunkAgeForTableManager = 12 * time.Hour

// The various modules that make up Loki.
const (
	Ring                     = "ring"
	Overrides                = "overrides"
	OverridesExporter        = "overrides-exporter"
	TenantConfigs            = "tenant-configs"
	Server                   = "server"
	InternalServer           = "internal-server"
	Distributor              = "distributor"
	IngestLimits             = "ingest-limits"
	IngestLimitsRing         = "ingest-limits-ring"
	IngestLimitsFrontend     = "ingest-limits-frontend"
	IngestLimitsFrontendRing = "ingest-limits-frontend-ring"
	Ingester                 = "ingester"
	PatternIngester          = "pattern-ingester"
	PatternRingClient        = "pattern-ring-client"
	PatternIngesterTee       = "pattern-ingester-tee"
	Querier                  = "querier"
	QueryFrontend            = "query-frontend"
	QueryFrontendTripperware = "query-frontend-tripperware"
	QueryLimiter             = "query-limiter"
	QueryLimitsInterceptors  = "query-limits-interceptors"
	QueryLimitsTripperware   = "query-limits-tripperware"
	Store                    = "store"
	TableManager             = "table-manager"
	RulerStorage             = "ruler-storage"
	Ruler                    = "ruler"
	RuleEvaluator            = "rule-evaluator"
	Compactor                = "compactor"
	IndexGateway             = "index-gateway"
	IndexGatewayRing         = "index-gateway-ring"
	IndexGatewayInterceptors = "index-gateway-interceptors"
	BloomStore               = "bloom-store"
	BloomGateway             = "bloom-gateway"
	BloomGatewayClient       = "bloom-gateway-client"
	BloomPlanner             = "bloom-planner"
	BloomBuilder             = "bloom-builder"
	QueryScheduler           = "query-scheduler"
	QuerySchedulerRing       = "query-scheduler-ring"
	IngesterQuerier          = "ingester-querier"
	IngesterGRPCInterceptors = "ingester-grpc-interceptors"
	RuntimeConfig            = "runtime-config"
	MemberlistKV             = "memberlist-kv"
	Analytics                = "analytics"
	CacheGenerationLoader    = "cache-generation-loader"
	PartitionRing            = "partition-ring"
	BlockBuilder             = "block-builder"
	BlockScheduler           = "block-scheduler"
	DataObjExplorer          = "dataobj-explorer"
	DataObjConsumer          = "dataobj-consumer"
	DataObjIndexBuilder      = "dataobj-index-builder"
	UI                       = "ui"
	All                      = "all"
	Read                     = "read"
	Write                    = "write"
	Backend                  = "backend"
)

const (
	schedulerRingKey    = "scheduler"
	indexGatewayRingKey = "index-gateway"
	bloomGatewayRingKey = "bloom-gateway"
)

func (t *Loki) initServer() (services.Service, error) {
	prometheus.MustRegister(version.NewCollector(constants.Loki))
	// unregister default go collector
	prometheus.Unregister(collectors.NewGoCollector())
	// register collector with additional metrics
	prometheus.MustRegister(collectors.NewGoCollector(
		collectors.WithGoCollectorRuntimeMetrics(collectors.MetricsAll),
	))

	// Loki handles signals on its own.
	DisableSignalHandling(&t.Cfg.Server)

	t.Metrics = server.NewServerMetrics(t.Cfg.Server)
	serv, err := server.NewWithMetrics(t.Cfg.Server, t.Metrics)
	if err != nil {
		return nil, err
	}

	t.Server = serv

	servicesToWaitFor := func() []services.Service {
		svs := []services.Service(nil)
		for m, s := range t.serviceMap {
			// Server should not wait for itself.
			if m != Server {
				svs = append(svs, s)
			}
		}
		return svs
	}

	s := NewServerService(t.Server, servicesToWaitFor)

	// Best effort to propagate the org ID from the start.
	h := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !t.Cfg.AuthEnabled {
				next.ServeHTTP(w, r.WithContext(user.InjectOrgID(r.Context(), "fake")))
				return
			}

			_, ctx, _ := user.ExtractOrgIDFromHTTPRequest(r)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}(t.Server.HTTPServer.Handler)

	t.Server.HTTPServer.Handler = middleware.Merge(serverutil.RecoveryHTTPMiddleware).Wrap(h)
	t.Server.HTTPServer.Handler = h2c.NewHandler(t.Server.HTTPServer.Handler, &http2.Server{})

	if t.Cfg.Server.HTTPListenPort == 0 {
		t.Cfg.Server.HTTPListenPort = portFromAddr(t.Server.HTTPListenAddr().String())
	}

	if t.Cfg.Server.GRPCListenPort == 0 {
		t.Cfg.Server.GRPCListenPort = portFromAddr(t.Server.GRPCListenAddr().String())
	}

	return s, nil
}

func portFromAddr(addr string) int {
	parts := strings.Split(addr, ":")
	port := parts[len(parts)-1]
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return portNumber
}

func (t *Loki) initInternalServer() (services.Service, error) {
	// Loki handles signals on its own.
	DisableSignalHandling(&t.Cfg.InternalServer.Config)
	serv, err := server.New(t.Cfg.InternalServer.Config)
	if err != nil {
		return nil, err
	}

	t.InternalServer = serv

	servicesToWaitFor := func() []services.Service {
		svs := []services.Service(nil)
		for m, s := range t.serviceMap {
			// Server should not wait for itself.
			if m != InternalServer {
				svs = append(svs, s)
			}
		}
		return svs
	}

	s := NewServerService(t.InternalServer, servicesToWaitFor)

	return s, nil
}

func (t *Loki) initRing() (_ services.Service, err error) {
	t.ring, err = ring.New(t.Cfg.Ingester.LifecyclerConfig.RingConfig, "ingester", ingester.RingKey, util_log.Logger, prometheus.WrapRegistererWithPrefix(t.Cfg.MetricsNamespace+"_", prometheus.DefaultRegisterer))
	if err != nil {
		return
	}
	t.Server.HTTP.Path("/ring").Methods("GET", "POST").Handler(t.ring)

	if t.Cfg.InternalServer.Enable {
		t.InternalServer.HTTP.Path("/ring").Methods("GET", "POST").Handler(t.ring)
	}
	return t.ring, nil
}

func (t *Loki) initRuntimeConfig() (services.Service, error) {
	if len(t.Cfg.RuntimeConfig.LoadPath) == 0 {
		if len(t.Cfg.LimitsConfig.PerTenantOverrideConfig) != 0 {
			t.Cfg.RuntimeConfig.LoadPath = []string{t.Cfg.LimitsConfig.PerTenantOverrideConfig}
		}
		t.Cfg.RuntimeConfig.ReloadPeriod = time.Duration(t.Cfg.LimitsConfig.PerTenantOverridePeriod)
	}

	if len(t.Cfg.RuntimeConfig.LoadPath) == 0 {
		// no need to initialize module if load path is empty
		return nil, nil
	}

	t.Cfg.RuntimeConfig.Loader = loadRuntimeConfig

	// make sure to set default limits before we start loading configuration into memory
	validation.SetDefaultLimitsForYAMLUnmarshalling(t.Cfg.LimitsConfig)
	runtime.SetDefaultLimitsForYAMLUnmarshalling(t.Cfg.OperationalConfig)

	var err error
	t.runtimeConfig, err = runtimeconfig.New(t.Cfg.RuntimeConfig, "loki", prometheus.WrapRegistererWithPrefix("loki_", prometheus.DefaultRegisterer), util_log.Logger)
	t.TenantLimits = newtenantLimitsFromRuntimeConfig(t.runtimeConfig)

	// Update config fields using runtime config. Only if multiKV is used for given ring these returned functions will be
	// called and register the listener.

	// By doing the initialization here instead of per-module init function, we avoid the problem
	// of projects based on Loki forgetting the wiring if they override module's init method (they also don't have access to private symbols).
	t.Cfg.CompactorConfig.CompactorRing.KVStore.Multi.ConfigProvider = multiClientRuntimeConfigChannel(t.runtimeConfig)
	t.Cfg.Distributor.DistributorRing.KVStore.Multi.ConfigProvider = multiClientRuntimeConfigChannel(t.runtimeConfig)
	t.Cfg.IndexGateway.Ring.KVStore.Multi.ConfigProvider = multiClientRuntimeConfigChannel(t.runtimeConfig)
	t.Cfg.Ingester.LifecyclerConfig.RingConfig.KVStore.Multi.ConfigProvider = multiClientRuntimeConfigChannel(t.runtimeConfig)
	t.Cfg.QueryScheduler.SchedulerRing.KVStore.Multi.ConfigProvider = multiClientRuntimeConfigChannel(t.runtimeConfig)
	t.Cfg.Ruler.Ring.KVStore.Multi.ConfigProvider = multiClientRuntimeConfigChannel(t.runtimeConfig)
	t.Cfg.IngestLimits.LifecyclerConfig.RingConfig.KVStore.Multi.ConfigProvider = multiClientRuntimeConfigChannel(t.runtimeConfig)
	t.Cfg.IngestLimitsFrontend.LifecyclerConfig.RingConfig.KVStore.Multi.ConfigProvider = multiClientRuntimeConfigChannel(t.runtimeConfig)

	return t.runtimeConfig, err
}

func (t *Loki) initOverrides() (_ services.Service, err error) {
	if t.Cfg.LimitsConfig.IndexGatewayShardSize == 0 {
		t.Cfg.LimitsConfig.IndexGatewayShardSize = t.Cfg.IndexGateway.Ring.ReplicationFactor
	}
	t.Overrides, err = validation.NewOverrides(t.Cfg.LimitsConfig, t.TenantLimits)
	// overrides are not a service, since they don't have any operational state.
	return nil, err
}

func (t *Loki) initOverridesExporter() (services.Service, error) {
	if t.Cfg.isTarget(OverridesExporter) && t.TenantLimits == nil || t.Overrides == nil {
		// This target isn't enabled by default ("all") and requires per-tenant limits to run.
		return nil, errors.New("overrides-exporter has been enabled, but no runtime configuration file was configured")
	}

	exporter := validation.NewOverridesExporter(t.Overrides)
	prometheus.MustRegister(exporter)

	// The overrides-exporter has no state and reads overrides for runtime configuration each time it
	// is collected so there is no need to return any service.
	return nil, nil
}

func (t *Loki) initTenantConfigs() (_ services.Service, err error) {
	t.tenantConfigs, err = runtime.NewTenantConfigs(newTenantConfigProvider(t.runtimeConfig))
	// tenantConfigs are not a service, since they don't have any operational state.
	return nil, err
}

func (t *Loki) initDistributor() (services.Service, error) {
	t.Cfg.Distributor.KafkaConfig = t.Cfg.KafkaConfig

	if t.Cfg.Distributor.KafkaEnabled && !t.Cfg.Ingester.KafkaIngestion.Enabled {
		return nil, errors.New("kafka is enabled in distributor but not in ingester")
	}

	var err error
	logger := log.With(util_log.Logger, "component", "distributor")
	t.distributor, err = distributor.New(
		t.Cfg.Distributor,
		t.Cfg.Ingester,
		t.Cfg.IngesterClient,
		t.tenantConfigs,
		t.ring,
		t.partitionRing,
		t.Overrides,
		prometheus.DefaultRegisterer,
		t.Cfg.MetricsNamespace,
		t.Tee,
		t.UsageTracker,
		t.Cfg.IngestLimitsFrontendClient,
		t.ingestLimitsFrontendRing,
		t.Cfg.IngestLimits.NumPartitions,
		logger,
	)
	if err != nil {
		return nil, err
	}

	if t.PushParserWrapper != nil {
		t.distributor.RequestParserWrapper = t.PushParserWrapper
	}

	// Register the distributor to receive Push requests over GRPC
	// EXCEPT when running with `-target=all` or `-target=` contains `ingester`
	if !t.Cfg.isTarget(All) && !t.Cfg.isTarget(Write) && !t.Cfg.isTarget(Ingester) {
		logproto.RegisterPusherServer(t.Server.GRPC, t.distributor)
	}

	httpPushHandlerMiddleware := middleware.Merge(
		serverutil.RecoveryHTTPMiddleware,
		t.HTTPAuthMiddleware,
	)

	lokiPushHandler := httpPushHandlerMiddleware.Wrap(http.HandlerFunc(t.distributor.PushHandler))
	otlpPushHandler := httpPushHandlerMiddleware.Wrap(http.HandlerFunc(t.distributor.OTLPPushHandler))

	t.Server.HTTP.Path("/distributor/ring").Methods("GET", "POST").Handler(t.distributor)

	if t.Cfg.InternalServer.Enable {
		t.InternalServer.HTTP.Path("/distributor/ring").Methods("GET", "POST").Handler(t.distributor)
	}

	t.Server.HTTP.Path("/api/prom/push").Methods("POST").Handler(lokiPushHandler)
	t.Server.HTTP.Path("/loki/api/v1/push").Methods("POST").Handler(lokiPushHandler)
	t.Server.HTTP.Path("/otlp/v1/logs").Methods("POST").Handler(otlpPushHandler)
	return t.distributor, nil
}

func (t *Loki) initIngestLimitsRing() (_ services.Service, err error) {
	if !t.Cfg.IngestLimits.Enabled {
		return nil, nil
	}

	reg := prometheus.WrapRegistererWithPrefix(t.Cfg.MetricsNamespace+"_", prometheus.DefaultRegisterer)

	t.ingestLimitsRing, err = ring.New(
		t.Cfg.IngestLimits.LifecyclerConfig.RingConfig,
		limits.RingName,
		limits.RingKey,
		util_log.Logger,
		reg,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create %s ring: %w", limits.RingName, err)
	}

	t.Server.HTTP.Path("/ingest-limits/ring").Methods("GET", "POST").Handler(t.ingestLimitsRing)
	if t.Cfg.InternalServer.Enable {
		t.InternalServer.HTTP.Path("/ingest-limits/ring").Methods("GET", "POST").Handler(t.ingestLimitsRing)
	}

	return t.ingestLimitsRing, nil
}

func (t *Loki) initIngestLimits() (services.Service, error) {
	if !t.Cfg.IngestLimits.Enabled {
		return nil, nil
	}

	t.Cfg.IngestLimits.LifecyclerConfig.ListenPort = t.Cfg.Server.GRPCListenPort
	t.Cfg.IngestLimits.KafkaConfig = t.Cfg.KafkaConfig

	ingestLimits, err := limits.New(
		t.Cfg.IngestLimits,
		t.Overrides,
		util_log.Logger,
		prometheus.DefaultRegisterer,
	)
	if err != nil {
		return nil, err
	}
	t.ingestLimits = ingestLimits

	limitsproto.RegisterIngestLimitsServer(t.Server.GRPC, ingestLimits)

	// Register HTTP handler for metadata
	t.Server.HTTP.Path("/ingest-limits/usage/{tenant}").Methods("GET").Handler(ingestLimits)

	return ingestLimits, nil
}

func (t *Loki) initIngestLimitsFrontendRing() (_ services.Service, err error) {
	if !t.Cfg.IngestLimits.Enabled {
		return nil, nil
	}

	reg := prometheus.WrapRegistererWithPrefix(t.Cfg.MetricsNamespace+"_", prometheus.DefaultRegisterer)

	if t.ingestLimitsFrontendRing, err = ring.New(
		t.Cfg.IngestLimitsFrontend.LifecyclerConfig.RingConfig,
		limits_frontend.RingName,
		limits_frontend.RingKey,
		util_log.Logger,
		reg,
	); err != nil {
		return nil, fmt.Errorf("failed to create %s ring: %w", limits_frontend.RingName, err)
	}

	t.Server.HTTP.Path("/ingest-limits-frontend/ring").
		Methods("GET", "POST").
		Handler(t.ingestLimitsFrontendRing)
	if t.Cfg.InternalServer.Enable {
		t.InternalServer.HTTP.Path("/ingest-limits-frontend/ring").
			Methods("GET", "POST").
			Handler(t.ingestLimitsFrontendRing)
	}

	return t.ingestLimitsFrontendRing, nil
}

func (t *Loki) initIngestLimitsFrontend() (services.Service, error) {
	if !t.Cfg.IngestLimits.Enabled {
		return nil, nil
	}

	// Members of the ring are expected to listen on their gRPC server port.
	t.Cfg.IngestLimitsFrontend.LifecyclerConfig.ListenPort = t.Cfg.Server.GRPCListenPort

	logger := log.With(util_log.Logger, "component", "ingest-limits-frontend")
	ingestLimitsFrontend, err := limits_frontend.New(
		t.Cfg.IngestLimitsFrontend,
		limits.RingName,
		t.ingestLimitsRing,
		logger,
		prometheus.DefaultRegisterer,
	)
	if err != nil {
		return nil, err
	}
	t.ingestLimitsFrontend = ingestLimitsFrontend
	limitsproto.RegisterIngestLimitsFrontendServer(t.Server.GRPC, ingestLimitsFrontend)

	// Register HTTP handler to check if a tenant exceeds limits
	// Returns a JSON response for the frontend to display which
	// streams are rejected.
	t.Server.HTTP.Path("/ingest-limits/exceeds-limits").Methods("POST").Handler(ingestLimitsFrontend)

	return ingestLimitsFrontend, nil
}

// initCodec sets the codec used to encode and decode requests.
func (t *Loki) initCodec() (services.Service, error) {
	t.Codec = queryrange.DefaultCodec
	return nil, nil
}

func (t *Loki) getQuerierStore() (querier.Store, error) {
	if !t.Cfg.DataObj.Querier.Enabled {
		return t.Store, nil
	}

	// verify that there's no schema with a date after the dataobj querier from date
	for _, schema := range t.Cfg.SchemaConfig.Configs {
		if schema.From.After(t.Cfg.DataObj.Querier.From) {
			return nil, fmt.Errorf("dataobj querier From should be after the last schema date")
		}
	}

	store, err := t.createDataObjBucket("dataobj-querier")
	if err != nil {
		return nil, err
	}

	logger := log.With(util_log.Logger, "component", "dataobj-querier")
	storeCombiner := querier.NewStoreCombiner([]querier.StoreConfig{
		{
			Store: dataobjquerier.NewStore(store, logger, metastore.NewObjectMetastore(store, logger, prometheus.DefaultRegisterer)),
			From:  t.Cfg.DataObj.Querier.From.Time,
		},
		{
			Store: t.Store,
		},
	})

	return storeCombiner, nil
}

func (t *Loki) initQuerier() (services.Service, error) {
	logger := log.With(util_log.Logger, "component", "querier")
	if t.Cfg.Ingester.QueryStoreMaxLookBackPeriod != 0 {
		t.Cfg.Querier.IngesterQueryStoreMaxLookback = t.Cfg.Ingester.QueryStoreMaxLookBackPeriod
	}

	// Use Pattern ingester RetainFor value to determine when to query pattern ingesters
	t.Cfg.Querier.QueryPatternIngestersWithin = t.Cfg.Pattern.RetainFor

	// Querier worker's max concurrent must be the same as the querier setting
	t.Cfg.Worker.MaxConcurrent = t.Cfg.Querier.MaxConcurrent
	deleteStore, err := t.deleteRequestsClient("querier", t.Overrides)
	if err != nil {
		return nil, err
	}

	querierStore, err := t.getQuerierStore()
	if err != nil {
		return nil, err
	}

	t.Querier, err = querier.New(t.Cfg.Querier, querierStore, t.ingesterQuerier, t.Overrides, deleteStore, logger)
	if err != nil {
		return nil, err
	}

	if t.Cfg.Pattern.Enabled {
		patternQuerier, err := pattern.NewIngesterQuerier(t.Cfg.Pattern, t.PatternRingClient, t.Cfg.MetricsNamespace, prometheus.DefaultRegisterer, util_log.Logger)
		if err != nil {
			return nil, err
		}
		t.Querier.WithPatternQuerier(patternQuerier)
	}

	if t.Cfg.Querier.MultiTenantQueriesEnabled {
		t.Querier = querier.NewMultiTenantQuerier(t.Querier, util_log.Logger)
	}

	querierWorkerServiceConfig := querier.WorkerServiceConfig{
		AllEnabled:            t.Cfg.isTarget(All),
		ReadEnabled:           t.Cfg.isTarget(Read),
		GrpcListenAddress:     t.Cfg.Server.GRPCListenAddress,
		GrpcListenPort:        t.Cfg.Server.GRPCListenPort,
		QuerierWorkerConfig:   &t.Cfg.Worker,
		QueryFrontendEnabled:  t.Cfg.isTarget(QueryFrontend),
		QuerySchedulerEnabled: t.Cfg.isTarget(QueryScheduler),
		SchedulerRing:         scheduler.SafeReadRing(t.Cfg.QueryScheduler, t.querySchedulerRingManager),
	}

	toMerge := []middleware.Interface{
		httpreq.ExtractQueryMetricsMiddleware(),
		httpreq.ExtractQueryTagsMiddleware(),
		httpreq.PropagateHeadersMiddleware(httpreq.LokiEncodingFlagsHeader, httpreq.LokiDisablePipelineWrappersHeader),
		serverutil.RecoveryHTTPMiddleware,
		t.HTTPAuthMiddleware,
		serverutil.NewPrepopulateMiddleware(),
		serverutil.ResponseJSONMiddleware(),
	}

	var store objstore.Bucket
	if t.Cfg.Querier.Engine.EnableV2Engine {
		store, err = t.createDataObjBucket("dataobj-querier")
		if err != nil {
			return nil, err
		}
	}

	t.querierAPI = querier.NewQuerierAPI(t.Cfg.Querier, t.Querier, t.Overrides, store, prometheus.DefaultRegisterer, logger)

	indexStatsHTTPMiddleware := querier.WrapQuerySpanAndTimeout("query.IndexStats", t.Overrides)
	indexShardsHTTPMiddleware := querier.WrapQuerySpanAndTimeout("query.IndexShards", t.Overrides)
	volumeHTTPMiddleware := querier.WrapQuerySpanAndTimeout("query.VolumeInstant", t.Overrides)
	volumeRangeHTTPMiddleware := querier.WrapQuerySpanAndTimeout("query.VolumeRange", t.Overrides)
	seriesHTTPMiddleware := querier.WrapQuerySpanAndTimeout("query.Series", t.Overrides)

	if t.supportIndexDeleteRequest() && t.Cfg.CompactorConfig.RetentionEnabled {
		toMerge = append(
			toMerge,
			queryrangebase.CacheGenNumberHeaderSetterMiddleware(t.cacheGenerationLoader),
		)

		indexStatsHTTPMiddleware = middleware.Merge(
			queryrangebase.CacheGenNumberHeaderSetterMiddleware(t.cacheGenerationLoader),
			indexStatsHTTPMiddleware,
		)

		volumeHTTPMiddleware = middleware.Merge(
			queryrangebase.CacheGenNumberHeaderSetterMiddleware(t.cacheGenerationLoader),
			volumeHTTPMiddleware,
		)

		volumeRangeHTTPMiddleware = middleware.Merge(
			queryrangebase.CacheGenNumberHeaderSetterMiddleware(t.cacheGenerationLoader),
			volumeRangeHTTPMiddleware,
		)
	}

	labelsHTTPMiddleware := querier.WrapQuerySpanAndTimeout("query.Label", t.Overrides)

	if t.Cfg.Querier.PerRequestLimitsEnabled {
		toMerge = append(
			toMerge,
			querylimits.NewQueryLimitsMiddleware(log.With(util_log.Logger, "component", "query-limits-middleware")),
		)
		labelsHTTPMiddleware = middleware.Merge(
			querylimits.NewQueryLimitsMiddleware(log.With(util_log.Logger, "component", "query-limits-middleware")),
			labelsHTTPMiddleware,
		)
	}

	httpMiddleware := middleware.Merge(toMerge...)

	handler := querier.NewQuerierHandler(t.querierAPI)
	httpHandler := querier.NewQuerierHTTPHandler(handler)

	// If the querier is running standalone without the query-frontend or query-scheduler, we must register the internal
	// HTTP handler externally (as it's the only handler that needs to register on querier routes) and provide the
	// external Loki Server HTTP handler to the frontend worker to ensure requests it processes use the default
	// middleware instrumentation.
	if querierWorkerServiceConfig.QuerierRunningStandalone() {
		labelsHTTPMiddleware = middleware.Merge(httpMiddleware, labelsHTTPMiddleware)
		indexStatsHTTPMiddleware = middleware.Merge(httpMiddleware, indexStatsHTTPMiddleware)
		indexShardsHTTPMiddleware = middleware.Merge(httpMiddleware, indexShardsHTTPMiddleware)
		volumeHTTPMiddleware = middleware.Merge(httpMiddleware, volumeHTTPMiddleware)
		volumeRangeHTTPMiddleware = middleware.Merge(httpMiddleware, volumeRangeHTTPMiddleware)
		seriesHTTPMiddleware = middleware.Merge(httpMiddleware, seriesHTTPMiddleware)

		// First, register the internal querier handler with the external HTTP server
		router := t.Server.HTTP
		if t.Cfg.Server.PathPrefix != "" {
			router = router.PathPrefix(t.Cfg.Server.PathPrefix).Subrouter()
		}

		router.Path("/loki/api/v1/query_range").Methods("GET", "POST").Handler(
			middleware.Merge(
				httpMiddleware,
				querier.WrapQuerySpanAndTimeout("query.RangeQuery", t.Overrides),
			).Wrap(httpHandler),
		)

		router.Path("/loki/api/v1/query").Methods("GET", "POST").Handler(
			middleware.Merge(
				httpMiddleware,
				querier.WrapQuerySpanAndTimeout("query.InstantQuery", t.Overrides),
			).Wrap(httpHandler),
		)

		router.Path("/loki/api/v1/label").Methods("GET", "POST").Handler(labelsHTTPMiddleware.Wrap(httpHandler))
		router.Path("/loki/api/v1/labels").Methods("GET", "POST").Handler(labelsHTTPMiddleware.Wrap(httpHandler))
		router.Path("/loki/api/v1/label/{name}/values").Methods("GET", "POST").Handler(labelsHTTPMiddleware.Wrap(httpHandler))

		router.Path("/loki/api/v1/series").Methods("GET", "POST").Handler(seriesHTTPMiddleware.Wrap(httpHandler))
		router.Path("/loki/api/v1/index/stats").Methods("GET", "POST").Handler(indexStatsHTTPMiddleware.Wrap(httpHandler))
		router.Path("/loki/api/v1/index/shards").Methods("GET", "POST").Handler(indexShardsHTTPMiddleware.Wrap(httpHandler))
		router.Path("/loki/api/v1/index/volume").Methods("GET", "POST").Handler(volumeHTTPMiddleware.Wrap(httpHandler))
		router.Path("/loki/api/v1/index/volume_range").Methods("GET", "POST").Handler(volumeRangeHTTPMiddleware.Wrap(httpHandler))
		router.Path("/loki/api/v1/patterns").Methods("GET", "POST").Handler(httpHandler)

		router.Path("/api/prom/query").Methods("GET", "POST").Handler(
			middleware.Merge(
				httpMiddleware,
				querier.WrapQuerySpanAndTimeout("query.LogQuery", t.Overrides),
			).Wrap(httpHandler),
		)

		router.Path("/api/prom/label").Methods("GET", "POST").Handler(labelsHTTPMiddleware.Wrap(httpHandler))
		router.Path("/api/prom/label/{name}/values").Methods("GET", "POST").Handler(labelsHTTPMiddleware.Wrap(httpHandler))
		router.Path("/api/prom/series").Methods("GET", "POST").Handler(seriesHTTPMiddleware.Wrap(httpHandler))
	}

	// We always want to register tail routes externally, tail requests are different from normal queries, they
	// are HTTP requests that get upgraded to websocket requests and need to be handled/kept open by the Queriers.
	// The frontend has code to proxy these requests, however when running in the same processes
	// (such as target=All or target=Read) we don't want the frontend to proxy and instead we want the Queriers
	// to directly register these routes.
	// In practice this means we always want the queriers to register the tail routes externally, when a querier
	// is standalone ALL routes are registered externally, and when it's in the same process as a frontend,
	// we disable the proxying of the tail routes in initQueryFrontend() and we still want these routes regiestered
	// on the external router.
	tailQuerier := tail.NewQuerier(t.ingesterQuerier, t.Querier, deleteStore, t.Overrides, t.Cfg.Querier.TailMaxDuration, tail.NewMetrics(prometheus.DefaultRegisterer), log.With(util_log.Logger, "component", "tail-querier"))
	t.Server.HTTP.Path("/loki/api/v1/tail").Methods("GET", "POST").Handler(httpMiddleware.Wrap(http.HandlerFunc(tailQuerier.TailHandler)))
	t.Server.HTTP.Path("/api/prom/tail").Methods("GET", "POST").Handler(httpMiddleware.Wrap(http.HandlerFunc(tailQuerier.TailHandler)))

	internalMiddlewares := []queryrangebase.Middleware{
		serverutil.RecoveryMiddleware,
		queryrange.Instrument{Metrics: t.Metrics},
		queryrange.Tracer{},
	}
	if t.supportIndexDeleteRequest() && t.Cfg.CompactorConfig.RetentionEnabled {
		internalMiddlewares = append(
			internalMiddlewares,
			queryrangebase.CacheGenNumberContextSetterMiddleware(t.cacheGenerationLoader),
		)
	}
	internalHandler := queryrangebase.MergeMiddlewares(internalMiddlewares...).Wrap(handler)

	svc, err := querier.InitWorkerService(
		logger,
		querierWorkerServiceConfig,
		prometheus.DefaultRegisterer,
		internalHandler,
		t.Codec,
	)
	if err != nil {
		return nil, err
	}

	if svc != nil {
		svc.AddListener(deleteRequestsStoreListener(deleteStore))
	}
	return svc, nil
}

func (t *Loki) initIngester() (_ services.Service, err error) {
	logger := log.With(util_log.Logger, "component", "ingester")
	t.Cfg.Ingester.LifecyclerConfig.ListenPort = t.Cfg.Server.GRPCListenPort
	t.Cfg.Ingester.KafkaIngestion.KafkaConfig = t.Cfg.KafkaConfig

	if t.Cfg.Ingester.ShutdownMarkerPath == "" && t.Cfg.Common.PathPrefix != "" {
		t.Cfg.Ingester.ShutdownMarkerPath = t.Cfg.Common.PathPrefix
	}
	if t.Cfg.Ingester.ShutdownMarkerPath == "" {
		level.Warn(util_log.Logger).Log("msg", "The config setting shutdown marker path is not set. The /ingester/prepare_shutdown endpoint won't work")
	}

	t.Ingester, err = ingester.New(t.Cfg.Ingester, t.Cfg.IngesterClient, t.Store, t.Overrides, t.tenantConfigs, prometheus.DefaultRegisterer, t.Cfg.Distributor.WriteFailuresLogging, t.Cfg.MetricsNamespace, logger, t.UsageTracker, t.ring, t.PartitionRingWatcher)
	if err != nil {
		return
	}

	if t.Cfg.Ingester.Wrapper != nil {
		t.Ingester = t.Cfg.Ingester.Wrapper.Wrap(t.Ingester)
	}

	logproto.RegisterPusherServer(t.Server.GRPC, t.Ingester)
	logproto.RegisterQuerierServer(t.Server.GRPC, t.Ingester)
	logproto.RegisterStreamDataServer(t.Server.GRPC, t.Ingester)

	httpMiddleware := middleware.Merge(
		serverutil.RecoveryHTTPMiddleware,
	)
	t.Server.HTTP.Methods("GET", "POST").Path("/flush").Handler(
		httpMiddleware.Wrap(http.HandlerFunc(t.Ingester.FlushHandler)),
	)
	t.Server.HTTP.Methods("POST", "GET", "DELETE").Path("/ingester/prepare_shutdown").Handler(
		httpMiddleware.Wrap(http.HandlerFunc(t.Ingester.PrepareShutdown)),
	)
	t.Server.HTTP.Methods("POST", "GET", "DELETE").Path("/ingester/prepare_partition_downscale").Handler(
		httpMiddleware.Wrap(http.HandlerFunc(t.Ingester.PreparePartitionDownscaleHandler)),
	)
	t.Server.HTTP.Methods("POST", "GET").Path("/ingester/shutdown").Handler(
		httpMiddleware.Wrap(http.HandlerFunc(t.Ingester.ShutdownHandler)),
	)
	return t.Ingester, nil
}

func (t *Loki) initPatternIngester() (_ services.Service, err error) {
	if !t.Cfg.Pattern.Enabled {
		return nil, nil
	}
	t.Cfg.Pattern.LifecyclerConfig.ListenPort = t.Cfg.Server.GRPCListenPort
	t.PatternIngester, err = pattern.New(
		t.Cfg.Pattern,
		t.Overrides,
		t.PatternRingClient,
		t.Cfg.MetricsNamespace,
		prometheus.DefaultRegisterer,
		util_log.Logger,
	)
	if err != nil {
		return nil, err
	}
	logproto.RegisterPatternServer(t.Server.GRPC, t.PatternIngester)

	t.Server.HTTP.Path("/pattern/ring").Methods("GET", "POST").Handler(t.PatternIngester)

	if t.Cfg.InternalServer.Enable {
		t.InternalServer.HTTP.Path("/pattern/ring").Methods("GET", "POST").Handler(t.PatternIngester)
	}
	return t.PatternIngester, nil
}

func (t *Loki) initPatternRingClient() (_ services.Service, err error) {
	if !t.Cfg.Pattern.Enabled {
		return nil, nil
	}
	ringClient, err := pattern.NewRingClient(t.Cfg.Pattern, t.Cfg.MetricsNamespace, prometheus.DefaultRegisterer, util_log.Logger)
	if err != nil {
		return nil, err
	}
	t.PatternRingClient = ringClient
	return ringClient, nil
}

func (t *Loki) initPatternIngesterTee() (services.Service, error) {
	logger := util_log.Logger

	if !t.Cfg.Pattern.Enabled {
		_ = level.Debug(logger).Log("msg", " pattern ingester tee service disabled")
		return nil, nil
	}
	_ = level.Debug(logger).Log("msg", "initializing pattern ingester tee service...")

	svc, err := pattern.NewTeeService(
		t.Cfg.Pattern,
		t.Overrides,
		t.PatternRingClient,
		t.tenantConfigs,
		t.Cfg.MetricsNamespace,
		prometheus.DefaultRegisterer,
		logger,
	)
	if err != nil {
		return nil, err
	}

	t.Tee = distributor.WrapTee(t.Tee, svc)

	return services.NewBasicService(
		svc.Start,
		func(_ context.Context) error {
			svc.WaitUntilDone()
			return nil
		},
		func(_ error) error {
			svc.WaitUntilDone()
			return nil
		},
	), nil
}

func (t *Loki) initTableManager() (services.Service, error) {
	level.Warn(util_log.Logger).Log("msg", "table manager is deprecated. Consider migrating to tsdb index which relies on a compactor instead.")

	err := t.Cfg.SchemaConfig.Load()
	if err != nil {
		return nil, err
	}

	// Assume the newest config is the one to use
	lastConfig := &t.Cfg.SchemaConfig.Configs[len(t.Cfg.SchemaConfig.Configs)-1]

	if (t.Cfg.TableManager.ChunkTables.WriteScale.Enabled ||
		t.Cfg.TableManager.IndexTables.WriteScale.Enabled ||
		t.Cfg.TableManager.ChunkTables.InactiveWriteScale.Enabled ||
		t.Cfg.TableManager.IndexTables.InactiveWriteScale.Enabled ||
		t.Cfg.TableManager.ChunkTables.ReadScale.Enabled ||
		t.Cfg.TableManager.IndexTables.ReadScale.Enabled ||
		t.Cfg.TableManager.ChunkTables.InactiveReadScale.Enabled ||
		t.Cfg.TableManager.IndexTables.InactiveReadScale.Enabled) &&
		t.Cfg.StorageConfig.AWSStorageConfig.Metrics.URL == "" {
		level.Error(util_log.Logger).Log("msg", "WriteScale is enabled but no Metrics URL has been provided")
		os.Exit(1)
	}

	reg := prometheus.WrapRegistererWith(prometheus.Labels{"component": "table-manager-store"}, prometheus.DefaultRegisterer)
	tableClient, err := storage.NewTableClient(lastConfig.IndexType, "table-manager", *lastConfig, t.Cfg.StorageConfig, t.ClientMetrics, reg, util_log.Logger)
	if err != nil {
		return nil, err
	}

	bucketClient, err := storage.NewBucketClient(t.Cfg.StorageConfig)
	util_log.CheckFatal("initializing bucket client", err, util_log.Logger)

	t.tableManager, err = index.NewTableManager(t.Cfg.TableManager, t.Cfg.SchemaConfig, maxChunkAgeForTableManager, tableClient, bucketClient, nil, prometheus.DefaultRegisterer, util_log.Logger)
	if err != nil {
		return nil, err
	}

	return t.tableManager, nil
}

func (t *Loki) initStore() (services.Service, error) {
	// Set configs pertaining to object storage based indices
	if config.UsingObjectStorageIndex(t.Cfg.SchemaConfig.Configs) {
		t.updateConfigForShipperStore()
		err := t.setupAsyncStore()
		if err != nil {
			return nil, err
		}
	}

	store, err := storage.NewStore(t.Cfg.StorageConfig, t.Cfg.ChunkStoreConfig, t.Cfg.SchemaConfig, t.Overrides, t.ClientMetrics, prometheus.DefaultRegisterer, util_log.Logger, t.Cfg.MetricsNamespace)
	if err != nil {
		return nil, err
	}

	t.Store = store

	return services.NewIdleService(nil, func(_ error) error {
		t.Store.Stop()
		return nil
	}), nil
}

func (t *Loki) initBloomStore() (services.Service, error) {
	// BloomStore is a dependency of IndexGateway and Bloom Planner & Builder.
	// Do not instantiate store and do not create a service if neither ar enabled.
	if !t.Cfg.BloomGateway.Enabled && !t.Cfg.BloomBuild.Enabled {
		return nil, nil
	}

	if !config.UsingObjectStorageIndex(t.Cfg.SchemaConfig.Configs) {
		return nil, errors.New("not using shipper index type")
	}

	t.updateConfigForShipperStore()

	var err error
	logger := log.With(util_log.Logger, "component", "bloomstore")

	reg := prometheus.DefaultRegisterer
	bsCfg := t.Cfg.StorageConfig.BloomShipperConfig

	var metasCache cache.Cache
	if (t.Cfg.isTarget(IndexGateway) || t.Cfg.isTarget(Backend)) && cache.IsCacheConfigured(bsCfg.MetasCache) {
		metasCache, err = cache.New(bsCfg.MetasCache, reg, logger, stats.BloomMetasCache, constants.Loki)

		// always enable LRU cache
		lruCfg := bsCfg.MetasLRUCache
		lruCfg.Enabled = true
		lruCfg.PurgeInterval = 1 * time.Minute
		lruCache := cache.NewEmbeddedCache("inmemory-metas-lru", lruCfg, reg, logger, stats.BloomMetasCache)

		metasCache = cache.NewTiered([]cache.Cache{lruCache, metasCache})
		if err != nil {
			return nil, fmt.Errorf("failed to create metas cache: %w", err)
		}
	} else {
		level.Info(logger).Log("msg", "no metas cache configured")
	}

	blocksCache := bloomshipper.NewFsBlocksCache(bsCfg.BlocksCache, reg, logger)
	if err = bloomshipper.LoadBlocksDirIntoCache(bsCfg.WorkingDirectory, blocksCache, logger); err != nil {
		level.Warn(logger).Log("msg", "failed to preload blocks cache", "err", err)
	}

	var pageAllocator mempool.Allocator

	// Set global BloomPageAllocator variable
	switch bsCfg.MemoryManagement.BloomPageAllocationType {
	case "simple":
		pageAllocator = &mempool.SimpleHeapAllocator{}
	case "dynamic":
		// sync buffer pool for bloom pages
		// 128KB 256KB 512KB 1MB 2MB 4MB 8MB 16MB 32MB 64MB 128MB
		pageAllocator = mempool.NewBytePoolAllocator(128<<10, 128<<20, 2)
	case "fixed":
		pageAllocator = mempool.New("bloom-page-pool", bsCfg.MemoryManagement.BloomPageMemPoolBuckets, reg)
	default:
		// should not happen as the type is validated upfront
		return nil, fmt.Errorf("failed to create bloom store: invalid allocator type")
	}

	t.BloomStore, err = bloomshipper.NewBloomStore(t.Cfg.SchemaConfig.Configs, t.Cfg.StorageConfig, t.ClientMetrics, metasCache, blocksCache, pageAllocator, reg, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create bloom store: %w", err)
	}

	return services.NewIdleService(nil, func(_ error) error {
		t.BloomStore.Stop()
		return nil
	}), nil
}

func (t *Loki) updateConfigForShipperStore() {
	// Always set these configs
	t.Cfg.StorageConfig.BoltDBShipperConfig.IndexGatewayClientConfig.Mode = t.Cfg.IndexGateway.Mode
	t.Cfg.StorageConfig.TSDBShipperConfig.IndexGatewayClientConfig.Mode = t.Cfg.IndexGateway.Mode

	if t.Cfg.IndexGateway.Mode == indexgateway.RingMode {
		t.Cfg.StorageConfig.BoltDBShipperConfig.IndexGatewayClientConfig.Ring = t.indexGatewayRingManager.Ring
		t.Cfg.StorageConfig.TSDBShipperConfig.IndexGatewayClientConfig.Ring = t.indexGatewayRingManager.Ring
	}

	t.Cfg.StorageConfig.BoltDBShipperConfig.IngesterName = t.Cfg.Ingester.LifecyclerConfig.ID
	t.Cfg.StorageConfig.TSDBShipperConfig.IngesterName = t.Cfg.Ingester.LifecyclerConfig.ID

	// If RF > 1 and current or upcoming index type is boltdb-shipper then disable index dedupe and write dedupe cache.
	// This is to ensure that index entries are replicated to all the boltdb files in ingesters flushing replicated data.
	if t.Cfg.Ingester.LifecyclerConfig.RingConfig.ReplicationFactor > 1 {
		t.Cfg.ChunkStoreConfig.DisableIndexDeduplication = true
		t.Cfg.ChunkStoreConfig.WriteDedupeCacheConfig = cache.Config{}
	}

	switch true {
	case t.Cfg.isTarget(Ingester), t.Cfg.isTarget(Write):
		// Use embedded cache for caching index in memory, this also significantly helps performance.
		t.Cfg.StorageConfig.IndexQueriesCacheConfig = cache.Config{
			EmbeddedCache: cache.EmbeddedCacheConfig{
				Enabled:   true,
				MaxSizeMB: 200,
				// This is a small hack to save some CPU cycles.
				// We check if the object is still valid after pulling it from cache using the IndexCacheValidity value
				// however it has to be deserialized to do so, setting the cache validity to some arbitrary amount less than the
				// IndexCacheValidity guarantees the Embedded cache will expire the object first which can be done without
				// having to deserialize the object.
				TTL: t.Cfg.StorageConfig.IndexCacheValidity - 1*time.Minute,
			},
		}

		// We do not want ingester to unnecessarily keep downloading files
		t.Cfg.StorageConfig.BoltDBShipperConfig.Mode = indexshipper.ModeWriteOnly
		t.Cfg.StorageConfig.BoltDBShipperConfig.IngesterDBRetainPeriod = shipperQuerierIndexUpdateDelay(t.Cfg.StorageConfig.IndexCacheValidity, t.Cfg.StorageConfig.BoltDBShipperConfig.ResyncInterval)

		t.Cfg.StorageConfig.TSDBShipperConfig.Mode = indexshipper.ModeWriteOnly
		t.Cfg.StorageConfig.TSDBShipperConfig.IngesterDBRetainPeriod = shipperQuerierIndexUpdateDelay(t.Cfg.StorageConfig.IndexCacheValidity, t.Cfg.StorageConfig.TSDBShipperConfig.ResyncInterval)

	case t.Cfg.isTarget(Querier), t.Cfg.isTarget(Ruler), t.Cfg.isTarget(Read), t.Cfg.isTarget(Backend), t.isModuleActive(IndexGateway), t.Cfg.isTarget(BloomPlanner), t.Cfg.isTarget(BloomBuilder):
		// We do not want query to do any updates to index
		t.Cfg.StorageConfig.BoltDBShipperConfig.Mode = indexshipper.ModeReadOnly
		t.Cfg.StorageConfig.TSDBShipperConfig.Mode = indexshipper.ModeReadOnly

	case t.Cfg.isTarget(BlockBuilder):
		// Blockbuilder handles index creation independently of the shipper.
		// TODO: introduce Disabled mode for boltdb shipper and set it here.
		t.Cfg.StorageConfig.BoltDBShipperConfig.Mode = indexshipper.ModeReadOnly
		t.Cfg.StorageConfig.TSDBShipperConfig.Mode = indexshipper.ModeDisabled

	default:
		// All other targets use the shipper store in RW mode
		t.Cfg.StorageConfig.BoltDBShipperConfig.Mode = indexshipper.ModeReadWrite
		t.Cfg.StorageConfig.BoltDBShipperConfig.IngesterDBRetainPeriod = shipperQuerierIndexUpdateDelay(t.Cfg.StorageConfig.IndexCacheValidity, t.Cfg.StorageConfig.BoltDBShipperConfig.ResyncInterval)
		t.Cfg.StorageConfig.TSDBShipperConfig.Mode = indexshipper.ModeReadWrite
		t.Cfg.StorageConfig.TSDBShipperConfig.IngesterDBRetainPeriod = shipperQuerierIndexUpdateDelay(t.Cfg.StorageConfig.IndexCacheValidity, t.Cfg.StorageConfig.TSDBShipperConfig.ResyncInterval)
	}
}

func (t *Loki) setupAsyncStore() error {
	var asyncStore bool

	shipperConfigIdx := config.ActivePeriodConfig(t.Cfg.SchemaConfig.Configs)
	iTy := t.Cfg.SchemaConfig.Configs[shipperConfigIdx].IndexType
	if iTy != types.BoltDBShipperType && iTy != types.TSDBType {
		shipperConfigIdx++
	}

	minIngesterQueryStoreDuration := shipperMinIngesterQueryStoreDuration(
		t.Cfg.Ingester.MaxChunkAge,
		shipperQuerierIndexUpdateDelay(
			t.Cfg.StorageConfig.IndexCacheValidity,
			shipperResyncInterval(t.Cfg.StorageConfig, t.Cfg.SchemaConfig.Configs),
		),
	)

	switch true {
	case t.Cfg.isTarget(Querier), t.Cfg.isTarget(Ruler), t.Cfg.isTarget(Read):
		// Do not use the AsyncStore if the querier is configured with QueryStoreOnly set to true
		if t.Cfg.Querier.QueryStoreOnly {
			break
		}

		// Use AsyncStore to query both ingesters local store and chunk store for store queries.
		// Only queriers should use the AsyncStore, it should never be used in ingesters.
		asyncStore = true

		// The legacy Read target includes the index gateway, so disable the index-gateway client in that configuration.
		if t.Cfg.LegacyReadTarget && t.Cfg.isTarget(Read) {
			t.Cfg.StorageConfig.BoltDBShipperConfig.IndexGatewayClientConfig.Disabled = true
			t.Cfg.StorageConfig.TSDBShipperConfig.IndexGatewayClientConfig.Disabled = true
		}
		// Backend target includes the index gateway
	case t.Cfg.isTarget(IndexGateway), t.Cfg.isTarget(Backend):
		// we want to use the actual storage when running the index-gateway, so we remove the Addr from the config
		t.Cfg.StorageConfig.BoltDBShipperConfig.IndexGatewayClientConfig.Disabled = true
		t.Cfg.StorageConfig.TSDBShipperConfig.IndexGatewayClientConfig.Disabled = true
	case t.Cfg.isTarget(All):
		// We want ingester to also query the store when using boltdb-shipper but only when running with target All.
		// We do not want to use AsyncStore otherwise it would start spiraling around doing queries over and over again to the ingesters and store.
		// ToDo: See if we can avoid doing this when not running loki in clustered mode.
		t.Cfg.Ingester.QueryStore = true

		mlb, err := calculateMaxLookBack(
			t.Cfg.SchemaConfig.Configs[shipperConfigIdx],
			t.Cfg.Ingester.QueryStoreMaxLookBackPeriod,
			minIngesterQueryStoreDuration,
		)
		if err != nil {
			return err
		}
		t.Cfg.Ingester.QueryStoreMaxLookBackPeriod = mlb
	}

	if asyncStore {
		t.Cfg.StorageConfig.EnableAsyncStore = true

		t.Cfg.StorageConfig.AsyncStoreConfig = storage.AsyncStoreCfg{
			IngesterQuerier: t.ingesterQuerier,
			QueryIngestersWithin: calculateAsyncStoreQueryIngestersWithin(
				t.Cfg.Querier.QueryIngestersWithin,
				minIngesterQueryStoreDuration,
			),
		}
	}

	return nil
}

func (t *Loki) initIngesterQuerier() (_ services.Service, err error) {
	logger := log.With(util_log.Logger, "component", "ingester-querier")

	t.ingesterQuerier, err = querier.NewIngesterQuerier(t.Cfg.Querier, t.Cfg.IngesterClient, t.ring, t.partitionRing, t.Overrides.IngestionPartitionsTenantShardSize, t.Cfg.MetricsNamespace, logger)
	if err != nil {
		return nil, err
	}

	return services.NewIdleService(nil, nil), nil
}

// Placeholder limits type to pass to cortex frontend
type disabledShuffleShardingLimits struct{}

func (disabledShuffleShardingLimits) MaxQueriersPerUser(_ string) uint { return 0 }

func (disabledShuffleShardingLimits) MaxQueryCapacity(_ string) float64 { return 0 }

// ingesterQueryOptions exists simply to avoid dependency cycles when using querier.Config directly in queryrange.NewMiddleware
type ingesterQueryOptions struct {
	querier.Config
}

func (i ingesterQueryOptions) QueryStoreOnly() bool {
	return i.Config.QueryStoreOnly
}

func (i ingesterQueryOptions) QueryIngestersWithin() time.Duration {
	return i.Config.QueryIngestersWithin
}

func (t *Loki) initQueryFrontendMiddleware() (_ services.Service, err error) {
	level.Debug(util_log.Logger).Log("msg", "initializing query frontend tripperware")

	schemas := t.Cfg.SchemaConfig
	// Adjust schema config to use constant sharding for the timerange of dataobj querier.
	if t.Cfg.DataObj.Querier.Enabled {
		schemas = schemas.Clone()
		schemas.Configs = append(schemas.Configs, t.Cfg.DataObj.Querier.PeriodConfig())
		sort.Slice(schemas.Configs, func(i, j int) bool {
			return schemas.Configs[i].From.UnixNano() < schemas.Configs[j].From.UnixNano()
		})
		for _, cfg := range schemas.Configs {
			level.Debug(util_log.Logger).Log("msg", "schema config", "from", cfg.From, "row_shards", cfg.RowShards, "index_type", cfg.IndexType, "object_store", cfg.ObjectType, "schema", cfg.Schema)
		}
	}

	middleware, stopper, err := queryrange.NewMiddleware(
		t.Cfg.QueryRange,
		t.Cfg.Querier.Engine,
		ingesterQueryOptions{t.Cfg.Querier},
		util_log.Logger,
		t.Overrides,
		schemas,
		t.cacheGenerationLoader, t.Cfg.CompactorConfig.RetentionEnabled,
		prometheus.DefaultRegisterer,
		t.Cfg.MetricsNamespace,
	)
	if err != nil {
		return
	}
	t.stopper = stopper
	t.QueryFrontEndMiddleware = middleware

	return services.NewIdleService(nil, nil), nil
}

func (t *Loki) initCacheGenerationLoader() (_ services.Service, err error) {
	var client generationnumber.CacheGenClient
	if t.supportIndexDeleteRequest() {
		compactorAddress, isGRPCAddress, err := t.compactorAddress()
		if err != nil {
			return nil, err
		}

		reg := prometheus.WrapRegistererWith(prometheus.Labels{"for": "cache_gen", "client_type": t.Cfg.Target.String()}, prometheus.DefaultRegisterer)
		if isGRPCAddress {
			client, err = compactorclient.NewGRPCClient(compactorAddress, t.Cfg.CompactorGRPCClient, reg)
			if err != nil {
				return nil, err
			}
		} else {
			client, err = compactorclient.NewHTTPClient(compactorAddress, t.Cfg.CompactorHTTPClient)
			if err != nil {
				return nil, err
			}
		}
	}

	t.cacheGenerationLoader = generationnumber.NewGenNumberLoader(client, prometheus.DefaultRegisterer)
	return services.NewIdleService(nil, func(_ error) error {
		t.cacheGenerationLoader.Stop()
		return nil
	}), nil
}

func (t *Loki) supportIndexDeleteRequest() bool {
	return config.UsingObjectStorageIndex(t.Cfg.SchemaConfig.Configs)
}

// compactorAddress returns the configured address of the compactor.
// It prefers grpc address over http. If the address is grpc then the bool would be true otherwise false
func (t *Loki) compactorAddress() (string, bool, error) {
	legacyReadMode := t.Cfg.LegacyReadTarget && t.Cfg.isTarget(Read)
	if t.Cfg.isTarget(All) || legacyReadMode || t.Cfg.isTarget(Backend) {
		// In single binary or read modes, this module depends on Server
		return net.JoinHostPort(t.Cfg.Server.GRPCListenAddress, strconv.Itoa(t.Cfg.Server.GRPCListenPort)), true, nil
	}

	if t.Cfg.Common.CompactorAddress == "" && t.Cfg.Common.CompactorGRPCAddress == "" {
		return "", false, errors.New("query filtering for deletes requires 'compactor_grpc_address' or 'compactor_address' to be configured")
	}

	if t.Cfg.Common.CompactorGRPCAddress != "" {
		return t.Cfg.Common.CompactorGRPCAddress, true, nil
	}

	return t.Cfg.Common.CompactorAddress, false, nil
}

func (t *Loki) initQueryFrontend() (_ services.Service, err error) {
	level.Debug(util_log.Logger).Log("msg", "initializing query frontend", "config", fmt.Sprintf("%+v", t.Cfg.Frontend))

	combinedCfg := frontend.CombinedFrontendConfig{
		Handler:       t.Cfg.Frontend.Handler,
		FrontendV1:    t.Cfg.Frontend.FrontendV1,
		FrontendV2:    t.Cfg.Frontend.FrontendV2,
		DownstreamURL: t.Cfg.Frontend.DownstreamURL,
	}
	frontendTripper, frontendV1, frontendV2, err := frontend.InitFrontend(
		combinedCfg,
		scheduler.SafeReadRing(t.Cfg.QueryScheduler, t.querySchedulerRingManager),
		disabledShuffleShardingLimits{},
		t.Cfg.Server.GRPCListenPort,
		util_log.Logger,
		prometheus.DefaultRegisterer,
		t.Cfg.MetricsNamespace,
		t.Codec,
	)
	if err != nil {
		return nil, err
	}

	if frontendV1 != nil {
		frontendv1pb.RegisterFrontendServer(t.Server.GRPC, frontendV1)
		t.frontend = frontendV1
		level.Debug(util_log.Logger).Log("msg", "using query frontend", "version", "v1")
	} else if frontendV2 != nil {
		frontendv2pb.RegisterFrontendForQuerierServer(t.Server.GRPC, frontendV2)
		t.frontend = frontendV2
		level.Debug(util_log.Logger).Log("msg", "using query frontend", "version", "v2")
	} else {
		level.Debug(util_log.Logger).Log("msg", "no query frontend configured")
	}

	roundTripper := queryrange.NewSerializeRoundTripper(t.QueryFrontEndMiddleware.Wrap(frontendTripper), queryrange.DefaultCodec, t.Cfg.Frontend.SupportParquetEncoding)

	frontendHandler := transport.NewHandler(t.Cfg.Frontend.Handler, roundTripper, util_log.Logger, prometheus.DefaultRegisterer, t.Cfg.MetricsNamespace)
	if t.Cfg.Frontend.CompressResponses {
		frontendHandler = gziphandler.GzipHandler(frontendHandler)
	}

	// TODO: add SerializeHTTPHandler
	toMerge := []middleware.Interface{
		httpreq.ExtractQueryTagsMiddleware(),
		httpreq.PropagateHeadersMiddleware(httpreq.LokiActorPathHeader, httpreq.LokiEncodingFlagsHeader, httpreq.LokiDisablePipelineWrappersHeader),
		serverutil.RecoveryHTTPMiddleware,
		t.HTTPAuthMiddleware,
		queryrange.StatsHTTPMiddleware,
		serverutil.NewPrepopulateMiddleware(),
		serverutil.ResponseJSONMiddleware(),
	}

	if t.Cfg.Querier.PerRequestLimitsEnabled {
		logger := log.With(util_log.Logger, "component", "query-limiter-middleware")
		toMerge = append(toMerge, querylimits.NewQueryLimitsMiddleware(logger))
	}

	frontendHandler = middleware.Merge(toMerge...).Wrap(frontendHandler)

	var defaultHandler http.Handler
	// If this process also acts as a Querier we don't do any proxying of tail requests
	if t.Cfg.Frontend.TailProxyURL != "" && !t.isModuleActive(Querier) {
		httpMiddleware := middleware.Merge(
			httpreq.ExtractQueryTagsMiddleware(),
			t.HTTPAuthMiddleware,
			queryrange.StatsHTTPMiddleware,
		)
		tailURL, err := url.Parse(t.Cfg.Frontend.TailProxyURL)
		if err != nil {
			return nil, err
		}
		tp := httputil.NewSingleHostReverseProxy(tailURL)

		cfg, err := t.Cfg.Frontend.TLS.GetTLSConfig()
		if err != nil {
			return nil, err
		}

		tp.Transport = &http.Transport{
			TLSClientConfig: cfg,
		}

		director := tp.Director
		tp.Director = func(req *http.Request) {
			director(req)
			req.Host = tailURL.Host
		}

		defaultHandler = httpMiddleware.Wrap(tp)
	} else {
		defaultHandler = frontendHandler
	}
	t.Server.HTTP.Path("/loki/api/v1/query_range").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/query").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/label").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/labels").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/label/{name}/values").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/series").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/patterns").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/detected_labels").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/detected_fields").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/detected_field/{name}/values").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/index/stats").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/index/shards").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/index/volume").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/loki/api/v1/index/volume_range").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/api/prom/query").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/api/prom/label").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/api/prom/label/{name}/values").Methods("GET", "POST").Handler(frontendHandler)
	t.Server.HTTP.Path("/api/prom/series").Methods("GET", "POST").Handler(frontendHandler)

	// Only register tailing requests if this process does not act as a Querier
	// If this process is also a Querier the Querier will register the tail endpoints.
	if !t.isModuleActive(Querier) {
		// defer tail endpoints to the default handler
		t.Server.HTTP.Path("/loki/api/v1/tail").Methods("GET", "POST").Handler(defaultHandler)
		t.Server.HTTP.Path("/api/prom/tail").Methods("GET", "POST").Handler(defaultHandler)
	}

	if t.frontend == nil {
		return services.NewIdleService(nil, func(_ error) error {
			if t.stopper != nil {
				t.stopper.Stop()
				t.stopper = nil
			}
			return nil
		}), nil
	}

	return services.NewIdleService(func(ctx context.Context) error {
		return services.StartAndAwaitRunning(ctx, t.frontend)
	}, func(_ error) error {
		// Log but not return in case of error, so that other following dependencies
		// are stopped too.
		if err := services.StopAndAwaitTerminated(context.Background(), t.frontend); err != nil {
			level.Warn(util_log.Logger).Log("msg", "failed to stop frontend service", "err", err)
		}

		if t.stopper != nil {
			t.stopper.Stop()
		}
		return nil
	}), nil
}

func (t *Loki) initRulerStorage() (_ services.Service, err error) {
	// if the ruler is not configured and we're in single binary then let's just log an error and continue.
	// unfortunately there is no way to generate a "default" config and compare default against actual
	// to determine if it's unconfigured.  the following check, however, correctly tests this.
	// Single binary integration tests will break if this ever drifts
	legacyReadMode := t.Cfg.LegacyReadTarget && t.Cfg.isTarget(Read)
	var storageNotConfigured bool
	var storagekey string
	if t.Cfg.StorageConfig.UseThanosObjstore {
		storageNotConfigured = t.Cfg.RulerStorage.IsDefaults()
		storagekey = "ruler_storage"
	} else {
		storageNotConfigured = t.Cfg.Ruler.StoreConfig.IsDefaults()
		storagekey = "ruler.storage"
	}
	if (t.Cfg.isTarget(All) || legacyReadMode || t.Cfg.isTarget(Backend)) && storageNotConfigured {
		level.Info(util_log.Logger).Log("msg", "Ruler storage is not configured; ruler will not be started.",
			"config_key", storagekey)
		return
	}

	// To help reduce user confusion, warn if the user has configured both the legacy ruler.storage and the new ruler_storage is overriding it, or vice versa.
	if t.Cfg.StorageConfig.UseThanosObjstore && !t.Cfg.Ruler.StoreConfig.IsDefaults() {
		level.Warn(util_log.Logger).Log("msg", "ruler.storage exists and is not empty, but will be ignored in favour of ruler_storage because storage_config.use_thanos_objstore is true.")
	} else if !t.Cfg.StorageConfig.UseThanosObjstore && !t.Cfg.RulerStorage.IsDefaults() {
		level.Warn(util_log.Logger).Log("msg", "ruler_storage exists and is not empty, but will be ignored in favour of ruler.storage because storage_config.use_thanos_objstore is false.")
	}

	// Make sure storage directory exists if using a filesystem store
	var localStoreDir string
	if t.Cfg.StorageConfig.UseThanosObjstore {
		// storage_config.use_thanos_objstore is true so we're using
		// ruler_storage. Is it one of the local backend spellings
		// 'filesystem' or 'local'?
		if t.Cfg.RulerStorage.Backend == local.Name {
			localStoreDir = t.Cfg.RulerStorage.Local.Directory
		} else if t.Cfg.RulerStorage.Backend == bucket.Filesystem {
			localStoreDir = t.Cfg.RulerStorage.Filesystem.Directory
		}
	} else if t.Cfg.Ruler.StoreConfig.Type == "local" {
		// Legacy ruler.storage.local.directory
		localStoreDir = t.Cfg.Ruler.StoreConfig.Local.Directory
	}
	if localStoreDir != "" {
		err := chunk_util.EnsureDirectory(localStoreDir)
		if err != nil {
			return nil, err
		}
	}

	if t.Cfg.StorageConfig.UseThanosObjstore {
		t.RulerStorage, err = base_ruler.NewRuleStore(context.Background(), t.Cfg.RulerStorage, t.Overrides, ruler.GroupLoader{}, util_log.Logger)
	} else {
		t.RulerStorage, err = base_ruler.NewLegacyRuleStore(t.Cfg.Ruler.StoreConfig, t.Cfg.StorageConfig.Hedging, t.ClientMetrics, ruler.GroupLoader{}, util_log.Logger)
	}

	return
}

func (t *Loki) initRuler() (_ services.Service, err error) {
	if t.RulerStorage == nil {
		level.Warn(util_log.Logger).Log("msg", "RulerStorage is nil. Not starting the ruler.")
		return nil, nil
	}

	if t.ruleEvaluator == nil {
		level.Warn(util_log.Logger).Log("msg", "RuleEvaluator is nil. Not starting the ruler.") // TODO better error msg
		return nil, nil
	}

	t.Cfg.Ruler.Ring.ListenPort = t.Cfg.Server.GRPCListenPort

	t.ruler, err = ruler.NewRuler(
		t.Cfg.Ruler,
		t.ruleEvaluator,
		prometheus.DefaultRegisterer,
		util_log.Logger,
		t.RulerStorage,
		t.Overrides,
		t.Cfg.MetricsNamespace,
	)
	if err != nil {
		return
	}

	t.rulerAPI = base_ruler.NewAPI(t.ruler, t.RulerStorage, util_log.Logger)

	// Expose HTTP endpoints.
	if t.Cfg.Ruler.EnableAPI {
		t.Server.HTTP.Path("/ruler/ring").Methods("GET", "POST").Handler(t.ruler)

		if t.Cfg.InternalServer.Enable {
			t.InternalServer.HTTP.Path("/ruler/ring").Methods("GET", "POST").Handler(t.ruler)
		}

		base_ruler.RegisterRulerServer(t.Server.GRPC, t.ruler)

		// Prometheus Rule API Routes
		t.Server.HTTP.Path("/prometheus/api/v1/rules").Methods("GET").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.PrometheusRules)))
		t.Server.HTTP.Path("/prometheus/api/v1/alerts").Methods("GET").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.PrometheusAlerts)))

		// Ruler Legacy API Routes
		t.Server.HTTP.Path("/api/prom/rules").Methods("GET").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.ListRules)))
		t.Server.HTTP.Path("/api/prom/rules/{namespace}").Methods("GET").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.ListRules)))
		t.Server.HTTP.Path("/api/prom/rules/{namespace}").Methods("POST").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.CreateRuleGroup)))
		t.Server.HTTP.Path("/api/prom/rules/{namespace}").Methods("DELETE").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.DeleteNamespace)))
		t.Server.HTTP.Path("/api/prom/rules/{namespace}/{groupName}").Methods("GET").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.GetRuleGroup)))
		t.Server.HTTP.Path("/api/prom/rules/{namespace}/{groupName}").Methods("DELETE").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.DeleteRuleGroup)))

		// Ruler API Routes
		t.Server.HTTP.Path("/loki/api/v1/rules").Methods("GET").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.ListRules)))
		t.Server.HTTP.Path("/loki/api/v1/rules/{namespace}").Methods("GET").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.ListRules)))
		t.Server.HTTP.Path("/loki/api/v1/rules/{namespace}").Methods("POST").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.CreateRuleGroup)))
		t.Server.HTTP.Path("/loki/api/v1/rules/{namespace}").Methods("DELETE").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.DeleteNamespace)))
		t.Server.HTTP.Path("/loki/api/v1/rules/{namespace}/{groupName}").Methods("GET").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.GetRuleGroup)))
		t.Server.HTTP.Path("/loki/api/v1/rules/{namespace}/{groupName}").Methods("DELETE").Handler(t.HTTPAuthMiddleware.Wrap(http.HandlerFunc(t.rulerAPI.DeleteRuleGroup)))
	}

	deleteStore, err := t.deleteRequestsClient("ruler", t.Overrides)
	if err != nil {
		return nil, err
	}
	t.ruler.AddListener(deleteRequestsStoreListener(deleteStore))

	return t.ruler, nil
}

func (t *Loki) initRuleEvaluator() (services.Service, error) {
	if err := t.Cfg.Ruler.Evaluation.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ruler evaluation config: %w", err)
	}

	var (
		evaluator ruler.Evaluator
		err       error
	)

	mode := t.Cfg.Ruler.Evaluation.Mode
	logger := log.With(util_log.Logger, "component", "ruler", "evaluation_mode", mode)

	var svc services.Service
	switch mode {
	case ruler.EvalModeLocal:
		var deleteStore deletion.DeleteRequestsClient
		deleteStore, err = t.deleteRequestsClient("rule-evaluator", t.Overrides)
		if err != nil {
			break
		}

		var engine *logql.QueryEngine
		engine, err = t.createRulerQueryEngine(logger, deleteStore)
		if err != nil {
			break
		}

		evaluator, err = ruler.NewLocalEvaluator(engine, logger)

		// The delete client needs to be stopped when the evaluator is stopped.
		// We wrap the client on a IDLE service and call Stop on shutdown.
		svc = services.NewIdleService(nil, func(_ error) error {
			deleteStore.Stop()
			return nil
		})
	case ruler.EvalModeRemote:
		qfClient, e := ruler.DialQueryFrontend(&t.Cfg.Ruler.Evaluation.QueryFrontend)
		if e != nil {
			return nil, fmt.Errorf("failed to dial query frontend for remote rule evaluation: %w", err)
		}

		evaluator, err = ruler.NewRemoteEvaluator(qfClient, t.Overrides, logger, prometheus.DefaultRegisterer)
	default:
		err = fmt.Errorf("unknown rule evaluation mode %q", mode)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create %s rule evaluator: %w", mode, err)
	}

	t.ruleEvaluator = ruler.NewEvaluatorWithJitter(evaluator, t.Cfg.Ruler.Evaluation.MaxJitter, fnv.New32a(), logger)

	return svc, nil
}

func (t *Loki) initMemberlistKV() (services.Service, error) {
	reg := prometheus.DefaultRegisterer

	t.Cfg.MemberlistKV.MetricsNamespace = constants.Loki
	t.Cfg.MemberlistKV.Codecs = []codec.Codec{
		ring.GetCodec(),
		analytics.JSONCodec,
		ring.GetPartitionRingCodec(),
	}

	dnsProviderReg := prometheus.WrapRegistererWithPrefix(
		t.Cfg.MetricsNamespace+"_",
		prometheus.WrapRegistererWith(
			prometheus.Labels{"name": "memberlist"},
			reg,
		),
	)
	dnsProvider := dns.NewProvider(util_log.Logger, dnsProviderReg, dns.GolangResolverType)

	// TODO(ashwanth): This is not considering component specific overrides for InstanceInterfaceNames.
	// This should be fixed in the future.
	var err error
	t.Cfg.MemberlistKV.AdvertiseAddr, err = ring.GetInstanceAddr(
		t.Cfg.MemberlistKV.AdvertiseAddr,
		t.Cfg.Common.Ring.InstanceInterfaceNames,
		util_log.Logger,
		t.Cfg.Common.Ring.EnableIPv6,
	)
	if err != nil {
		return nil, err
	}
	t.MemberlistKV = memberlist.NewKVInitService(&t.Cfg.MemberlistKV, util_log.Logger, dnsProvider, reg)

	t.Cfg.CompactorConfig.CompactorRing.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV
	t.Cfg.Distributor.DistributorRing.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV
	t.Cfg.IndexGateway.Ring.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV
	t.Cfg.Ingester.LifecyclerConfig.RingConfig.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV
	t.Cfg.QueryScheduler.SchedulerRing.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV
	t.Cfg.Ruler.Ring.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV
	t.Cfg.Pattern.LifecyclerConfig.RingConfig.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV
	t.Cfg.Ingester.KafkaIngestion.PartitionRingConfig.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV
	t.Cfg.IngestLimits.LifecyclerConfig.RingConfig.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV
	t.Cfg.IngestLimitsFrontend.LifecyclerConfig.RingConfig.KVStore.MemberlistKV = t.MemberlistKV.GetMemberlistKV

	t.Server.HTTP.Handle("/memberlist", t.MemberlistKV)

	if t.Cfg.InternalServer.Enable {
		t.InternalServer.HTTP.Path("/memberlist").Methods("GET").Handler(t.MemberlistKV)
	}

	return t.MemberlistKV, nil
}

func (t *Loki) initCompactorWorkerMode() (services.Service, error) {
	err := t.Cfg.SchemaConfig.Load()
	if err != nil {
		return nil, err
	}

	if !config.UsingObjectStorageIndex(t.Cfg.SchemaConfig.Configs) {
		return nil, errors.New("for running the compactor in worker mode, the schema must have a tsdb or boltdb-shipper index type")
	}

	if t.Cfg.Common.CompactorGRPCAddress == "" {
		return nil, errors.New("for running compactor in worker mode, compactor_grpc_address must be configured to grpc address of the main compactor")
	}

	reg := prometheus.WrapRegistererWith(prometheus.Labels{"for": "job_queue", "client_type": "compactor_worker"}, prometheus.DefaultRegisterer)
	compactorClient, err := compactorclient.NewGRPCClient(t.Cfg.Common.CompactorGRPCAddress, t.Cfg.CompactorGRPCClient, reg)
	if err != nil {
		return nil, err
	}

	objectClients := make(map[config.DayTime]client.ObjectClient)
	for _, periodConfig := range t.Cfg.SchemaConfig.Configs {
		if !config.IsObjectStorageIndex(periodConfig.IndexType) {
			continue
		}

		objectClient, err := storage.NewObjectClient(periodConfig.ObjectType, "compactor", t.Cfg.StorageConfig, t.ClientMetrics)
		if err != nil {
			return nil, fmt.Errorf("failed to create object client: %w", err)
		}

		objectClients[periodConfig.From] = objectClient
	}

	return compactor.NewWorkerManager(t.Cfg.CompactorConfig, compactorClient, t.Cfg.SchemaConfig, objectClients, prometheus.DefaultRegisterer)
}

func (t *Loki) initCompactor() (services.Service, error) {
	if t.Cfg.CompactorConfig.HorizontalScalingMode == compactor.HorizontalScalingModeWorker {
		return t.initCompactorWorkerMode()
	}

	// Set some config sections from other config sections in the config struct
	t.Cfg.CompactorConfig.CompactorRing.ListenPort = t.Cfg.Server.GRPCListenPort

	err := t.Cfg.SchemaConfig.Load()
	if err != nil {
		return nil, err
	}

	if !config.UsingObjectStorageIndex(t.Cfg.SchemaConfig.Configs) {
		level.Info(util_log.Logger).Log("msg", "schema does not contain tsdb or boltdb-shipper index types, not starting compactor")
		return nil, nil
	}

	objectClients := make(map[config.DayTime]client.ObjectClient)
	for _, periodConfig := range t.Cfg.SchemaConfig.Configs {
		if !config.IsObjectStorageIndex(periodConfig.IndexType) {
			continue
		}

		objectClient, err := storage.NewObjectClient(periodConfig.ObjectType, "compactor", t.Cfg.StorageConfig, t.ClientMetrics)
		if err != nil {
			return nil, fmt.Errorf("failed to create object client: %w", err)
		}

		objectClients[periodConfig.From] = objectClient
	}

	var deleteRequestStoreClient client.ObjectClient
	if t.Cfg.CompactorConfig.RetentionEnabled {
		if deleteStore := t.Cfg.CompactorConfig.DeleteRequestStore; deleteStore != "" {
			deleteRequestStoreClient, err = storage.NewObjectClient(deleteStore, "delete-store", t.Cfg.StorageConfig, t.ClientMetrics)
			if err != nil {
				return nil, fmt.Errorf("failed to create delete request store object client: %w", err)
			}
		} else {
			return nil, fmt.Errorf("compactor.delete-request-store should be configured when retention is enabled")
		}
	}

	indexUpdatePropagationMaxDelay := shipperQuerierIndexUpdateDelay(t.Cfg.StorageConfig.IndexCacheValidity, shipperResyncInterval(t.Cfg.StorageConfig, t.Cfg.SchemaConfig.Configs))
	t.compactor, err = compactor.NewCompactor(
		t.Cfg.CompactorConfig,
		objectClients,
		deleteRequestStoreClient,
		t.Cfg.SchemaConfig,
		t.Overrides,
		indexUpdatePropagationMaxDelay,
		prometheus.DefaultRegisterer,
		t.Cfg.MetricsNamespace,
	)
	if err != nil {
		return nil, err
	}

	t.compactor.RegisterIndexCompactor(types.BoltDBShipperType, boltdbcompactor.NewIndexCompactor())
	t.compactor.RegisterIndexCompactor(types.TSDBType, tsdb.NewIndexCompactor())
	prefix, compactorHandler := t.compactor.Handler()
	t.Server.HTTP.PathPrefix(prefix).Handler(compactorHandler)

	if t.Cfg.InternalServer.Enable {
		t.InternalServer.HTTP.PathPrefix(prefix).Handler(compactorHandler)
	}

	if t.Cfg.CompactorConfig.RetentionEnabled {
		t.Server.HTTP.Path("/loki/api/v1/delete").Methods("PUT", "POST").Handler(t.addCompactorMiddleware(t.compactor.DeleteRequestsHandler.AddDeleteRequestHandler))
		t.Server.HTTP.Path("/loki/api/v1/delete").Methods("GET").Handler(t.addCompactorMiddleware(t.compactor.DeleteRequestsHandler.GetAllDeleteRequestsHandler))
		t.Server.HTTP.Path("/loki/api/v1/delete").Methods("DELETE").Handler(t.addCompactorMiddleware(t.compactor.DeleteRequestsHandler.CancelDeleteRequestHandler))
		t.Server.HTTP.Path("/loki/api/v1/cache/generation_numbers").Methods("GET").Handler(t.addCompactorMiddleware(t.compactor.DeleteRequestsHandler.GetCacheGenerationNumberHandler))
		grpc.RegisterCompactorServer(t.Server.GRPC, t.compactor.DeleteRequestsGRPCHandler)
	}

	if t.Cfg.CompactorConfig.HorizontalScalingMode == compactor.HorizontalScalingModeMain {
		grpc.RegisterJobQueueServer(t.Server.GRPC, t.compactor.JobQueue)
	}

	return t.compactor, nil
}

func (t *Loki) addCompactorMiddleware(h http.HandlerFunc) http.Handler {
	return middleware.Merge(t.HTTPAuthMiddleware, deletion.TenantMiddleware(t.Overrides)).Wrap(h)
}

func (t *Loki) initBloomGateway() (services.Service, error) {
	if !t.Cfg.BloomGateway.Enabled {
		return nil, nil
	}
	logger := log.With(util_log.Logger, "component", "bloom-gateway")

	gateway, err := bloomgateway.New(t.Cfg.BloomGateway, t.BloomStore, logger, prometheus.DefaultRegisterer)
	if err != nil {
		return nil, err
	}
	logproto.RegisterBloomGatewayServer(t.Server.GRPC, gateway)
	return gateway, nil
}

func (t *Loki) initIndexGateway() (services.Service, error) {
	shardingStrategy := indexgateway.GetShardingStrategy(t.Cfg.IndexGateway, t.indexGatewayRingManager, t.Overrides)

	var indexClients []indexgateway.IndexClientWithRange
	for i, period := range t.Cfg.SchemaConfig.Configs {
		if period.IndexType != types.BoltDBShipperType {
			continue
		}

		periodEndTime := config.DayTime{Time: math.MaxInt64}
		if i < len(t.Cfg.SchemaConfig.Configs)-1 {
			periodEndTime = config.DayTime{Time: t.Cfg.SchemaConfig.Configs[i+1].From.Time.Add(-time.Millisecond)}
		}
		tableRange := period.GetIndexTableNumberRange(periodEndTime)

		indexClient, err := storage.NewIndexClient("index-store", period, tableRange, t.Cfg.StorageConfig, t.Cfg.SchemaConfig, t.Overrides, t.ClientMetrics, shardingStrategy,
			prometheus.DefaultRegisterer, log.With(util_log.Logger, "index-store", fmt.Sprintf("%s-%s", period.IndexType, period.From.String())), t.Cfg.MetricsNamespace,
		)
		if err != nil {
			return nil, err
		}

		indexClients = append(indexClients, indexgateway.IndexClientWithRange{
			IndexClient: indexClient,
			TableRange:  tableRange,
		})
	}

	logger := log.With(util_log.Logger, "component", "index-gateway")

	var bloomQuerier indexgateway.BloomQuerier
	if t.Cfg.BloomGateway.Enabled {
		resolver := bloomgateway.NewBlockResolver(t.BloomStore, logger)
		querierCfg := bloomgateway.QuerierConfig{
			BuildTableOffset: t.Cfg.BloomBuild.Planner.MinTableOffset,
			BuildInterval:    t.Cfg.BloomBuild.Planner.PlanningInterval,
		}
		bloomQuerier = bloomgateway.NewQuerier(t.bloomGatewayClient, querierCfg, t.Overrides, resolver, prometheus.DefaultRegisterer, logger)
	}

	gateway, err := indexgateway.NewIndexGateway(t.Cfg.IndexGateway, t.Overrides, logger, prometheus.DefaultRegisterer, t.Store, indexClients, bloomQuerier)
	if err != nil {
		return nil, err
	}

	logproto.RegisterIndexGatewayServer(t.Server.GRPC, gateway)
	return gateway, nil
}

func (t *Loki) initIndexGatewayRing() (_ services.Service, err error) {
	// Inherit ring listen port from gRPC config
	t.Cfg.IndexGateway.Ring.ListenPort = t.Cfg.Server.GRPCListenPort

	// IndexGateway runs by default on legacy read and backend targets, and should always assume
	// ring mode when run in this way.
	legacyReadMode := t.Cfg.LegacyReadTarget && t.isModuleActive(Read)
	if legacyReadMode || t.isModuleActive(Backend) {
		t.Cfg.IndexGateway.Mode = indexgateway.RingMode
	}

	if t.Cfg.IndexGateway.Mode != indexgateway.RingMode {
		return
	}

	t.Cfg.StorageConfig.BoltDBShipperConfig.Mode = indexshipper.ModeReadOnly
	t.Cfg.StorageConfig.TSDBShipperConfig.Mode = indexshipper.ModeReadOnly

	managerMode := lokiring.ClientMode
	if t.Cfg.isTarget(IndexGateway) || legacyReadMode || t.Cfg.isTarget(Backend) {
		managerMode = lokiring.ServerMode
	}
	rm, err := lokiring.NewRingManager(indexGatewayRingKey, managerMode, t.Cfg.IndexGateway.Ring, t.Cfg.IndexGateway.Ring.ReplicationFactor, indexgateway.NumTokens, util_log.Logger, prometheus.DefaultRegisterer)
	if err != nil {
		return nil, gerrors.Wrap(err, "new index gateway ring manager")
	}

	t.indexGatewayRingManager = rm

	t.Server.HTTP.Path("/indexgateway/ring").Methods("GET", "POST").Handler(t.indexGatewayRingManager)

	if t.Cfg.InternalServer.Enable {
		t.InternalServer.HTTP.Path("/indexgateway/ring").Methods("GET", "POST").Handler(t.indexGatewayRingManager)
	}

	return t.indexGatewayRingManager, nil
}

func (t *Loki) initIndexGatewayInterceptors() (services.Service, error) {
	// Only expose per-tenant metric if index gateway runs as standalone service
	if t.Cfg.isTarget(IndexGateway) {
		interceptors := indexgateway.NewServerInterceptors(prometheus.DefaultRegisterer)
		t.Cfg.Server.GRPCMiddleware = append(t.Cfg.Server.GRPCMiddleware, interceptors.PerTenantRequestCount)
	}
	return nil, nil
}

func (t *Loki) initBloomPlanner() (services.Service, error) {
	if !t.Cfg.BloomBuild.Enabled {
		return nil, nil
	}

	logger := log.With(util_log.Logger, "component", "bloom-planner")

	var ringManager *lokiring.RingManager
	if t.Cfg.isTarget(Backend) && t.indexGatewayRingManager != nil {
		// Bloom planner and builder are part of the backend target in Simple Scalable Deployment mode.
		// To avoid creating a new ring just for this special case, we can use the index gateway ring, which is already
		// part of the backend target. The planner creates a watcher service that regularly checks which replica is
		// the leader. Only the leader plans the tasks. Builders connect to the leader instance to pull tasks.
		level.Info(logger).Log("msg", "initializing bloom planner in ring mode as part of backend target")
		ringManager = t.indexGatewayRingManager
	}

	p, err := planner.New(
		t.Cfg.BloomBuild.Planner,
		t.Overrides,
		t.Cfg.SchemaConfig,
		t.Cfg.StorageConfig,
		t.ClientMetrics,
		t.BloomStore,
		logger,
		prometheus.DefaultRegisterer,
		ringManager,
	)
	if err != nil {
		return nil, err
	}

	bloomprotos.RegisterPlannerForBuilderServer(t.Server.GRPC, p)
	return p, nil
}

func (t *Loki) initBloomGatewayClient() (services.Service, error) {
	var err error
	if t.Cfg.BloomGateway.Enabled {
		logger := log.With(util_log.Logger, "component", "bloom-gateway-client")
		t.bloomGatewayClient, err = bloomgateway.NewClient(t.Cfg.BloomGateway.Client, prometheus.DefaultRegisterer, logger)
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (t *Loki) initBloomBuilder() (services.Service, error) {
	if !t.Cfg.BloomBuild.Enabled {
		return nil, nil
	}

	logger := log.With(util_log.Logger, "component", "bloom-builder")

	var ringManager *lokiring.RingManager
	if t.Cfg.isTarget(Backend) && t.indexGatewayRingManager != nil {
		// Bloom planner and builder are part of the backend target in Simple Scalable Deployment mode.
		// To avoid creating a new ring just for this special case, we can use the index gateway ring, which is already
		// part of the backend target. The planner creates a watcher service that regularly checks which replica is
		// the leader. Only the leader plans the tasks. Builders connect to the leader instance to pull tasks.
		level.Info(logger).Log("msg", "initializing bloom builder in ring mode as part of backend target")
		ringManager = t.indexGatewayRingManager
	}

	return builder.New(
		t.Cfg.BloomBuild.Builder,
		t.Overrides,
		t.Cfg.SchemaConfig,
		t.Cfg.StorageConfig,
		t.ClientMetrics,
		t.Store,
		t.BloomStore,
		t.bloomGatewayClient,
		logger,
		prometheus.DefaultRegisterer,
		ringManager,
	)
}

func (t *Loki) initQueryScheduler() (services.Service, error) {
	s, err := scheduler.NewScheduler(t.Cfg.QueryScheduler, t.Overrides, util_log.Logger, t.querySchedulerRingManager, prometheus.DefaultRegisterer, t.Cfg.MetricsNamespace)
	if err != nil {
		return nil, err
	}

	schedulerpb.RegisterSchedulerForFrontendServer(t.Server.GRPC, s)
	schedulerpb.RegisterSchedulerForQuerierServer(t.Server.GRPC, s)

	t.queryScheduler = s
	return s, nil
}

func (t *Loki) initQuerySchedulerRing() (_ services.Service, err error) {
	if !t.Cfg.QueryScheduler.UseSchedulerRing {
		return
	}

	// Set some config sections from other config sections in the config struct
	t.Cfg.QueryScheduler.SchedulerRing.ListenPort = t.Cfg.Server.GRPCListenPort

	managerMode := lokiring.ClientMode
	if t.Cfg.isTarget(QueryScheduler) || t.Cfg.isTarget(Backend) || t.Cfg.isTarget(All) || (t.Cfg.LegacyReadTarget && t.Cfg.isTarget(Read)) {
		managerMode = lokiring.ServerMode
	}
	rm, err := lokiring.NewRingManager(schedulerRingKey, managerMode, t.Cfg.QueryScheduler.SchedulerRing, scheduler.ReplicationFactor, scheduler.NumTokens, util_log.Logger, prometheus.DefaultRegisterer)
	if err != nil {
		return nil, gerrors.Wrap(err, "new scheduler ring manager")
	}

	t.querySchedulerRingManager = rm

	t.Server.HTTP.Path("/scheduler/ring").Methods("GET", "POST").Handler(t.querySchedulerRingManager)

	if t.Cfg.InternalServer.Enable {
		t.InternalServer.HTTP.Path("/scheduler/ring").Methods("GET", "POST").Handler(t.querySchedulerRingManager)
	}

	return t.querySchedulerRingManager, nil
}

func (t *Loki) initQueryLimiter() (services.Service, error) {
	_ = level.Debug(util_log.Logger).Log("msg", "initializing query limiter")
	logger := log.With(util_log.Logger, "component", "query-limiter")
	t.Overrides = querylimits.NewLimiter(logger, t.Overrides)
	return nil, nil
}

func (t *Loki) initQueryLimitsInterceptors() (services.Service, error) {
	_ = level.Debug(util_log.Logger).Log("msg", "initializing query limits interceptors")
	t.Cfg.Server.GRPCMiddleware = append(t.Cfg.Server.GRPCMiddleware, querylimits.ServerQueryLimitsInterceptor)
	t.Cfg.Server.GRPCStreamMiddleware = append(t.Cfg.Server.GRPCStreamMiddleware, querylimits.StreamServerQueryLimitsInterceptor)

	return nil, nil
}

func (t *Loki) initIngesterGRPCInterceptors() (services.Service, error) {
	_ = level.Debug(util_log.Logger).Log("msg", "initializing ingester query tags interceptors")
	t.Cfg.Server.GRPCStreamMiddleware = append(
		t.Cfg.Server.GRPCStreamMiddleware,
		serverutil.StreamServerQueryTagsInterceptor,
		serverutil.StreamServerHTTPHeadersInterceptor,
	)

	t.Cfg.Server.GRPCMiddleware = append(
		t.Cfg.Server.GRPCMiddleware,
		serverutil.UnaryServerQueryTagsInterceptor,
		serverutil.UnaryServerHTTPHeadersnIterceptor,
	)

	return nil, nil
}

func (t *Loki) initAnalytics() (services.Service, error) {
	if !t.Cfg.Analytics.Enabled {
		return nil, nil
	}
	t.Cfg.Analytics.Leader = false
	if t.isModuleActive(Ingester) {
		t.Cfg.Analytics.Leader = true
	}

	analytics.Target(t.Cfg.Target.String())
	period, err := t.Cfg.SchemaConfig.SchemaForTime(model.Now())
	if err != nil {
		return nil, err
	}

	objectClient, err := storage.NewObjectClient(period.ObjectType, "analytics", t.Cfg.StorageConfig, t.ClientMetrics)
	if err != nil {
		level.Info(util_log.Logger).Log("msg", "failed to initialize usage report", "err", err)
		return nil, nil
	}
	ur, err := analytics.NewReporter(t.Cfg.Analytics, t.Cfg.Ingester.LifecyclerConfig.RingConfig.KVStore, objectClient, util_log.Logger, prometheus.DefaultRegisterer)
	if err != nil {
		level.Info(util_log.Logger).Log("msg", "failed to initialize usage report", "err", err)
		return nil, nil
	}
	t.usageReport = ur
	return ur, nil
}

// The Ingest Partition Ring is responsible for watching the available ingesters and assigning partitions to incoming requests.
func (t *Loki) initPartitionRing() (services.Service, error) {
	if !t.Cfg.Ingester.KafkaIngestion.Enabled && !t.Cfg.Querier.QueryPartitionIngesters {
		return nil, nil
	}

	kvClient, err := kv.NewClient(t.Cfg.Ingester.KafkaIngestion.PartitionRingConfig.KVStore, ring.GetPartitionRingCodec(), kv.RegistererWithKVName(prometheus.DefaultRegisterer, ingester.PartitionRingName+"-watcher"), util_log.Logger)
	if err != nil {
		return nil, fmt.Errorf("creating KV store for partitions ring watcher: %w", err)
	}

	t.PartitionRingWatcher = ring.NewPartitionRingWatcher(ingester.PartitionRingName, ingester.PartitionRingKey, kvClient, util_log.Logger, prometheus.WrapRegistererWithPrefix("loki_", prometheus.DefaultRegisterer))
	t.partitionRing = ring.NewPartitionInstanceRing(t.PartitionRingWatcher, t.ring, t.Cfg.Ingester.LifecyclerConfig.RingConfig.HeartbeatTimeout)

	// Expose a web page to view the partitions ring state.
	t.Server.HTTP.Path("/partition-ring").Methods("GET", "POST").Handler(ring.NewPartitionRingPageHandler(t.PartitionRingWatcher, ring.NewPartitionRingEditor(ingester.PartitionRingKey, kvClient)))

	return t.PartitionRingWatcher, nil
}

func (t *Loki) initBlockBuilder() (services.Service, error) {
	logger := log.With(util_log.Logger, "component", "block_builder")

	// TODO(owen-d): perhaps refactor to not use the ingester config?
	id := t.Cfg.Ingester.LifecyclerConfig.ID

	objectStore, err := blockbuilder.NewMultiStore(t.Cfg.SchemaConfig.Configs, t.Cfg.StorageConfig, t.ClientMetrics)
	if err != nil {
		return nil, err
	}

	bb, err := blockbuilder.NewBlockBuilder(
		id,
		t.Cfg.BlockBuilder,
		t.Cfg.KafkaConfig,
		t.Cfg.SchemaConfig.Configs,
		t.Store,
		objectStore,
		logger,
		prometheus.DefaultRegisterer,
	)
	if err != nil {
		return nil, err
	}

	t.blockBuilder = bb
	return t.blockBuilder, nil
}

func (t *Loki) initBlockScheduler() (services.Service, error) {
	logger := log.With(util_log.Logger, "component", "block_scheduler")

	offsetManager, err := partition.NewKafkaOffsetManager(
		t.Cfg.KafkaConfig,
		t.Cfg.Ingester.LifecyclerConfig.ID,
		logger,
		prometheus.DefaultRegisterer,
	)
	if err != nil {
		return nil, fmt.Errorf("creating kafka offset manager: %w", err)
	}

	s, err := blockscheduler.NewScheduler(
		t.Cfg.BlockScheduler,
		offsetManager,
		logger,
		prometheus.DefaultRegisterer,
	)
	if err != nil {
		return nil, err
	}

	t.Server.HTTP.Path("/blockscheduler/status").Methods("GET").Handler(s)

	blockprotos.RegisterSchedulerServiceServer(
		t.Server.GRPC,
		blocktypes.NewSchedulerServer(s),
	)

	return s, nil
}

func (t *Loki) initDataObjExplorer() (services.Service, error) {
	store, err := t.createDataObjBucket("dataobj-explorer")
	if err != nil {
		return nil, err
	}

	explorer, err := explorer.New(store, util_log.Logger)
	if err != nil {
		return nil, err
	}
	path, handler := explorer.Handler()
	t.Server.HTTP.PathPrefix(path).Handler(handler)
	return explorer, nil
}

func (t *Loki) initUI() (services.Service, error) {
	if !t.Cfg.UI.Enabled {
		// UI is disabled, return nil to skip initialization
		return nil, nil
	}
	t.Cfg.UI = t.Cfg.UI.WithAdvertisePort(t.Cfg.Server.HTTPListenPort)
	svc, err := ui.NewService(t.Cfg.UI, t.Server.HTTP, log.With(util_log.Logger, "component", "ui"), prometheus.DefaultRegisterer)
	if err != nil {
		return nil, err
	}
	t.UI = svc
	return svc, nil
}

func (t *Loki) initDataObjConsumer() (services.Service, error) {
	if !t.Cfg.Ingester.KafkaIngestion.Enabled {
		return nil, nil
	}
	store, err := t.createDataObjBucket("dataobj-consumer")
	if err != nil {
		return nil, err
	}

	level.Info(util_log.Logger).Log("msg", "initializing dataobj consumer", "instance", t.Cfg.Ingester.LifecyclerConfig.ID)
	t.dataObjConsumer = consumer.New(
		t.Cfg.KafkaConfig,
		t.Cfg.DataObj.Consumer,
		t.Cfg.DataObj.Metastore,
		t.Cfg.Distributor.TenantTopic.TopicPrefix,
		store,
		t.Cfg.Ingester.LifecyclerConfig.ID,
		t.partitionRing,
		prometheus.DefaultRegisterer,
		util_log.Logger,
	)

	return t.dataObjConsumer, nil
}

func (t *Loki) initDataObjIndexBuilder() (services.Service, error) {
	if !t.Cfg.Ingester.KafkaIngestion.Enabled {
		return nil, nil
	}
	store, err := t.createDataObjBucket("dataobj-index-builder")
	if err != nil {
		return nil, err
	}

	level.Info(util_log.Logger).Log("msg", "initializing dataobj index builder", "instance", t.Cfg.Ingester.LifecyclerConfig.ID)
	t.dataObjIndexBuilder, err = dataobjindex.NewIndexBuilder(
		t.Cfg.DataObj.Index,
		t.Cfg.DataObj.Metastore,
		t.Cfg.KafkaConfig,
		util_log.Logger,
		t.Cfg.Ingester.LifecyclerConfig.ID,
		store,
		prometheus.DefaultRegisterer,
	)

	return t.dataObjIndexBuilder, err
}

func (t *Loki) createDataObjBucket(clientName string) (objstore.Bucket, error) {
	schema, err := t.Cfg.SchemaConfig.SchemaForTime(model.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to get schema for now: %w", err)
	}

	// Handle named stores
	cfg := t.Cfg.StorageConfig.ObjectStore
	backend := schema.ObjectType
	if st, ok := cfg.NamedStores.LookupStoreType(schema.ObjectType); ok {
		backend = st
		// override config with values from named store config
		if err := cfg.NamedStores.OverrideConfig(&cfg.Config, schema.ObjectType); err != nil {
			return nil, err
		}
	}

	var objstoreBucket objstore.Bucket
	objstoreBucket, err = bucket.NewClient(context.Background(), backend, cfg.Config, clientName, util_log.Logger)
	if err != nil {
		return nil, err
	}

	if t.Cfg.DataObj.StorageBucketPrefix != "" {
		objstoreBucket = objstore.NewPrefixedBucket(objstoreBucket, t.Cfg.DataObj.StorageBucketPrefix)
	}

	return objstoreBucket, nil
}

func (t *Loki) deleteRequestsClient(clientType string, limits limiter.CombinedLimits) (deletion.DeleteRequestsClient, error) {
	if !t.supportIndexDeleteRequest() || !t.Cfg.CompactorConfig.RetentionEnabled {
		return deletion.NewNoOpDeleteRequestsClient(), nil
	}

	compactorAddress, isGRPCAddress, err := t.compactorAddress()
	if err != nil {
		return nil, err
	}

	reg := prometheus.WrapRegistererWith(prometheus.Labels{"for": "delete_requests", "client_type": clientType}, prometheus.DefaultRegisterer)
	var compactorClient deletion.CompactorClient
	if isGRPCAddress {
		compactorClient, err = compactorclient.NewGRPCClient(compactorAddress, t.Cfg.CompactorGRPCClient, reg)
		if err != nil {
			return nil, err
		}
	} else {
		compactorClient, err = compactorclient.NewHTTPClient(compactorAddress, t.Cfg.CompactorHTTPClient)
		if err != nil {
			return nil, err
		}
	}

	client, err := deletion.NewDeleteRequestsClient(compactorClient, t.deleteClientMetrics, clientType)
	if err != nil {
		return nil, err
	}

	return deletion.NewPerTenantDeleteRequestsClient(client, limits), nil
}

func (t *Loki) createRulerQueryEngine(logger log.Logger, deleteStore deletion.DeleteRequestsClient) (eng *logql.QueryEngine, err error) {
	querierStore, err := t.getQuerierStore()
	if err != nil {
		return nil, err
	}

	q, err := querier.New(t.Cfg.Querier, querierStore, t.ingesterQuerier, t.Overrides, deleteStore, logger)
	if err != nil {
		return nil, fmt.Errorf("could not create querier: %w", err)
	}

	return logql.NewEngine(t.Cfg.Querier.Engine, q, t.Overrides, logger), nil
}

func calculateMaxLookBack(pc config.PeriodConfig, maxLookBackConfig, minDuration time.Duration) (time.Duration, error) {
	if pc.ObjectType != indexshipper.FilesystemObjectStoreType && maxLookBackConfig.Nanoseconds() != 0 {
		return 0, errors.New("it is an error to specify a non zero `query_store_max_look_back_period` value when using any object store other than `filesystem`")
	}

	if maxLookBackConfig == 0 {
		// If the QueryStoreMaxLookBackPeriod is still it's default value of 0, set it to the minDuration.
		return minDuration, nil
	} else if maxLookBackConfig > 0 && maxLookBackConfig < minDuration {
		// If the QueryStoreMaxLookBackPeriod is > 0 (-1 is allowed for infinite), make sure it's at least greater than minDuration or throw an error
		return 0, fmt.Errorf("the configured query_store_max_look_back_period of '%v' is less than the calculated default of '%v' "+
			"which is calculated based on the max_chunk_age + 15 minute boltdb-shipper interval + 15 min additional buffer.  Increase this value"+
			"greater than the default or remove it from the configuration to use the default", maxLookBackConfig, minDuration)
	}
	return maxLookBackConfig, nil
}

func calculateAsyncStoreQueryIngestersWithin(queryIngestersWithinConfig, minDuration time.Duration) time.Duration {
	// 0 means do not limit queries, we would also not limit ingester queries from AsyncStore.
	if queryIngestersWithinConfig == 0 {
		return 0
	}

	if queryIngestersWithinConfig < minDuration {
		return minDuration
	}
	return queryIngestersWithinConfig
}

// shipperQuerierIndexUpdateDelay returns duration it could take for queriers to serve the index since it was uploaded.
// It considers upto 3 sync attempts for the indexgateway/queries to be successful in syncing the files to factor in worst case scenarios like
// failures in sync, low download throughput, various kinds of caches in between etc. which can delay the sync operation from getting all the updates from the storage.
// It also considers index cache validity because a querier could have cached index just before it was going to resync which means
// it would keep serving index until the cache entries expire.
func shipperQuerierIndexUpdateDelay(cacheValidity, resyncInterval time.Duration) time.Duration {
	return cacheValidity + resyncInterval*3
}

// shipperIngesterIndexUploadDelay returns duration it could take for an index file containing id of a chunk to be uploaded to the shared store since it got flushed.
func shipperIngesterIndexUploadDelay() time.Duration {
	return boltdb.ShardDBsByDuration + indexshipper.UploadInterval
}

// shipperMinIngesterQueryStoreDuration returns minimum duration(with some buffer) ingesters should query their stores to
// avoid missing any logs or chunk ids due to async nature of shipper.
func shipperMinIngesterQueryStoreDuration(maxChunkAge, querierUpdateDelay time.Duration) time.Duration {
	return maxChunkAge + shipperIngesterIndexUploadDelay() + querierUpdateDelay + 5*time.Minute
}

// shipperResyncInterval returns the resync interval for the active shipper index type i.e boltdb-shipper | tsdb
func shipperResyncInterval(storageConfig storage.Config, schemaConfigs []config.PeriodConfig) time.Duration {
	shipperConfigIdx := config.ActivePeriodConfig(schemaConfigs)
	iTy := schemaConfigs[shipperConfigIdx].IndexType
	if iTy != types.BoltDBShipperType && iTy != types.TSDBType {
		shipperConfigIdx++
	}

	var resyncInterval time.Duration
	switch schemaConfigs[shipperConfigIdx].IndexType {
	case types.BoltDBShipperType:
		resyncInterval = storageConfig.BoltDBShipperConfig.ResyncInterval
	case types.TSDBType:
		resyncInterval = storageConfig.TSDBShipperConfig.ResyncInterval
	}

	return resyncInterval
}

// NewServerService constructs service from Server component.
// servicesToWaitFor is called when server is stopping, and should return all
// services that need to terminate before server actually stops.
// N.B.: this function is NOT Cortex specific, please let's keep it that way.
// Passed server should not react on signals. Early return from Run function is considered to be an error.
func NewServerService(serv *server.Server, servicesToWaitFor func() []services.Service) services.Service {
	serverDone := make(chan error, 1)

	runFn := func(ctx context.Context) error {
		go func() {
			defer close(serverDone)
			serverDone <- serv.Run()
		}()

		select {
		case <-ctx.Done():
			return nil
		case err := <-serverDone:
			if err != nil {
				return err
			}
			return fmt.Errorf("server stopped unexpectedly")
		}
	}

	stoppingFn := func(_ error) error {
		// wait until all modules are done, and then shutdown server.
		for _, s := range servicesToWaitFor() {
			_ = s.AwaitTerminated(context.Background())
		}

		// shutdown HTTP and gRPC servers (this also unblocks Run)
		serv.Shutdown()

		// if not closed yet, wait until server stops.
		<-serverDone
		level.Info(util_log.Logger).Log("msg", "server stopped")
		return nil
	}

	return services.NewBasicService(nil, runFn, stoppingFn)
}

// DisableSignalHandling puts a dummy signal handler
func DisableSignalHandling(config *server.Config) {
	config.SignalHandler = make(ignoreSignalHandler)
}

type ignoreSignalHandler chan struct{}

func (dh ignoreSignalHandler) Loop() {
	<-dh
}

func (dh ignoreSignalHandler) Stop() {
	close(dh)
}

func schemaHasBoltDBShipperConfig(scfg config.SchemaConfig) bool {
	for _, cfg := range scfg.Configs {
		if cfg.IndexType == types.BoltDBShipperType {
			return true
		}
	}

	return false
}
