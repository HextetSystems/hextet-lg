package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

// ---- helpers ----------------------------------------------------------------

var generatedRE = regexp.MustCompile(`<span id="page-timing" class="timing"[^>]*> · Generated in ([^<]+)</span>`)

// generatedIn extracts the footer duration from a body. ok is false when the
// body has no timing indicator.
func generatedIn(t *testing.T, body string) (time.Duration, bool) {
	t.Helper()
	m := generatedRE.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	txt := html_unescape(m[1])
	if txt == "<0.1 ms" {
		return 0, true
	}
	f := strings.Fields(txt)
	if len(f) != 2 {
		t.Fatalf("malformed duration %q", txt)
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		t.Fatalf("malformed duration %q", txt)
	}
	switch f[1] {
	case "ms":
		return time.Duration(v * float64(time.Millisecond)), true
	case "s":
		return time.Duration(v * float64(time.Second)), true
	}
	t.Fatalf("unknown unit in %q", txt)
	return 0, false
}

func html_unescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

func m48Handler(t *testing.T, edit func(*config.Config), specs ...backendSpec) http.Handler {
	t.Helper()
	return m47Handler(t, edit, specs...)
}

func delayed(d time.Duration, n int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
		fmt.Fprint(w, ribJSON(n, false))
	}
}

func timingCount(body string) int { return strings.Count(body, `id="page-timing"`) }

// ---- formatting ---------------------------------------------------------------

func TestFormatGenerated(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "<0.1 ms"},
		{40 * time.Microsecond, "<0.1 ms"},
		{50 * time.Microsecond, "0.1 ms"},
		{420 * time.Microsecond, "0.4 ms"},
		{8400 * time.Microsecond, "8.4 ms"},
		{42700 * time.Microsecond, "42.7 ms"},
		{99940 * time.Microsecond, "99.9 ms"},
		{99960 * time.Microsecond, "100 ms"}, // rounds up into the next tier
		{836 * time.Millisecond, "836 ms"},
		{999400 * time.Microsecond, "999 ms"},
		{999600 * time.Microsecond, "1.00 s"},
		{1240 * time.Millisecond, "1.24 s"},
		{9994 * time.Millisecond, "9.99 s"},
		{9996 * time.Millisecond, "10.0 s"},
		{12340 * time.Millisecond, "12.3 s"},
		{99960 * time.Millisecond, "100 s"},
		{134 * time.Second, "134 s"},
		{-time.Second, ""},
	} {
		if got := FormatGenerated(tc.d); got != tc.want {
			t.Errorf("FormatGenerated(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
	// Monotonic in the input and never malformed across a sweep.
	re := regexp.MustCompile(`^(<0\.1 ms|\d+\.\d ms|\d+ ms|\d+\.\d\d s|\d+\.\d s|\d+ s)$`)
	for d := time.Duration(0); d < 3*time.Minute; d += 337 * time.Microsecond * 13 {
		if s := FormatGenerated(d); !re.MatchString(s) {
			t.Fatalf("FormatGenerated(%v) = %q", d, s)
		}
	}
}

func TestPageTimingOmittedWithoutStart(t *testing.T) {
	if got := pageTiming(time.Time{}); got != "" {
		t.Fatalf("no start must give no value, got %q", got)
	}
	if got := pageTiming(time.Now().Add(-1500 * time.Millisecond)); !strings.HasSuffix(got, " s") {
		t.Fatalf("got %q", got)
	}
}

// ---- configuration ------------------------------------------------------------

func TestPageTimingFlag(t *testing.T) {
	for name, tc := range map[string]struct {
		flag *bool
		want bool
	}{"omitted": {nil, true}, "enabled": {b3(true), true}, "disabled": {b3(false), false}} {
		t.Run(name, func(t *testing.T) {
			h := m48Handler(t, func(c *config.Config) { c.Features.PageTiming = tc.flag }, backendSpec{"a", "Alpha", delayed(0, 2)})
			for _, path := range []string{"/", "/?q=10.0.0.0/24&type=prefix&backend=a", "/?q=bad", "/communities", "/neighbors", "/summary", "/rtr"} {
				rr := get(h, path)
				_, ok := generatedIn(t, rr.Body.String())
				if ok != tc.want || timingCount(rr.Body.String()) > 1 {
					t.Errorf("%s: status %d, timing present=%v (want %v), count=%d", path, rr.Code, ok, tc.want, timingCount(rr.Body.String()))
				}
				if !tc.want && strings.Contains(rr.Body.String(), "Generated in") {
					t.Errorf("%s: disabled timing left text behind", path)
				}
				// The rest of the footer is untouched either way.
				if rr.Code != 404 && !strings.Contains(rr.Body.String(), "Powered by OpenBGPD") {
					t.Errorf("%s: footer lost", path)
				}
			}
			// The HTMX fragment likewise carries nothing when disabled.
			rr := get(h, "/?q=10.0.0.0/24&type=prefix&backend=a", "HX-Request", "true")
			if (timingCount(rr.Body.String()) > 0) != tc.want {
				t.Errorf("fragment timing = %v, want %v", timingCount(rr.Body.String()) > 0, tc.want)
			}
		})
	}
}

// ---- HTML pages ---------------------------------------------------------------

func TestTimingOnEveryHTMLPageExactlyOnce(t *testing.T) {
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rtr":
			http.NotFound(w, r) // unsupported backend
		case "/neighbors":
			fmt.Fprint(w, m47Neighbors)
		case "/summary":
			fmt.Fprint(w, m47Summary)
		default:
			fmt.Fprint(w, ribJSON(3, false))
		}
	}})
	for name, path := range map[string]string{
		"route explorer":    "/",
		"search results":    "/?type=prefix&q=10.0.0.0/24&backend=a",
		"text view":         "/?type=prefix&q=10.0.0.0/24&backend=a&view=text",
		"empty result page": "/?type=prefix&q=10.0.0.0/24&backend=nope",
		"invalid request":   "/?type=prefix&q=not-a-prefix",
		"unknown parameter": "/?bogus=1",
		"summary":           "/summary",
		"neighbors":         "/neighbors",
		"rtr unsupported":   "/rtr",
		"communities":       "/communities",
	} {
		t.Run(name, func(t *testing.T) {
			rr := get(h, path)
			body := rr.Body.String()
			d, ok := generatedIn(t, body)
			if !ok || timingCount(body) != 1 {
				t.Fatalf("status %d: timing present=%v count=%d", rr.Code, ok, timingCount(body))
			}
			if d < 0 || d > 5*time.Second {
				t.Errorf("implausible duration %v", d)
			}
			if !strings.Contains(body, "Read-only queries") || !strings.Contains(body, "Powered by Hextet LG") || strings.Contains(body, "AS395089") {
				t.Errorf("generic footer content wrong or operator-specific text present")
			}
			if strings.Contains(body, `hx-swap-oob`) {
				t.Errorf("a full page must not carry an out-of-band update")
			}
		})
	}
}

