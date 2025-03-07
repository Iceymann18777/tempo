package distributor

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/cortexproject/cortex/pkg/ring"
	ring_client "github.com/cortexproject/cortex/pkg/ring/client"
	"github.com/cortexproject/cortex/pkg/util/limiter"
	cortex_util "github.com/cortexproject/cortex/pkg/util/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gogo/status"
	"github.com/grafana/dskit/services"
	"github.com/segmentio/fasthash/fnv1a"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/logging"
	"github.com/weaveworks/common/user"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/grafana/tempo/modules/distributor/receiver"
	ingester_client "github.com/grafana/tempo/modules/ingester/client"
	"github.com/grafana/tempo/modules/overrides"
	"github.com/grafana/tempo/pkg/tempopb"
	v1 "github.com/grafana/tempo/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/pkg/util"
	"github.com/grafana/tempo/pkg/validation"
)

const (
	discardReasonLabel = "reason"

	// reasonRateLimited indicates that the tenants spans/second exceeded their limits
	reasonRateLimited = "rate_limited"
	// reasonTraceTooLarge indicates that a single trace has too many spans
	reasonTraceTooLarge = "trace_too_large"
	// reasonLiveTracesExceeded indicates that tempo is already tracking too many live traces in the ingesters for this user
	reasonLiveTracesExceeded = "live_traces_exceeded"
	// reasonInternalError indicates an unexpected error occurred processing these spans. analogous to a 500
	reasonInternalError = "internal_error"
)

var (
	metricIngesterAppends = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "distributor_ingester_appends_total",
		Help:      "The total number of batch appends sent to ingesters.",
	}, []string{"ingester"})
	metricIngesterAppendFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "distributor_ingester_append_failures_total",
		Help:      "The total number of failed batch appends sent to ingesters.",
	}, []string{"ingester"})
	metricSpansIngested = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "distributor_spans_received_total",
		Help:      "The total number of spans received per tenant",
	}, []string{"tenant"})
	metricBytesIngested = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "distributor_bytes_received_total",
		Help:      "The total number of proto bytes received per tenant",
	}, []string{"tenant"})
	metricTracesPerBatch = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "tempo",
		Name:      "distributor_traces_per_batch",
		Help:      "The number of traces in each batch",
		Buckets:   prometheus.ExponentialBuckets(2, 2, 10),
	})
	metricIngesterClients = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "tempo",
		Name:      "distributor_ingester_clients",
		Help:      "The current number of ingester clients.",
	})
	metricDiscardedSpans = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "discarded_spans_total",
		Help:      "The total number of samples that were discarded.",
	}, []string{discardReasonLabel, "tenant"})
)

// Distributor coordinates replicates and distribution of log streams.
type Distributor struct {
	services.Service

	cfg             Config
	clientCfg       ingester_client.Config
	ingestersRing   ring.ReadRing
	pool            *ring_client.Pool
	DistributorRing *ring.Ring
	searchEnabled   bool

	// Per-user rate limiter.
	ingestionRateLimiter *limiter.RateLimiter

	// Manager for subservices
	subservices        *services.Manager
	subservicesWatcher *services.FailureWatcher
}

// New a distributor creates.
func New(cfg Config, clientCfg ingester_client.Config, ingestersRing ring.ReadRing, o *overrides.Overrides, multitenancyEnabled bool, level logging.Level, searchEnabled bool) (*Distributor, error) {
	factory := cfg.factory
	if factory == nil {
		factory = func(addr string) (ring_client.PoolClient, error) {
			return ingester_client.New(addr, clientCfg)
		}
	}

	subservices := []services.Service(nil)

	// Create the configured ingestion rate limit strategy (local or global).
	var ingestionRateStrategy limiter.RateLimiterStrategy
	var distributorRing *ring.Ring

	if o.IngestionRateStrategy() == overrides.GlobalIngestionRateStrategy {
		lifecyclerCfg := cfg.DistributorRing.ToLifecyclerConfig()
		lifecycler, err := ring.NewLifecycler(lifecyclerCfg, nil, "distributor", cfg.OverrideRingKey, false, prometheus.DefaultRegisterer)
		if err != nil {
			return nil, err
		}
		subservices = append(subservices, lifecycler)
		ingestionRateStrategy = newGlobalIngestionRateStrategy(o, lifecycler)

		ring, err := ring.New(lifecyclerCfg.RingConfig, "distributor", cfg.OverrideRingKey, prometheus.DefaultRegisterer)
		if err != nil {
			return nil, errors.Wrap(err, "unable to initialize distributor ring")
		}
		distributorRing = ring
		subservices = append(subservices, distributorRing)
	} else {
		ingestionRateStrategy = newLocalIngestionRateStrategy(o)
	}

	pool := ring_client.NewPool("distributor_pool",
		clientCfg.PoolConfig,
		ring_client.NewRingServiceDiscovery(ingestersRing),
		factory,
		metricIngesterClients,
		cortex_util.Logger)

	subservices = append(subservices, pool)

	d := &Distributor{
		cfg:                  cfg,
		clientCfg:            clientCfg,
		ingestersRing:        ingestersRing,
		pool:                 pool,
		DistributorRing:      distributorRing,
		ingestionRateLimiter: limiter.NewRateLimiter(ingestionRateStrategy, 10*time.Second),
		searchEnabled:        searchEnabled,
	}

	cfgReceivers := cfg.Receivers
	if len(cfgReceivers) == 0 {
		cfgReceivers = defaultReceivers
	}

	receivers, err := receiver.New(cfgReceivers, d, multitenancyEnabled, level)
	if err != nil {
		return nil, err
	}
	subservices = append(subservices, receivers)

	d.subservices, err = services.NewManager(subservices...)
	if err != nil {
		return nil, fmt.Errorf("failed to create subservices %w", err)
	}
	d.subservicesWatcher = services.NewFailureWatcher()
	d.subservicesWatcher.WatchManager(d.subservices)

	d.Service = services.NewBasicService(d.starting, d.running, d.stopping)
	return d, nil
}

