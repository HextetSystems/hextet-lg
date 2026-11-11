package web

// M3.2: structured logging, Prometheus metrics, backend visibility.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/HextetSystems/hextet-lg/internal/config"
	"github.com/HextetSystems/hextet-lg/internal/obs"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// records parses every log line as JSON, failing the test on any line that is not.
func (s *syncBuf) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func find(recs []map[string]any, msg string, match ...any) map[string]any {
	for _, r := range recs {
		if r["msg"] != msg {
			continue
		}
		ok := true
		for i := 0; i+1 < len(match); i += 2 {
			if fmt.Sprint(r[match[i].(string)]) != fmt.Sprint(match[i+1]) {
				ok = false
			}
		}
		if ok {
			return r
		}
	}
	return nil
}

func newObsHandler(t *testing.T, lg config.LookingGlass, level string, specs ...backendSpec) (*Handler, *syncBuf) {
	t.Helper()
	cfg, closeAll := testConfig(lg, specs...)
	t.Cleanup(closeAll)
	buf := &syncBuf{}
	h, err := NewHandler(cfg, WithLogger(obs.NewLogger(buf, level, "json")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, buf
}

// metric finds one sample whose labels include all the given pairs.
func metric(t *testing.T, h *Handler, name string, labels ...string) (float64, bool) {
	t.Helper()
	fams, err := h.metrics.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatch(m, labels) {
				switch f.GetType() {
				case dto.MetricType_COUNTER:
					return m.GetCounter().GetValue(), true
				case dto.MetricType_GAUGE:
					return m.GetGauge().GetValue(), true
				case dto.MetricType_HISTOGRAM:
					return float64(m.GetHistogram().GetSampleCount()), true
				}
			}
		}
	}
	return 0, false
}

func labelsMatch(m *dto.Metric, want []string) bool {
	have := map[string]string{}
	for _, l := range m.GetLabel() {
		have[l.GetName()] = l.GetValue()
	}
	for i := 0; i+1 < len(want); i += 2 {
		if have[want[i]] != want[i+1] {
			return false
		}
	}
	return true
}

func wantMetric(t *testing.T, h *Handler, want float64, name string, labels ...string) {
	t.Helper()
	got, ok := metric(t, h, name, labels...)
	if !ok || got != want {
		t.Errorf("%s%v = %v (present=%v), want %v", name, labels, got, ok, want)
	}
}

func TestStructuredLogsAndRequestID(t *testing.T) {
	lg := defaults()
	lg.CacheTTL = time.Minute
	h, buf := newObsHandler(t, lg, "info", backendSpec{id: "a", name: "A", h: serveDoc(ribJSON(3, false))})
	routes := h.Routes()
	const secret = "192.0.2.77/32"
	rr := get(routes, "/?q="+secret+"&type=prefix", "X-Request-Id", "forged\nid", "Authorization", "Bearer topsecret")
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	id := rr.Header().Get("X-Request-Id")
	if len(id) != 16 || strings.Contains(id, "forged") {
		t.Fatalf("request id %q must be generated, never taken from the client", id)
	}
	recs := buf.records(t) // fails if any line is not JSON
	req := find(recs, "request", "request_id", id)
	if req == nil {
		t.Fatalf("no access record for %s in %s", id, buf.String())
	}
	for _, k := range []string{"time", "level", "msg", "method", "route", "status", "duration_ms", "bytes", "result", "backends", "cache_hits", "cache_misses"} {
		if _, ok := req[k]; !ok {
			t.Errorf("access record lacks %q: %v", k, req)
		}
	}
	if req["route"] != "index" || req["method"] != "GET" || req["status"].(float64) != 200 || req["result"] != "complete" || req["level"] != "INFO" {
		t.Errorf("unexpected access record: %v", req)
	}
	bq := find(recs, "backend query", "backend", "a")
	if bq == nil || bq["operation"] != "rib" || bq["status"] != "complete" || bq["request_id"] != id {
		t.Fatalf("backend record: %v", bq)
	}
	if _, ok := bq["backend_duration_ms"]; !ok {
		t.Errorf("backend record lacks duration: %v", bq)
	}
	for _, leak := range []string{"192.0.2.77", "topsecret", "Bearer", "forged", "127.0.0.1:"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("log contains %q: %s", leak, buf.String())
		}
	}
	// A second identical request is a cache hit and says so.
	get(routes, "/?q="+secret+"&type=prefix")
	if hit := find(buf.records(t), "request", "cache_hits", 1); hit == nil {
		t.Errorf("cache hit not reported: %s", buf.String())
	}
}