func TestInvalidRequestKeepsStatusAndGetsFooter(t *testing.T) {
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", delayed(0, 1)})
	rr := get(h, "/?type=prefix&q=not-a-prefix")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
	if _, ok := generatedIn(t, rr.Body.String()); !ok {
		t.Fatal("error page has no timing")
	}
}

func TestTimingIsEscapedAndOnlyDurationText(t *testing.T) {
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", delayed(0, 1)})
	body := get(h, "/communities?q=%3Cscript%3Ealert(1)%3C/script%3E&category=%22%3E%3Cb%3E").Body.String()
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatal("query reflected unescaped")
	}
	m := generatedRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no timing")
	}
	// The sub-millisecond form contains '<'; it must be escaped in HTML.
	if strings.Contains(m[1], "<") || strings.Contains(m[1], `"`) {
		t.Errorf("unescaped markup in duration: %q", m[1])
	}
	if !regexp.MustCompile(`^(&lt;0\.1 ms|\d+(\.\d+)? (ms|s))$`).MatchString(m[1]) {
		t.Errorf("duration is not just a formatted duration: %q", m[1])
	}
	// Nothing but the duration is exposed: no nanoseconds, no timestamps.
	for _, bad := range []string{"ns", "µs", "monotonic", "m=+"} {
		if strings.Contains(m[0], bad) {
			t.Errorf("timing leaks %q: %s", bad, m[0])
		}
	}
}

// ---- measurement boundaries ---------------------------------------------------

// accessLog collects the structured access-log records written by protect.
type accessLog struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	recs []map[string]any
}

func (a *accessLog) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.Write(p)
}

func (a *accessLog) durations(route string) []float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []float64
	for _, line := range bytes.Split(a.buf.Bytes(), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["msg"] == "request" && m["route"] == route {
			out = append(out, m["duration_ms"].(float64))
		}
	}
	return out
}

