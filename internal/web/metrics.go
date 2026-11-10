package web

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
	"github.com/HextetSystems/hextet-lg/internal/version"
)

// Backend operations are a fixed set so they can label metrics.
var backendOperations = []string{"rib", "rib_in", "rib_out", "neighbors", "summary"}

var backendStatuses = []bgp.Status{bgp.StatusComplete, bgp.StatusPartial, bgp.StatusFailed}

// durationBuckets cover fast prefix lookups through the 30 s default query
// timeout and longer per-backend overrides.
var durationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 30, 45, 60, 120}

// metrics holds the directly recorded instruments. State that already has an
// authoritative counter elsewhere (resource budget, work gate, cache) is not
// duplicated here; resourceCollector reads those counters at scrape time.
//
// No label ever carries a prefix, ASN, neighbor, client address, request id or
// query string: routes are fixed names, backends are configured ids, and
// operations and reasons come from fixed sets.
type metrics struct {
	reg *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	rateLimited  *prometheus.CounterVec
	clientAborts *prometheus.CounterVec

	backendRequests *prometheus.CounterVec
	backendDuration *prometheus.HistogramVec
	backendFailures *prometheus.CounterVec
	backendActive   *prometheus.GaugeVec
	queryRejected   *prometheus.CounterVec
}

func newMetrics(backendIDs []string) *metrics {
	m := &metrics{reg: prometheus.NewRegistry()}
	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hextet_lg_http_requests_total", Help: "HTTP requests by method, normalized route and status class. status_class client_closed means the client went away before the response completed; 5xx also covers deliberately aborted responses."}, []string{"method", "route", "status_class"})
	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "hextet_lg_http_request_duration_seconds", Help: "HTTP request duration, including time spent writing the response.", Buckets: durationBuckets}, []string{"method", "route"})
	m.rateLimited = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hextet_lg_http_rate_limited_total", Help: "Requests refused by the per-peer rate limiter (bucket page or live)."}, []string{"bucket"})
	m.clientAborts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hextet_lg_http_client_aborts_total", Help: "Requests whose client canceled, disconnected or stopped reading (write_timeout)."}, []string{"route", "kind"})
	m.backendRequests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hextet_lg_backend_requests_total", Help: "Upstream OpenBGPD requests that were attempted, by outcome (complete, partial, failed)."}, []string{"backend", "operation", "status"})
	m.backendDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "hextet_lg_backend_request_duration_seconds", Help: "Upstream request duration including streaming the whole response.", Buckets: durationBuckets}, []string{"backend", "operation"})
	m.backendFailures = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hextet_lg_backend_failures_total", Help: "Partial or failed upstream requests by bounded reason (timeout, canceled, connection_closed, invalid_json, backend_error, response_limit, memory_limit, capacity)."}, []string{"backend", "operation", "reason"})
	m.backendActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "hextet_lg_backend_active_queries", Help: "Upstream requests currently in flight."}, []string{"backend"})
	m.queryRejected = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hextet_lg_query_rejections_total", Help: "Upstream queries refused locally before reaching a backend, by reason (queue_full, slot_timeout, shutdown, canceled, budget)."}, []string{"reason"})
	for _, r := range []string{"queue_full", "slot_timeout", "shutdown", "canceled", "budget"} {
		m.queryRejected.WithLabelValues(r)
	}
	for _, id := range backendIDs {
		m.backendActive.WithLabelValues(id)
		for _, op := range backendOperations {
			m.backendDuration.WithLabelValues(id, op)
			for _, st := range backendStatuses {
				m.backendRequests.WithLabelValues(id, op, string(st))
			}
		}
	}
	// Build identity: one constant series per process, so cardinality is 1.
	// It carries only the release version, abbreviated revision and Go
	// toolchain, never configuration.
	vi := version.Get()
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{Name: "hextet_lg_build_info", Help: "Build identity of the running binary (value is always 1).", ConstLabels: prometheus.Labels{"version": vi.Version, "revision": vi.Commit, "go_version": vi.GoVersion}})
	buildInfo.Set(1)
	m.reg.MustRegister(buildInfo, m.httpRequests, m.httpDuration, m.rateLimited, m.clientAborts, m.backendRequests, m.backendDuration, m.backendFailures, m.backendActive, m.queryRejected,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// backendHealth is what the application last saw from one backend. It is
// updated by real traffic only; nothing polls the backend.
type backendHealth struct {
	mu         sync.Mutex
	lastOK     time.Time
	lastFail   time.Time
	lastReason bgp.Reason
}

// backendFault reports reasons that point at the backend or the path to it,
// as opposed to this instance's own limits and the client leaving.
func backendFault(r bgp.Reason) bool {
	switch r {
	case bgp.ReasonTimeout, bgp.ReasonConnectionClosed, bgp.ReasonInvalidJSON, bgp.ReasonBackendError:
		return true
	}
	return false
}

// operationName is the fixed metric/log label for an endpoint path.
func operationName(path string) string {
	return strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "_")
}