func (d *Distributor) starting(ctx context.Context) error {
	// Only report success if all sub-services start properly
	err := services.StartManagerAndAwaitHealthy(ctx, d.subservices)
	if err != nil {
		return fmt.Errorf("failed to start subservices %w", err)
	}

	return nil
}

func (d *Distributor) running(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-d.subservicesWatcher.Chan():
		return fmt.Errorf("distributor subservices failed %w", err)
	}
}

// Called after distributor is asked to stop via StopAsync.
func (d *Distributor) stopping(_ error) error {
	return services.StopManagerAndAwaitStopped(context.Background(), d.subservices)
}

// Push a set of streams.
func (d *Distributor) Push(ctx context.Context, req *tempopb.PushRequest) (*tempopb.PushResponse, error) {
	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		// can't record discarded spans here b/c there's no tenant
		return nil, err
	}

	if d.cfg.LogReceivedTraces {
		logTraces(req.Batch)
	}

	// metric size
	size := req.Size()
	metricBytesIngested.WithLabelValues(userID).Add(float64(size))

	// metric spans
	if req.Batch == nil {
		return &tempopb.PushResponse{}, nil
	}
	spanCount := 0
	for _, ils := range req.Batch.InstrumentationLibrarySpans {
		spanCount += len(ils.Spans)
	}
	if spanCount == 0 {
		return &tempopb.PushResponse{}, nil
	}
	metricSpansIngested.WithLabelValues(userID).Add(float64(spanCount))

	// check limits
	now := time.Now()
	if !d.ingestionRateLimiter.AllowN(now, userID, req.Size()) {
		metricDiscardedSpans.WithLabelValues(reasonRateLimited, userID).Add(float64(spanCount))
		return nil, status.Errorf(codes.ResourceExhausted,
			"%s ingestion rate limit (%d bytes) exceeded while adding %d bytes",
			overrides.ErrorPrefixRateLimited,
			int(d.ingestionRateLimiter.Limit(now, userID)),
			req.Size())
	}

	keys, traces, ids, err := requestsByTraceID(req, userID, spanCount)
	if err != nil {
		metricDiscardedSpans.WithLabelValues(reasonInternalError, userID).Add(float64(spanCount))
		return nil, err
	}

	//var
	var searchData [][]byte
	if d.searchEnabled {
		searchData = extractSearchDataAll(traces, ids)
	}

	err = d.sendToIngestersViaBytes(ctx, userID, traces, searchData, keys, ids)
	if err != nil {
		recordDiscaredSpans(err, userID, spanCount)
	}

	return nil, err // PushRequest is ignored, so no reason to create one
}

func (d *Distributor) sendToIngestersViaBytes(ctx context.Context, userID string, traces []*tempopb.Trace, searchData [][]byte, keys []uint32, ids [][]byte) error {
	// Marshal to bytes once
	marshalledTraces := make([][]byte, len(traces))
	for i, t := range traces {
		b, err := t.Marshal()
		if err != nil {
			return errors.Wrap(err, "failed to marshal PushRequest")
		}
		marshalledTraces[i] = b
	}

	op := ring.WriteNoExtend
	if d.cfg.ExtendWrites {
		op = ring.Write
	}

	err := ring.DoBatch(ctx, op, d.ingestersRing, keys, func(ingester ring.InstanceDesc, indexes []int) error {
		localCtx, cancel := context.WithTimeout(context.Background(), d.clientCfg.RemoteTimeout)
		defer cancel()
		localCtx = user.InjectOrgID(localCtx, userID)

		req := tempopb.PushBytesRequest{
			Traces:     make([]tempopb.PreallocBytes, len(indexes)),
			Ids:        make([]tempopb.PreallocBytes, len(indexes)),
			SearchData: make([]tempopb.PreallocBytes, len(indexes)),
		}

		for i, j := range indexes {
			req.Traces[i].Slice = marshalledTraces[j][0:]
			req.Ids[i].Slice = ids[j]

			// Search data optional
			if len(searchData) > j {
				req.SearchData[i].Slice = searchData[j]
			}
		}

		c, err := d.pool.GetClientFor(ingester.Addr)
		if err != nil {
			return err
		}

		_, err = c.(tempopb.PusherClient).PushBytes(localCtx, &req)
		metricIngesterAppends.WithLabelValues(ingester.Addr).Inc()
		if err != nil {
			metricIngesterAppendFailures.WithLabelValues(ingester.Addr).Inc()
		}
		return err
	}, func() {})

	return err
}