func handlerWithLog(t *testing.T, edit func(*config.Config), specs ...backendSpec) (http.Handler, *accessLog) {
	t.Helper()
	cfg, closeAll := testConfig(defaults(), specs...)
	t.Cleanup(closeAll)
	if edit != nil {
		edit(&cfg)
	}
	al := &accessLog{}
	h, err := NewHandler(cfg, WithLogger(slog.New(slog.NewJSONHandler(al, nil))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h.Routes(), al
}

// The footer value must lie between the backend delay (backend latency is
// included) and the request duration the access log and HTTP metric record
// from the same start instant (the footer is read just before the handler
// returns, so it can never exceed it), and close to it: the part after the
// footer is a handful of bytes.
func TestFooterDurationMatchesDocumentedBoundary(t *testing.T) {
	const delay = 150 * time.Millisecond
	h, al := handlerWithLog(t, nil, backendSpec{"a", "Alpha", delayed(delay, 5)})
	body := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a").Body.String()
	d, ok := generatedIn(t, body)
	if !ok {
		t.Fatal("no timing")
	}
	if d < delay {
		t.Errorf("footer %v is below the backend latency %v: backend wait not included", d, delay)
	}
	logged := al.durations("index")
	if len(logged) != 1 {
		t.Fatalf("access-log records: %v", logged)
	}
	loggedD := time.Duration(logged[0] * float64(time.Millisecond))
	// The display rounds to 0.1 ms (or 1 ms above 100 ms), allow for it.
	if d > loggedD+time.Millisecond {
		t.Errorf("footer %v exceeds the whole request %v", d, loggedD)
	}
	if loggedD-d > 25*time.Millisecond {
		t.Errorf("footer %v is far below the whole request %v: it does not cover template generation", d, loggedD)
	}
}

// The clock starts with protect(), not at the page handler: time already spent
// before a handler runs (limits, rate limiting, admission) counts.
func TestFooterStartsAtRequestStart(t *testing.T) {
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", delayed(0, 1)})
	defer closeAll()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for path, handler := range map[string]http.HandlerFunc{"/communities": h.communityPage, "/": h.index} {
		req := httptest.NewRequest("GET", path, nil)
		info := &reqInfo{id: "x", start: time.Now().Add(-3 * time.Second)}
		req = req.WithContext(withInfo(req.Context(), info))
		rr := httptest.NewRecorder()
		handler(rr, req)
		d, ok := generatedIn(t, rr.Body.String())
		if !ok || d < 3*time.Second || d > 4*time.Second {
			t.Errorf("%s: footer %v (present=%v), want ≈3 s measured from the recorded start", path, d, ok)
		}
	}
	// No recorded start (not behind protect): omit rather than invent a value.
	rr := httptest.NewRecorder()
	h.communityPage(rr, httptest.NewRequest("GET", "/communities", nil))
	if _, ok := generatedIn(t, rr.Body.String()); ok || strings.Contains(rr.Body.String(), "Generated in") {
		t.Error("a page without a request start must omit the indicator")
	}
}

// ---- caching and shared flights -------------------------------------------------

func TestCacheHitMeasuresThisRequestNotTheOriginalQuery(t *testing.T) {
	const delay = 250 * time.Millisecond
	var calls int
	var mu sync.Mutex
	h := m48Handler(t, func(c *config.Config) { c.LookingGlass.CacheTTL = 30 * time.Second }, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		delayed(delay, 5)(w, r)
	}})
	const path = "/?type=prefix&q=10.0.0.0/24&backend=a"
	miss := get(h, path).Body.String()
	hit := get(h, path).Body.String()
	missD, ok1 := generatedIn(t, miss)
	hitD, ok2 := generatedIn(t, hit)
	if !ok1 || !ok2 {
		t.Fatal("missing timing")
	}
	if calls != 1 {
		t.Fatalf("backend calls = %d, want 1 (second request must be a cache hit)", calls)
	}
	if missD < delay {
		t.Errorf("cache miss %v must include the backend wait %v", missD, delay)
	}
	if hitD >= delay/2 {
		t.Errorf("cache hit %v looks like the original query (%v), not this request", hitD, delay)
	}
	if !strings.Contains(hit, "cached ") {
		t.Error("second response should be marked cached")
	}
	// The status line still reports the original query's backend time; it is a
	// different, labelled measurement and must not be what the footer shows.
	if !strings.Contains(hit, "Generated in") {
		t.Error("footer missing")
	}
}