// recordBackend notes one finished upstream request. complete counts as a
// success and a backend fault (timeout, closed connection, bad JSON, error
// status) as a failure, whether or not partial routes survived. Local limits
// (response_limit, memory_limit, capacity) and cancellations touch neither
// timestamp: they say nothing about backend health.
func (h *Handler) recordBackend(b backendClient, op string, status bgp.Status, reason bgp.Reason, elapsed time.Duration) {
	m := h.metrics
	m.backendRequests.WithLabelValues(b.ID, op, string(status)).Inc()
	m.backendDuration.WithLabelValues(b.ID, op).Observe(elapsed.Seconds())
	if reason != bgp.ReasonNone {
		m.backendFailures.WithLabelValues(b.ID, op, string(reason)).Inc()
	}
	hl := h.health[b.ID]
	hl.mu.Lock()
	defer hl.mu.Unlock()
	switch {
	case status == bgp.StatusComplete:
		hl.lastOK = time.Now()
	case backendFault(reason):
		hl.lastFail, hl.lastReason = time.Now(), reason
	}
}

// resourceCollector exposes the authoritative M3.1 counters at scrape time.
// Every read is O(1): no cache scan, no copying.
type resourceCollector struct{ h *Handler }

var (
	dRetained   = prometheus.NewDesc("hextet_lg_resource_retained_bytes", "Accounted retained route bytes (aggregate budget in use: cache, flights, renderers, overview reservations).", nil, nil)
	dBudget     = prometheus.NewDesc("hextet_lg_resource_memory_budget_bytes", "Configured aggregate retained-memory budget (looking_glass.max_total_retained_bytes).", nil, nil)
	dDenied     = prometheus.NewDesc("hextet_lg_resource_budget_denials_total", "Reservations refused because the aggregate budget was exhausted.", nil, nil)
	dDecodes    = prometheus.NewDesc("hextet_lg_active_route_decodes", "Upstream route queries holding an instance-wide query slot.", nil, nil)
	dQueue      = prometheus.NewDesc("hextet_lg_query_queue_depth", "Queries active or waiting in the upstream-query queue.", nil, nil)
	dWork       = prometheus.NewDesc("hextet_lg_work_active", "Admitted backend-driven requests (decode and render, including cache hits and slow writers).", nil, nil)
	dWorkLimit  = prometheus.NewDesc("hextet_lg_work_limit", "Configured maximum concurrent admitted work.", nil, nil)
	dWaiters    = prometheus.NewDesc("hextet_lg_work_admission_waiters", "Requests waiting for an admission slot.", nil, nil)
	dRejections = prometheus.NewDesc("hextet_lg_work_gate_rejections_total", "Requests refused by the work gate, by reason.", []string{"reason"}, nil)
	dFlights    = prometheus.NewDesc("hextet_lg_cache_active_flights", "Shared single-flight upstream queries running now.", nil, nil)
	dShutdown   = prometheus.NewDesc("hextet_lg_shutting_down", "1 once shutdown has begun and admission is closed.", nil, nil)
	dActiveHTTP = prometheus.NewDesc("hextet_lg_http_active_requests", "HTTP requests currently being served.", nil, nil)

	dHits       = prometheus.NewDesc("hextet_lg_cache_hits_total", "Result cache hits.", nil, nil)
	dMisses     = prometheus.NewDesc("hextet_lg_cache_misses_total", "Result cache misses (includes requests that joined an in-flight query).", nil, nil)
	dEntries    = prometheus.NewDesc("hextet_lg_cache_entries", "Entries resident in the result cache.", nil, nil)
	dCacheBytes = prometheus.NewDesc("hextet_lg_cache_retained_bytes", "Estimated bytes held by the result cache (a subset of the aggregate budget).", nil, nil)
	dCacheMax   = prometheus.NewDesc("hextet_lg_cache_max_bytes", "Configured result cache budget.", nil, nil)
	dEvictions  = prometheus.NewDesc("hextet_lg_cache_evictions_total", "Entries evicted early to make room (least recently used).", nil, nil)
	dExpired    = prometheus.NewDesc("hextet_lg_cache_expirations_total", "Entries removed because their TTL elapsed.", nil, nil)
	dSharedFl   = prometheus.NewDesc("hextet_lg_cache_shared_flight_reuse_total", "Requests that joined another request's identical in-flight query.", nil, nil)
	dSkips      = prometheus.NewDesc("hextet_lg_cache_admission_skips_total", "Finished results not stored in the cache, by reason (disabled, failed, capacity, transient, abandoned, too_large).", []string{"reason"}, nil)

	dLastOK     = prometheus.NewDesc("hextet_lg_backend_last_success_timestamp_seconds", "Unix time of the last complete upstream request (absent until one happens).", []string{"backend"}, nil)
	dLastFail   = prometheus.NewDesc("hextet_lg_backend_last_failure_timestamp_seconds", "Unix time of the last upstream request that failed for a backend-side reason (absent until one happens).", []string{"backend"}, nil)
	dLastReason = prometheus.NewDesc("hextet_lg_backend_last_failure_info", "Always 1; the reason label is the most recent backend-side failure reason.", []string{"backend", "reason"}, nil)
)