// PushBytes Not used by the distributor
func (d *Distributor) PushBytes(context.Context, *tempopb.PushBytesRequest) (*tempopb.PushResponse, error) {
	return nil, nil
}

// Check implements the grpc healthcheck
func (*Distributor) Check(_ context.Context, _ *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

// requestsByTraceID takes an incoming tempodb.PushRequest and creates a set of keys for the hash ring
// and traces to pass onto the ingesters.
func requestsByTraceID(req *tempopb.PushRequest, userID string, spanCount int) ([]uint32, []*tempopb.Trace, [][]byte, error) {
	type traceAndID struct {
		id    []byte
		trace *tempopb.Trace
	}

	const tracesPerBatch = 20 // p50 of internal env
	tracesByID := make(map[uint32]*traceAndID, tracesPerBatch)
	spansByILS := make(map[uint32]*v1.InstrumentationLibrarySpans)

	for _, ils := range req.Batch.InstrumentationLibrarySpans {
		for _, span := range ils.Spans {
			traceID := span.TraceId
			if !validation.ValidTraceID(traceID) {
				return nil, nil, nil, status.Errorf(codes.InvalidArgument, "trace ids must be 128 bit")
			}

			traceKey := util.TokenFor(userID, traceID)
			ilsKey := traceKey
			if ils.InstrumentationLibrary != nil {
				ilsKey = fnv1a.AddString32(ilsKey, ils.InstrumentationLibrary.Name)
				ilsKey = fnv1a.AddString32(ilsKey, ils.InstrumentationLibrary.Version)
			}

			existingILS, ok := spansByILS[ilsKey]
			if !ok {
				existingILS = &v1.InstrumentationLibrarySpans{
					InstrumentationLibrary: ils.InstrumentationLibrary,
					Spans:                  make([]*v1.Span, 0, spanCount/tracesPerBatch),
				}
				spansByILS[ilsKey] = existingILS
			}
			existingILS.Spans = append(existingILS.Spans, span)

			// if we found an ILS we assume its already part of a request and can go to the next span
			if ok {
				continue
			}

			existingTrace, ok := tracesByID[traceKey]
			if !ok {
				existingTrace = &traceAndID{
					id: traceID,
					trace: &tempopb.Trace{
						Batches: make([]*v1.ResourceSpans, 0, spanCount/tracesPerBatch),
					},
				}

				tracesByID[traceKey] = existingTrace
			}

			existingTrace.trace.Batches = append(existingTrace.trace.Batches, &v1.ResourceSpans{
				Resource:                    req.Batch.Resource,
				InstrumentationLibrarySpans: []*v1.InstrumentationLibrarySpans{existingILS},
			})
		}
	}

	metricTracesPerBatch.Observe(float64(len(tracesByID)))

	keys := make([]uint32, 0, len(tracesByID))
	traces := make([]*tempopb.Trace, 0, len(tracesByID))
	ids := make([][]byte, 0, len(tracesByID))

	for k, r := range tracesByID {
		keys = append(keys, k)
		traces = append(traces, r.trace)
		ids = append(ids, r.id)
	}

	return keys, traces, ids, nil
}

func recordDiscaredSpans(err error, userID string, spanCount int) {
	s := status.Convert(err)
	if s == nil {
		return
	}
	desc := s.Message()

	if strings.HasPrefix(desc, overrides.ErrorPrefixLiveTracesExceeded) {
		metricDiscardedSpans.WithLabelValues(reasonLiveTracesExceeded, userID).Add(float64(spanCount))
	} else if strings.HasPrefix(desc, overrides.ErrorPrefixTraceTooLarge) {
		metricDiscardedSpans.WithLabelValues(reasonTraceTooLarge, userID).Add(float64(spanCount))
	} else {
		metricDiscardedSpans.WithLabelValues(reasonInternalError, userID).Add(float64(spanCount))
	}
}

func logTraces(batch *v1.ResourceSpans) {
	for _, ils := range batch.InstrumentationLibrarySpans {
		for _, s := range ils.Spans {
			level.Info(cortex_util.Logger).Log("msg", "received", "spanid", hex.EncodeToString(s.SpanId), "traceid", hex.EncodeToString(s.TraceId))
		}
	}
}