func TestSharedFlightWaiterMeasuresItsOwnWait(t *testing.T) {
	const delay = 600 * time.Millisecond
	h := m48Handler(t, func(c *config.Config) { c.LookingGlass.CacheTTL = 30 * time.Second }, backendSpec{"a", "Alpha", delayed(delay, 3)})
	const path = "/?type=prefix&q=10.0.0.0/24&backend=a"
	first := make(chan time.Duration, 1)
	go func() {
		d, _ := generatedIn(t, get(h, path).Body.String())
		first <- d
	}()
	time.Sleep(300 * time.Millisecond)
	second, ok := generatedIn(t, get(h, path).Body.String())
	firstD := <-first
	if !ok {
		t.Fatal("no timing")
	}
	if firstD < delay {
		t.Errorf("first %v < backend delay", firstD)
	}
	// The joiner only waited for the remainder of the flight.
	if second >= firstD-100*time.Millisecond || second < 100*time.Millisecond {
		t.Errorf("joiner %v should reflect its own wait, shorter than the leader's %v", second, firstD)
	}
}

func TestMultipleBackendsAndPartialFailure(t *testing.T) {
	slow := 200 * time.Millisecond
	h := m48Handler(t, func(c *config.Config) { c.LookingGlass.QueryTimeout = 600 * time.Millisecond },
		backendSpec{"a", "Alpha", delayed(slow, 3)},
		backendSpec{"b", "Beta", delayed(20*time.Millisecond, 3)},
		backendSpec{"c", "Gamma", partialBackend(3)},
	)
	rr := get(h, "/?type=prefix&q=10.0.0.0/24&backend=all")
	body := rr.Body.String()
	d, ok := generatedIn(t, body)
	if !ok || timingCount(body) != 1 {
		t.Fatalf("status %d present=%v count=%d", rr.Code, ok, timingCount(body))
	}
	// Backends run in parallel: the page waits for the slowest, once.
	// The partial backend streams routes and then stalls until its 600 ms
	// timeout; the page waits for that one backend once, not for the sum.
	if d < 600*time.Millisecond || d > 1200*time.Millisecond {
		t.Errorf("footer %v, want ≈ the slowest backend's 600 ms timeout, not the sum of all backends", d)
	}
	_ = slow
	if !strings.Contains(body, "Incomplete results") {
		t.Error("expected an incomplete-results notice (partial backend)")
	}
}

func TestEmptyResultAndTimeoutKeepStatusAndFooter(t *testing.T) {
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", serveDoc("{}")})
	rr := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "No matching routes") {
		t.Fatalf("status %d", rr.Code)
	}
	if _, ok := generatedIn(t, rr.Body.String()); !ok {
		t.Error("empty result has no timing")
	}

	h = m48Handler(t, func(c *config.Config) { c.LookingGlass.QueryTimeout = 100 * time.Millisecond },
		backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) { time.Sleep(400 * time.Millisecond) }})
	rr = get(h, "/?type=prefix&q=10.0.0.0/24&backend=a")
	if rr.Code != http.StatusGatewayTimeout && rr.Code != http.StatusBadGateway && rr.Code != 200 && rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status %d", rr.Code)
	}
	want := rr.Code
	d, ok := generatedIn(t, rr.Body.String())
	if !ok || d < 90*time.Millisecond || d > 350*time.Millisecond {
		t.Errorf("timeout page: footer %v present=%v, want ≈ the 100 ms timeout", d, ok)
	}
	if again := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a"); again.Code != want {
		t.Errorf("status changed between identical requests: %d vs %d", again.Code, want)
	}
}

func TestStatusCodesUnchangedByTimingFlag(t *testing.T) {
	paths := []string{"/", "/?type=prefix&q=bad", "/?type=prefix&q=10.0.0.0/24&backend=a", "/?bogus=1", "/communities", "/summary", "/rtr", "/nope"}
	codes := func(flag *bool) []int {
		h := m48Handler(t, func(c *config.Config) { c.Features.PageTiming = flag }, backendSpec{"a", "Alpha", delayed(0, 2)})
		var out []int
		for _, p := range paths {
			out = append(out, get(h, p).Code)
		}
		return out
	}
	on, off := codes(b3(true)), codes(b3(false))
	for i := range paths {
		if on[i] != off[i] {
			t.Errorf("%s: %d with timing, %d without", paths[i], on[i], off[i])
		}
	}
}