func TestLogLevelsFilter(t *testing.T) {
	h, buf := newObsHandler(t, defaults(), "warn", backendSpec{id: "a", name: "A", h: serveDoc(ribJSON(1, false))})
	get(h.Routes(), "/?q=10.0.0.0/24&type=prefix")
	if s := buf.String(); s != "" {
		t.Fatalf("warn level logged routine info: %s", s)
	}
	h2, buf2 := newObsHandler(t, defaults(), "debug", backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	get(h2.Routes(), "/healthz")
	if find(buf2.records(t), "request", "route", "healthz") == nil {
		t.Fatalf("debug level should include health checks: %s", buf2.String())
	}
	h3, buf3 := newObsHandler(t, defaults(), "info", backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	get(h3.Routes(), "/healthz")
	get(h3.Routes(), "/static/app.css")
	if buf3.String() != "" {
		t.Fatalf("health and static requests must not log at info: %s", buf3.String())
	}
}

func TestLongAndHostileValuesNeverLogged(t *testing.T) {
	h, buf := newObsHandler(t, defaults(), "debug", backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	long := strings.Repeat("A", 5000)
	get(h.Routes(), "/?type=prefix&q="+long)
	get(h.Routes(), "/no/such/path/"+long[:200])
	if strings.Contains(buf.String(), "AAAAAAAA") || strings.Contains(buf.String(), "no/such") {
		t.Fatalf("user supplied text reached the log: %.300s", buf.String())
	}
	if find(buf.records(t), "request", "route", "other") == nil {
		t.Fatalf("unknown path not normalized: %s", buf.String())
	}
}

func TestMetricsFamiliesAndValues(t *testing.T) {
	lg := defaults()
	lg.CacheTTL = time.Minute
	var calls sync.Map
	h, _ := newObsHandler(t, lg, "error",
		backendSpec{id: "good", name: "Good", h: serveDoc(ribJSON(5, false))},
		backendSpec{id: "bad", name: "Bad", h: func(w http.ResponseWriter, r *http.Request) { calls.Store("bad", true); http.Error(w, "boom", 500) }},
	)
	routes := h.Routes()
	get(routes, "/?q=10.0.0.0/24&type=prefix&backend=good")
	get(routes, "/?q=10.0.0.0/24&type=prefix&backend=good") // cache hit
	get(routes, "/?q=10.0.0.0/24&type=prefix&backend=bad")
	get(routes, "/?q=bad%20value&type=prefix") // 400
	get(routes, "/healthz")

	wantMetric(t, h, 1, "hextet_lg_backend_requests_total", "backend", "good", "operation", "rib", "status", "complete")
	wantMetric(t, h, 1, "hextet_lg_backend_requests_total", "backend", "bad", "operation", "rib", "status", "failed")
	wantMetric(t, h, 1, "hextet_lg_backend_failures_total", "backend", "bad", "operation", "rib", "reason", "backend_error")
	wantMetric(t, h, 1, "hextet_lg_backend_request_duration_seconds", "backend", "good", "operation", "rib")
	wantMetric(t, h, 0, "hextet_lg_backend_active_queries", "backend", "good")
	wantMetric(t, h, 1, "hextet_lg_cache_hits_total")
	wantMetric(t, h, 2, "hextet_lg_cache_misses_total")
	wantMetric(t, h, 1, "hextet_lg_cache_entries")
	wantMetric(t, h, 3, "hextet_lg_http_requests_total", "method", "GET", "route", "index", "status_class", "2xx")
	wantMetric(t, h, 1, "hextet_lg_http_requests_total", "route", "index", "status_class", "4xx")
	wantMetric(t, h, 1, "hextet_lg_http_requests_total", "route", "healthz", "status_class", "2xx")
	wantMetric(t, h, 0, "hextet_lg_http_active_requests")

	// The resource gauges are read from the authoritative counters.
	used, limit := h.budget.usage()
	wantMetric(t, h, float64(used), "hextet_lg_resource_retained_bytes")
	wantMetric(t, h, float64(limit), "hextet_lg_resource_memory_budget_bytes")
	wantMetric(t, h, float64(h.cache.used), "hextet_lg_cache_retained_bytes")
	wantMetric(t, h, float64(cap(h.work.active)), "hextet_lg_work_limit")
	for _, n := range []string{"hextet_lg_active_route_decodes", "hextet_lg_work_active", "hextet_lg_work_admission_waiters", "hextet_lg_cache_active_flights", "hextet_lg_resource_budget_denials_total", "hextet_lg_cache_evictions_total", "hextet_lg_cache_expirations_total", "hextet_lg_cache_shared_flight_reuse_total", "hextet_lg_shutting_down", "hextet_lg_query_queue_depth", "hextet_lg_cache_max_bytes", "hextet_lg_http_rate_limited_total"} {
		if n == "hextet_lg_http_rate_limited_total" {
			continue // vector without samples until a request is limited
		}
		if _, ok := metric(t, h, n); !ok {
			t.Errorf("metric family %s missing", n)
		}
	}
	wantMetric(t, h, 1, "hextet_lg_cache_admission_skips_total", "reason", "failed")
	wantMetric(t, h, 0, "hextet_lg_work_gate_rejections_total", "reason", "queue_full")

	// Backend visibility: good has a success, bad a failure with its reason.
	if v, ok := metric(t, h, "hextet_lg_backend_last_success_timestamp_seconds", "backend", "good"); !ok || time.Since(time.Unix(int64(v), 0)) > time.Minute {
		t.Errorf("last success: %v %v", v, ok)
	}
	if _, ok := metric(t, h, "hextet_lg_backend_last_failure_timestamp_seconds", "backend", "bad"); !ok {
		t.Error("last failure timestamp missing")
	}
	wantMetric(t, h, 1, "hextet_lg_backend_last_failure_info", "backend", "bad", "reason", "backend_error")
	if _, ok := metric(t, h, "hextet_lg_backend_last_failure_timestamp_seconds", "backend", "good"); ok {
		t.Error("healthy backend reports a failure")
	}

	// Go and process metrics come with the registry.
	for _, n := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if _, ok := metric(t, h, n); !ok {
			t.Errorf("runtime metric %s missing", n)
		}
	}
}

func TestMetricsExpositionFormat(t *testing.T) {
	h, _ := newObsHandler(t, defaults(), "error", backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	get(h.Routes(), "/?q=10.0.0.0/24&type=prefix")
	rr := get(h.MetricsHandler(), "/metrics")
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("scrape: %d %s", rr.Code, rr.Header().Get("Content-Type"))
	}
	body := rr.Body.String()
	for _, want := range []string{
		"# TYPE hextet_lg_http_requests_total counter",
		"# TYPE hextet_lg_http_request_duration_seconds histogram",
		"# TYPE hextet_lg_resource_retained_bytes gauge",
		`hextet_lg_backend_requests_total{backend="a",operation="rib",status="complete"} 1`,
		"hextet_lg_resource_memory_budget_bytes 8.05306368e+08",
		"# TYPE go_goroutines gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q", want)
		}
	}
	// Build identity: exactly one series, labelled only with fixed build data.
	var buildLines []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "hextet_lg_build_info{") {
			buildLines = append(buildLines, l)
		}
	}
	if len(buildLines) != 1 || !strings.HasSuffix(buildLines[0], "} 1") ||
		!strings.Contains(buildLines[0], `go_version="go`) || !strings.Contains(buildLines[0], `revision="`) || !strings.Contains(buildLines[0], `version="`) {
		t.Errorf("build info series = %q", buildLines)
	}
	if rr := get(h.MetricsHandler(), "/"); rr.Code != 404 {
		t.Errorf("management listener served %d for /", rr.Code)
	}
	req := httptest.NewRequest("POST", "/metrics", nil)
	rec := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Errorf("POST /metrics = %d", rec.Code)
	}
}