func (c resourceCollector) Describe(ch chan<- *prometheus.Desc) { prometheus.DescribeByCollect(c, ch) }

func (c resourceCollector) Collect(ch chan<- prometheus.Metric) {
	h := c.h
	g := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }
	ctr := func(d *prometheus.Desc, v float64, lv ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, lv...)
	}
	used, limit := h.budget.usage()
	g(dRetained, float64(used))
	g(dBudget, float64(limit))
	ctr(dDenied, float64(h.budget.denied.Load()))
	g(dDecodes, float64(len(h.expensive)))
	g(dQueue, float64(len(h.queue)))
	g(dWork, float64(len(h.work.active)))
	g(dWorkLimit, float64(cap(h.work.active)))
	g(dWaiters, float64(len(h.work.queue)))
	for i := range h.work.rejected {
		ctr(dRejections, float64(h.work.rejected[i].Load()), gateReasonNames[i])
	}
	entries, bytes, flights := h.cache.snapshot()
	g(dFlights, float64(flights))
	g(dEntries, float64(entries))
	g(dCacheBytes, float64(bytes))
	g(dCacheMax, float64(h.cache.maxBytes))
	ctr(dHits, float64(h.cache.hits.Load()))
	ctr(dMisses, float64(h.cache.misses.Load()))
	ctr(dEvictions, float64(h.cache.evictions.Load()))
	ctr(dExpired, float64(h.cache.expirations.Load()))
	ctr(dSharedFl, float64(h.cache.shared.Load()))
	for i := range h.cache.skips {
		ctr(dSkips, float64(h.cache.skips[i].Load()), skipReasonNames[i])
	}
	stopping := 0.0
	if h.stopping.Load() {
		stopping = 1
	}
	g(dShutdown, stopping)
	g(dActiveHTTP, float64(h.activeHTTP.Load()))
	for _, b := range h.backends {
		hl := h.health[b.ID]
		hl.mu.Lock()
		ok, fail, reason := hl.lastOK, hl.lastFail, hl.lastReason
		hl.mu.Unlock()
		if !ok.IsZero() {
			g2(ch, dLastOK, float64(ok.UnixNano())/1e9, b.ID)
		}
		if !fail.IsZero() {
			g2(ch, dLastFail, float64(fail.UnixNano())/1e9, b.ID)
			g2(ch, dLastReason, 1, b.ID, string(reason))
		}
	}
}

func g2(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, lv ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, lv...)
}

// MetricsHandler serves /metrics for the separate management listener. It is
// never part of Routes(), so the public listener cannot expose it.
func (h *Handler) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(h.metrics.reg, promhttp.HandlerOpts{MaxRequestsInFlight: 4, Timeout: 10 * time.Second}))
	return mux
}