// ---- HTMX ---------------------------------------------------------------------

func TestHTMXFragmentCarriesOutOfBandTiming(t *testing.T) {
	const delay = 120 * time.Millisecond
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", delayed(delay, 4)})
	rr := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a", "HX-Request", "true")
	body := rr.Body.String()
	if strings.Contains(body, "<footer") || strings.Contains(body, "<html") {
		t.Fatal("fragment must not contain the page chrome")
	}
	if timingCount(body) != 1 || !strings.Contains(body, `id="page-timing" class="timing" hx-swap-oob="true"`) {
		t.Fatalf("fragment must carry exactly one out-of-band timing element:\n%s", body[max(0, len(body)-400):])
	}
	// It is the last thing in the fragment, top-level (not inside results markup).
	tail := body[strings.LastIndex(body, `<span id="page-timing"`):]
	if !strings.HasSuffix(strings.TrimSpace(tail), "</span>") || strings.Count(tail, "<") != 2 {
		t.Errorf("out-of-band element is not a self-contained trailing span: %q", tail)
	}
	d, ok := generatedIn(t, body)
	if !ok || d < delay {
		t.Errorf("fragment timing %v (present=%v) must include backend latency %v", d, ok, delay)
	}
	if rr.Header().Get("HX-Push-Url") == "" {
		t.Error("HX-Push-Url lost")
	}
}

// Two consecutive searches each report their own duration, in the same response
// as their results: a response can never deliver another request's timing.
func TestConsecutiveHTMXSearchesReportTheirOwnDuration(t *testing.T) {
	h := m48Handler(t, nil,
		backendSpec{"slow", "Slow", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			fmt.Fprint(w, ribJSON(2, false))
		}},
		backendSpec{"fast", "Fast", delayed(0, 2)},
	)
	slow := get(h, "/?type=prefix&q=10.0.0.0/24&backend=slow", "HX-Request", "true").Body.String()
	fast := get(h, "/?type=prefix&q=10.0.0.0/24&backend=fast", "HX-Request", "true").Body.String()
	slowD, _ := generatedIn(t, slow)
	fastD, ok := generatedIn(t, fast)
	if !ok || slowD < 300*time.Millisecond || fastD >= 200*time.Millisecond {
		t.Errorf("slow=%v fast=%v", slowD, fastD)
	}
	if !strings.Contains(slow, "Slow") || strings.Contains(slow, ">Fast") || !strings.Contains(fast, "Fast") {
		t.Error("timing must travel in the same response as its results")
	}
}

// A history-restore request is answered with a full page (new, accurate
// measurement) and never with an out-of-band fragment.
func TestHistoryRestoreRequestGetsFullPageTiming(t *testing.T) {
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", delayed(0, 2)})
	rr := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a", "HX-Request", "true", "HX-History-Restore-Request", "true")
	body := rr.Body.String()
	if !strings.Contains(body, "<footer>") || strings.Contains(body, "hx-swap-oob") || timingCount(body) != 1 {
		t.Fatalf("history restore must return the full page with one in-footer timing (count %d)", timingCount(body))
	}
	if _, ok := generatedIn(t, body); !ok {
		t.Fatal("no timing")
	}
}

// An unsuccessful measurement still clears a previous value in the browser
// instead of leaving a stale one: the fragment then carries a hidden, empty
// replacement.
func TestFragmentWithoutStartClearsStaleTiming(t *testing.T) {
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", delayed(0, 1)})
	defer closeAll()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	req := httptest.NewRequest("GET", "/?type=prefix&q=10.0.0.0/24&backend=a", nil)
	req.Header.Set("HX-Request", "true") // no reqInfo in the context: no recorded start
	rr := httptest.NewRecorder()
	h.index(rr, req)
	body := rr.Body.String()
	if strings.Contains(body, "Generated in") || !strings.Contains(body, `<span id="page-timing" class="timing" hx-swap-oob="true" hidden></span>`) {
		t.Fatalf("expected a hidden clearing element, got tail: %q", body[max(0, len(body)-200):])
	}
}

// ---- coexistence with enrichment and non-HTML output ----------------------------