// The public handler must never serve metrics, whatever the client claims.
func TestMetricsNotOnPublicListener(t *testing.T) {
	h, _ := newObsHandler(t, defaults(), "error", backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	for _, hdr := range [][]string{nil, {"X-Forwarded-For", "127.0.0.1"}, {"X-Real-IP", "127.0.0.1", "Forwarded", "for=127.0.0.1"}, {"Accept", "application/openmetrics-text"}} {
		for _, path := range []string{"/metrics", "/metrics/", "/metrics?x=1", "/debug/vars", "/debug/pprof/"} {
			rr := get(h.Routes(), path, hdr...)
			if rr.Code == 200 || strings.Contains(rr.Body.String(), "hextet_lg_") || strings.Contains(rr.Body.String(), "go_goroutines") {
				t.Errorf("public %s %v -> %d", path, hdr, rr.Code)
			}
		}
	}
}

// Labels are drawn from fixed sets: nothing the client sends can create a series.
func TestMetricLabelsAreBounded(t *testing.T) {
	h, _ := newObsHandler(t, defaults(), "error", backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	routes := h.Routes()
	for i := 0; i < 20; i++ {
		get(routes, fmt.Sprintf("/?q=203.0.113.%d/32&type=prefix", i))
		get(routes, fmt.Sprintf("/?as=%d", 64500+i))
		get(routes, fmt.Sprintf("/x%d/..%%2f?k=v", i))
	}
	fams, _ := h.metrics.reg.Gather()
	series := 0
	for _, f := range fams {
		if !strings.HasPrefix(f.GetName(), "hextet_lg_") {
			continue
		}
		for _, m := range f.GetMetric() {
			series++
			for _, l := range m.GetLabel() {
				v := l.GetValue()
				if strings.Contains(v, "203.0.113") || strings.Contains(v, "6450") || strings.HasPrefix(v, "/") || strings.Contains(v, "x1") {
					t.Errorf("%s label %s=%q carries request data", f.GetName(), l.GetName(), v)
				}
			}
		}
	}
	if series > 400 {
		t.Errorf("%d application series for a one-backend instance", series)
	}
	if v, ok := metric(t, h, "hextet_lg_http_requests_total", "route", "other"); !ok || v != 20 {
		t.Errorf("unknown paths should aggregate under route=other: %v %v", v, ok)
	}
}

func TestBackendTimeoutIsClassifiedAndCounted(t *testing.T) {
	lg := defaults()
	lg.QueryTimeout = 150 * time.Millisecond
	h, buf := newObsHandler(t, lg, "info", backendSpec{id: "slow", name: "Slow", h: func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"rib":[`)
		flushW(w)
		<-r.Context().Done()
	}})
	rr := get(h.Routes(), "/?as=64500")
	if rr.Code != 503 && rr.Code != 504 && rr.Code != 502 && rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	wantMetric(t, h, 1, "hextet_lg_backend_failures_total", "backend", "slow", "reason", "timeout")
	wantMetric(t, h, 1, "hextet_lg_backend_last_failure_info", "backend", "slow", "reason", "timeout")
	rec := find(buf.records(t), "backend query", "backend", "slow")
	if rec == nil || rec["level"] != "WARN" || rec["reason"] != "timeout" {
		t.Fatalf("timeout record: %v", rec)
	}
	if v, _ := metric(t, h, "hextet_lg_backend_request_duration_seconds", "backend", "slow", "operation", "rib"); v != 1 {
		t.Errorf("duration histogram samples = %v", v)
	}
}

func TestOverviewBackendFailureIsRecorded(t *testing.T) {
	h, buf := newObsHandler(t, defaults(), "info", backendSpec{id: "a", name: "A", h: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }})
	get(h.Routes(), "/summary")
	wantMetric(t, h, 1, "hextet_lg_backend_failures_total", "backend", "a", "operation", "summary", "reason", "backend_error")
	if find(buf.records(t), "backend query", "operation", "summary", "level", "WARN") == nil {
		t.Fatalf("no warning: %s", buf.String())
	}
}

func TestRateLimitAndAdmissionCounters(t *testing.T) {
	lg := defaults()
	lg.MaxConcurrentWork, lg.MaxQueuedWork, lg.QueueWait = 1, 1, 20*time.Millisecond
	cfg, closeAll := testConfig(lg, backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	defer closeAll()
	cfg.Server.LiveRatePerSecond, cfg.Server.LiveRateBurst = 1, 2
	buf := &syncBuf{}
	h, err := NewHandler(cfg, WithLogger(obs.NewLogger(buf, "debug", "json")))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	routes := h.Routes()
	codes := map[int]int{}
	for i := 0; i < 5; i++ {
		codes[get(routes, "/?q=10.0.0.0/24&type=prefix").Code]++
	}
	if codes[429] == 0 {
		t.Fatalf("limiter never refused: %v", codes)
	}
	if v, _ := metric(t, h, "hextet_lg_http_rate_limited_total", "bucket", "live"); v != float64(codes[429]) {
		t.Errorf("rate limited counter %v, want %d", v, codes[429])
	}
	wantMetric(t, h, float64(codes[429]), "hextet_lg_http_requests_total", "route", "index", "status_class", "4xx")
	if find(buf.records(t), "request", "status", 429, "level", "DEBUG") == nil {
		t.Errorf("429 should be logged at debug: %s", buf.String())
	}

}