// DNS enrichment is asynchronous: its lookups are blocked for the whole test
// and the page, with its timing, still completes without waiting for them.
func TestTimingExcludesAndCoexistsWithAsyncEnrichment(t *testing.T) {
	release := make(chan struct{}) // never closed before the test ends: every lookup blocks
	defer close(release)
	calls := make(chan string, 20)
	doc := `{"rib":[{"prefix":"8.8.8.0/24","aspath":"174 13335","true_nexthop":"192.0.2.1","exit_nexthop":"192.0.2.1"}]}`
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", serveDoc(doc)})
	defer closeAll()
	cfg.ASNEnrichment = config.ASNDefaults()
	cfg.ASNEnrichment.Enabled = true
	cfg.PTREnrichment = config.PTRDefaults()
	cfg.PTREnrichment.Enabled = true
	h, err := NewHandler(cfg, WithASNResolver(mockASNResolver{release, calls}), WithPTRResolver(mockPTR{calls, release}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rt := h.Routes()
	const path = "/?type=prefix&q=8.8.8.0%2F24&backend=a"
	for name, hdr := range map[string][]string{"full": nil, "fragment": {"HX-Request", "true"}} {
		start := time.Now()
		body := get(rt, path, hdr...).Body.String()
		wall := time.Since(start)
		d, ok := generatedIn(t, body)
		if !ok || timingCount(body) != 1 {
			t.Fatalf("%s: timing present=%v count=%d", name, ok, timingCount(body))
		}
		if wall > time.Second || d > wall+time.Millisecond { // the display rounds to 0.1 ms
			t.Errorf("%s: page waited for DNS (wall %v, footer %v)", name, wall, d)
		}
		if !strings.Contains(body, `data-asn="13335"`) || !strings.Contains(body, `data-ptr="192.0.2.1"`) {
			t.Errorf("%s: enrichment markers missing", name)
		}
		// The enrichment loop writes `title` on [data-asn]/[data-ptr] nodes only;
		// the timing element is neither and carries a fixed, static title.
		el := generatedRE.FindString(body)
		if strings.Contains(el, "data-asn") || strings.Contains(el, "data-ptr") {
			t.Errorf("%s: timing element must not be an enrichment target", name)
		}
	}
}

func TestNonHTMLOutputIsUnchanged(t *testing.T) {
	var cfgFlag *bool
	build := func() http.Handler {
		return m48Handler(t, func(c *config.Config) { c.Features.PageTiming = cfgFlag }, backendSpec{"a", "Alpha", delayed(0, 3)})
	}
	on := build()
	cfgFlag = b3(false)
	off := build()
	for _, path := range []string{
		"/?type=prefix&q=10.0.0.0/24&backend=a&view=text&output=plain",
		"/?type=prefix&q=10.0.0.0/24&backend=a&view=detail&output=plain",
		"/healthz", "/readyz", "/static/app.css", "/static/app.js", "/asn-status?asn=13335", "/ptr-status?ip=192.0.2.1",
	} {
		a, b := get(on, path), get(off, path)
		if a.Code != b.Code || a.Body.String() != b.Body.String() {
			t.Errorf("%s: output differs with the flag (status %d/%d)", path, a.Code, b.Code)
		}
		if strings.Contains(a.Body.String(), "Generated in") || strings.Contains(a.Body.String(), "page-timing") {
			t.Errorf("%s: machine-readable output carries timing", path)
		}
		for k := range a.Header() {
			if strings.Contains(strings.ToLower(k), "timing") || strings.EqualFold(k, "Server-Timing") {
				t.Errorf("%s: unexpected timing header %s", path, k)
			}
		}
	}
}

func TestMetricsOutputHasNoTimingAndHTTPMetricsStillRecorded(t *testing.T) {
	for _, flag := range []*bool{b3(true), b3(false)} {
		cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", delayed(0, 2)})
		cfg.Features.PageTiming = flag
		h, err := NewHandler(cfg)
		if err != nil {
			t.Fatal(err)
		}
		rt := h.Routes()
		get(rt, "/communities")
		rr := httptest.NewRecorder()
		h.MetricsHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
		out := rr.Body.String()
		if strings.Contains(out, "page-timing") || strings.Contains(out, "page_timing") {
			t.Error("metrics must not gain a page-timing series")
		}
		if !strings.Contains(out, `hextet_lg_http_request_duration_seconds_count{method="GET",route="communities"} 1`) {
			t.Errorf("flag %v: HTTP duration metric not recorded", *flag)
		}
		h.Close()
		closeAll()
	}
}

func withInfo(ctx context.Context, i *reqInfo) context.Context {
	return context.WithValue(ctx, reqInfoKey{}, i)
}
