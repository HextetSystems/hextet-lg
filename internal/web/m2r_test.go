package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
	"github.com/HextetSystems/hextet-lg/internal/config"
)

func rowCount(body string) int { return strings.Count(body, "Route details") }

func TestPaginateSlicesAcrossBackends(t *testing.T) {
	mk := func(n int) BackendResult {
		return BackendResult{Result: bgp.Result{Routes: make([]bgp.Route, n), Completion: bgp.Completion{Status: bgp.StatusComplete}}}
	}
	results := []BackendResult{mk(120), mk(0), mk(90)} // 210 routes
	for _, tc := range []struct {
		page, pages, from, to int
		rows                  []int
		oor                   bool
	}{
		{1, 3, 1, 100, []int{100, 0, 0}, false},
		{2, 3, 101, 200, []int{20, 0, 80}, false},
		{3, 3, 201, 210, []int{0, 0, 10}, false},
		{4, 3, 0, 0, []int{0, 0, 0}, true},
	} {
		secs, pg := paginate(results, tc.page, 100)
		if pg.Pages != tc.pages || pg.From != tc.from || pg.To != tc.to || pg.OutOfRange != tc.oor || pg.Total != 210 {
			t.Errorf("page %d: %+v", tc.page, pg)
		}
		for i, want := range tc.rows {
			if len(secs[i].Rows) != want {
				t.Errorf("page %d section %d: %d rows want %d", tc.page, i, len(secs[i].Rows), want)
			}
		}
	}
	if _, pg := paginate(nil, 1, 100); pg.Pages != 1 || pg.Total != 0 {
		t.Fatal("empty result must have one page")
	}
}

func TestParsePage(t *testing.T) {
	for in, want := range map[string]int{"": 1, "1": 1, "7": 7, " 3 ": 3} {
		if got, err := parsePage(url.Values{"page": {in}}); err != nil || got != want {
			t.Errorf("%q => %d %v", in, got, err)
		}
	}
	for _, in := range []string{"0", "-1", "abc", "1.5", "99999999999", "2e3"} {
		if _, err := parsePage(url.Values{"page": {in}}); err == nil {
			t.Errorf("%q must be rejected", in)
		}
	}
	if _, err := parsePage(url.Values{"page": {"1", "2"}}); err == nil {
		t.Error("repeated page must be rejected")
	}
}

func TestHTMLPaginationLargeResultDoesNotRenderEverything(t *testing.T) {
	var hits atomic.Int32
	doc := ribJSON(5000, false)
	lg := defaults()
	lg.QueryTimeout = 30 * time.Second
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = io.WriteString(w, doc) }})
	base := "/?type=large-community&q=395089%3A0%3A8&backend=a&view=table"

	rr := get(h, base)
	body := rr.Body.String()
	if rr.Code != 200 || rowCount(body) != 100 || !strings.Contains(body, "5,000 routes decoded") || !strings.Contains(body, "Page 1 of 50") {
		t.Fatalf("page 1: code=%d rows=%d", rr.Code, rowCount(body))
	}
	if len(body) > 600<<10 {
		t.Fatalf("page 1 is %d bytes: too much HTML for 100 rows", len(body))
	}
	next := "/?type=large-community&amp;q=395089%3A0%3A8&amp;backend=a&amp;view=table&amp;page=2"
	if !strings.Contains(body, `href="`+next+`"`) || !strings.Contains(body, "COMPLETE") {
		t.Fatalf("next-page link or status missing")
	}
	if strings.Contains(body, `rel="prev"`) {
		t.Fatal("no previous link on the first page")
	}

	body = get(h, base+"&page=25").Body.String()
	if rowCount(body) != 100 || !strings.Contains(body, "routes 2,401–2,500") || !strings.Contains(body, `rel="prev"`) || !strings.Contains(body, `rel="next"`) {
		t.Fatal("middle page")
	}
	if body = get(h, base+"&page=50").Body.String(); rowCount(body) != 100 || strings.Contains(body, `rel="next"`) {
		t.Fatal("final page")
	}
	// Navigating pages is served from the short-lived cache only when enabled;
	// with the cache off every request is a live query.
	if hits.Load() != 3 {
		t.Fatalf("cache disabled but hits=%d", hits.Load())
	}

	body = get(h, base+"&page=51").Body.String()
	if rowCount(body) != 0 || !strings.Contains(body, "Page 51 does not exist") || !strings.Contains(body, "page=50") {
		t.Fatal("out of range page")
	}
	for _, bad := range []string{"0", "abc", "-3"} {
		body = get(h, base+"&page="+bad).Body.String()
		if !strings.Contains(body, "invalid page number") || rowCount(body) != 0 {
			t.Fatalf("page=%s: %s", bad, body[:min(len(body), 300)])
		}
	}
}

func TestPaginationUsesCacheAcrossPages(t *testing.T) {
	var hits atomic.Int32
	doc := ribJSON(450, false)
	lg := defaults()
	lg.CacheTTL = time.Minute
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = io.WriteString(w, doc) }})
	base := "/?as=64500&backend=a&view=table"
	first := get(h, base)
	if strings.Contains(first.Body.String(), "cached") {
		t.Fatal("first request must be a live query")
	}
	second := get(h, base+"&page=2")
	if hits.Load() != 1 || rowCount(second.Body.String()) != 100 || !strings.Contains(second.Body.String(), "cached") || !strings.Contains(second.Body.String(), "not snapshots") {
		t.Fatalf("hits=%d", hits.Load())
	}
	if !strings.Contains(second.Header().Get("X-Hextet-LG-Backends"), "cached=1") {
		t.Fatal("cache use must be visible in metadata")
	}
	// A different query is a different cache key.
	get(h, "/?as=64501&backend=a&view=table")
	if hits.Load() != 2 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

// partialBackend sends n complete routes then stalls until the client gives up.
func partialBackend(n int) http.HandlerFunc {
	doc := strings.TrimSuffix(strings.TrimSpace(ribJSON(n, false)), "]\n}")
	doc = strings.TrimSuffix(strings.TrimSpace(doc), "") + `,{"prefix":"10.99.9`
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Replace(doc, "\n]\n}", "", 1))
		flushW(w)
		<-r.Context().Done()
	}
}

func TestPartialResultsAreShownAndFlagged(t *testing.T) {
	lg := defaults()
	lg.QueryTimeout = 200 * time.Millisecond
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", partialBackend(250)})

	rr := get(h, "/?type=asn&q=64500&backend=a&view=table")
	body := rr.Body.String()
	if rr.Code != 200 || rowCount(body) != 100 {
		t.Fatalf("code=%d rows=%d", rr.Code, rowCount(body))
	}
	for _, want := range []string{"PARTIAL", "Partial results", "Backend timeout", "Additional matching routes may exist", "Incomplete results", "(incomplete)", "250 routes decoded"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "No matching routes") || strings.Contains(body, "10.99.9") {
		t.Fatal("partial result presented as empty/complete, or incomplete route leaked")
	}
	if rr.Header().Get("X-Hextet-LG-Completeness") != "partial" {
		t.Fatal(rr.Header())
	}
	// Pagination of a partial result keeps the warning on every page.
	if p3 := get(h, "/?type=asn&q=64500&backend=a&view=table&page=3").Body.String(); rowCount(p3) != 50 || !strings.Contains(p3, "Partial results") {
		t.Fatal("page 3 of partial result")
	}
}

func TestPlainTextPartialWarningAndMetadata(t *testing.T) {
	lg := defaults()
	lg.QueryTimeout = 200 * time.Millisecond
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", partialBackend(30)})
	rr := get(h, "/?type=asn&q=64500&backend=a&view=text&output=plain")
	body := rr.Body.String()
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("%d %s", rr.Code, rr.Header().Get("Content-Type"))
	}
	for _, want := range []string{"WARNING: THIS RESULT IS INCOMPLETE", "WARNING: PARTIAL RESULTS\nBackend: Alpha (Test)\nReason: Backend timeout after 200ms\nRoutes decoded: 30\nElapsed: 0.", "only a subset of the\nmatching routing information", "-- end of partial results"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body[:min(len(body), 600)])
		}
	}
	if n := strings.Count(body, "10.0."); n < 30 {
		t.Fatalf("plain text must contain all %d decoded routes, saw %d", 30, n)
	}
	hdr := rr.Header()
	if hdr.Get("X-Hextet-LG-Completeness") != "partial" || hdr.Get("X-Hextet-LG-Routes") != "30" || !strings.HasPrefix(hdr.Get("X-Hextet-LG-Backends"), "a=partial;reason=timeout;routes=30") || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", hdr)
	}
}

func TestPlainTextOutputAllRoutesDetailAndFamilies(t *testing.T) {
	h := newTestHandler(t, defaults(),
		backendSpec{"a", "Alpha", serveDoc(ribJSON(3, false))},
		backendSpec{"b", "Beta", serveDoc(ribJSON(2, true))})
	rr := get(h, "/?large-community=395089%3A0%3A8&backend=all&view=detail&output=plain")
	body := rr.Body.String()
	if rr.Code != 200 || rr.Header().Get("X-Hextet-LG-Completeness") != "complete" || strings.Contains(body, "WARNING") {
		t.Fatalf("%d %v", rr.Code, rr.Header())
	}
	for _, want := range []string{"Backend: Alpha (Test)\nStatus: COMPLETE — 3 routes decoded", "Backend: Beta (Test)\nStatus: COMPLETE — 2 routes decoded", "Prefix: 10.0.2.0/24", "Prefix: 2001:db8:1::/48", "Large communities: 395089:0:8 395089:5:2", "Extended communities: rt 64500:1", "Communities: 64500:1", "Neighbor: peer-a (192.0.2.1)"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Accept: text/plain also selects plain output for text views.
	if rr = get(h, "/?as=64500&view=text", "Accept", "text/plain"); !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/plain") {
		t.Fatal("Accept header not honoured")
	}
	// The table view is HTML even if text/plain is requested.
	if rr = get(h, "/?as=64500&view=table&output=plain"); !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/html") {
		t.Fatal("table view must stay HTML")
	}
	// page= paginates plain output when explicitly requested.
	rr = get(h, "/?as=64500&backend=a&view=text&output=plain&page=2")
	if rr.Code != 404 || !strings.Contains(rr.Body.String(), "PAGE OUT OF RANGE") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

func TestMultiBackendIndependentCompletion(t *testing.T) {
	lg := defaults()
	lg.QueryTimeout = 250 * time.Millisecond
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	cfg, closeAll := testConfig(lg,
		backendSpec{"a", "Alpha", serveDoc(ribJSON(15, false))},
		backendSpec{"b", "Beta", partialBackend(42)})
	defer closeAll()
	cfg.Backends = append(cfg.Backends, config.Backend{ID: "c", Name: "Gamma", URL: deadURL, Enabled: true})
	hd, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := hd.Routes()

	rr := get(h, "/?type=asn&q=64500&view=table")
	body := rr.Body.String()
	for _, want := range []string{"COMPLETE", "PARTIAL", "FAILED", "15 routes", "42 routes", "0 routes", "Query failed", "Partial results", "1 of 3 backends completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if rowCount(body) != 57 {
		t.Fatalf("usable results from complete and partial backends must be kept: %d rows", rowCount(body))
	}
	hdr := rr.Header().Get("X-Hextet-LG-Backends")
	for _, want := range []string{"a=complete;routes=15", "b=partial;reason=timeout;routes=42", "c=failed;reason=backend_error;routes=0"} {
		if !strings.Contains(hdr, want) {
			t.Errorf("header %q missing %q", hdr, want)
		}
	}
	if rr.Header().Get("X-Hextet-LG-Completeness") != "partial" {
		t.Fatal("overall must be partial, not merged into complete")
	}
	if strings.Contains(body, deadURL) || strings.Contains(hdr, "127.0.0.1") {
		t.Fatal("backend address leaked")
	}
}

func TestAllBackendsFailedPlainStatus(t *testing.T) {
	lg := defaults()
	lg.QueryTimeout = 100 * time.Millisecond
	stall := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", stall})
	rr := get(h, "/?as=1&view=text&output=plain")
	if rr.Code != http.StatusGatewayTimeout || !strings.Contains(rr.Body.String(), "ERROR: QUERY FAILED") || !strings.Contains(rr.Body.String(), "NOT\nevidence that no matching routes exist") || rr.Header().Get("X-Hextet-LG-Completeness") != "failed" {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	h = newTestHandler(t, defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) }})
	if rr = get(h, "/?as=1&view=text&output=plain"); rr.Code != http.StatusBadGateway {
		t.Fatal(rr.Code)
	}
	// A complete, empty answer is a genuine "no matching routes".
	h = newTestHandler(t, defaults(), backendSpec{"a", "Alpha", serveDoc("{\n\n}\n")})
	rr = get(h, "/?as=1&view=text&output=plain")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "No matching routes.") || rr.Header().Get("X-Hextet-LG-Completeness") != "complete" {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

func TestResponseAndMemoryLimitsShownAsPartial(t *testing.T) {
	lg := defaults()
	lg.MaxResponseBytes = 30_000
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", serveDoc(ribJSON(500, false))})
	rr := get(h, "/?as=64500&view=text&output=plain")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "WARNING: PARTIAL RESULTS") || !strings.Contains(rr.Body.String(), "exceeded the configured limit of 29.3 KiB") || !strings.HasPrefix(rr.Header().Get("X-Hextet-LG-Backends"), "a=partial;reason=response_limit") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String()[:400])
	}
	lg = defaults()
	lg.MaxRetainedBytes = 8192
	h = newTestHandler(t, lg, backendSpec{"a", "Alpha", serveDoc(ribJSON(500, false))})
	rr = get(h, "/?as=64500&view=text&output=plain")
	if !strings.Contains(rr.Body.String(), "memory limit of 8.0 KiB reached") || !strings.HasPrefix(rr.Header().Get("X-Hextet-LG-Backends"), "a=partial;reason=memory_limit") {
		t.Fatalf("%s", rr.Body.String()[:400])
	}
}

func TestCompoundQueryReachesBackendInOneRequest(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		_, _ = io.WriteString(w, ribJSON(2, false))
	}})
	target := "/?as=15169&large-community=395089%3A0%3A6&community=NO_EXPORT&ext-community=rt+65000%3A1&af=ipv4&ovs=valid&best=1&backend=a&view=table"
	rr := get(h, target)
	body := rr.Body.String()
	if rr.Code != 200 || rowCount(body) != 2 {
		t.Fatalf("%d rows=%d", rr.Code, rowCount(body))
	}
	if len(seen) != 1 {
		t.Fatalf("compound query must be a single upstream request, got %v", seen)
	}
	got, _ := url.ParseQuery(strings.TrimPrefix(seen[0], "/rib?"))
	want := url.Values{"as": {"15169"}, "large-community": {"395089:0:6"}, "community": {"NO_EXPORT"}, "ext-community": {"rt 65000:1"}, "af": {"ipv4"}, "ovs": {"valid"}, "best": {"1"}}
	if !strings.HasPrefix(seen[0], "/rib?") || len(got) != len(want) {
		t.Fatalf("upstream %s", seen[0])
	}
	for k, v := range want {
		if got.Get(k) != v[0] {
			t.Errorf("%s=%q want %q", k, got.Get(k), v[0])
		}
	}
	// Filters are shown (none silently dropped) and kept distinct.
	for _, chip := range []string{"<b>community</b>=NO_EXPORT", "<b>ext-community</b>=rt 65000:1", "<b>large-community</b>=395089:0:6", "<b>as</b>=15169", "<b>best</b>=1"} {
		if !strings.Contains(body, chip) {
			t.Errorf("filter chip %q missing", chip)
		}
	}
	// Pager links keep every filter, in canonical order, with the page.
	if !strings.Contains(body, "Live query sent to each backend") || strings.Contains(body, `rel="next"`) {
		t.Fatal("two routes need no next link")
	}
}

func TestCompoundQueryStableURLsAndPagination(t *testing.T) {
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", serveDoc(ribJSON(250, false))})
	body := get(h, "/?best=1&af=ipv4&as=64500&backend=a&view=text").Body.String()
	// Canonical order is endpoint, prefix, as, ... af ... flags — independent of input order.
	want := `href="/?as=64500&amp;af=ipv4&amp;best=1&amp;backend=a&amp;view=text&amp;page=2"`
	if !strings.Contains(body, want) {
		t.Fatalf("canonical next URL missing; body has %q", body[strings.Index(body, "rel=\"next\"")-200:strings.Index(body, "rel=\"next\"")+60])
	}
	// Following the canonical URL reproduces the same query.
	p2 := get(h, "/?as=64500&af=ipv4&best=1&backend=a&view=text&page=2").Body.String()
	if !strings.Contains(p2, "routes 101–200") || !strings.Contains(p2, `rel="prev"`) {
		t.Fatal("page 2 via canonical URL")
	}
	// Plain text link preserves all filters.
	if !strings.Contains(body, `href="/?as=64500&amp;af=ipv4&amp;best=1&amp;backend=a&amp;view=text&amp;output=plain"`) {
		t.Fatal("plain-text link")
	}

}

func TestSimpleURLsRemainBackwardCompatible(t *testing.T) {
	var path, raw string
	h := newTestHandler(t, defaults(), backendSpec{"stable", "Stable", func(w http.ResponseWriter, r *http.Request) {
		path, raw = r.URL.Path, r.URL.RawQuery
		_, _ = io.WriteString(w, ribJSON(250, false))
	}})
	for _, tc := range []struct{ in, up, canon string }{
		{"/?type=prefix&q=8.8.8.8&backend=stable&view=table", "prefix=8.8.8.8", "/?type=prefix&amp;q=8.8.8.8&amp;backend=stable&amp;view=table&amp;page=2"},
		{"/?type=asn&q=AS15169&backend=stable&view=table", "as=15169", "/?type=asn&amp;q=15169&amp;backend=stable&amp;view=table&amp;page=2"},
		{"/?type=large-community&q=395089%3A0%3A6&backend=stable&view=text", "large-community=395089%3A0%3A6", "/?type=large-community&amp;q=395089%3A0%3A6&amp;backend=stable&amp;view=text&amp;page=2"},
		{"/?q=8.8.4.4", "prefix=8.8.4.4", "type=prefix&amp;q=8.8.4.4&amp;backend=all&amp;view=table&amp;page=2"},
	} {
		rr := get(h, tc.in)
		if rr.Code != 200 || path != "/rib" || raw != tc.up || !strings.Contains(rr.Body.String(), tc.canon) {
			t.Errorf("%s => %d upstream %s?%s", tc.in, rr.Code, path, raw)
		}
	}
}

func TestInvalidAndConflictingFilters(t *testing.T) {
	var hits atomic.Int32
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = io.WriteString(w, "{}") }})
	for name, tc := range map[string]struct{ q, want string }{
		"neighbor+group":    {"neighbor=a&group=b", "neighbor and group are mutually exclusive"},
		"two states":        {"as=1&best=1&leaked=1", "mutually exclusive; choose one route state"},
		"all+or-shorter":    {"prefix=10.0.0.0/8&all=1&or-shorter=1", "all and or-shorter are mutually exclusive"},
		"all w/o prefix":    {"as=1&all=1", "All requires a prefix"},
		"rib on adj-rib-in": {"endpoint=rib-in&rib=x&best=1", "selectable RIB endpoints are no longer supported"},
		"unknown endpoint":  {"endpoint=fib&as=1", "selectable RIB endpoints are no longer supported"},
		"bad af":            {"af=ipx&as=1", "ipv4, ipv6"},
		"bad community":     {"community=1:2:3", "as:value"},
		"repeated filter":   {"as=1&as=2", "more than once"},
		"type+filter dup":   {"type=asn&q=1&as=2", "both as q"},
		"bad flag":          {"best=yes&as=1", "expected 1"},
		"bad type":          {"type=aspath&q=1", "unsupported search type"},
		"mistyped filter":   {"as=1&av=valid", "unknown parameter: av is"},
		"unknown alone":     {"asn=15169", "unknown parameter: asn is"},
	} {
		t.Run(name, func(t *testing.T) {
			rr := get(h, "/?"+tc.q+"&backend=a")
			if rr.Code != 400 || !strings.Contains(strings.ToLower(rr.Body.String()), strings.ToLower(tc.want)) || rowCount(rr.Body.String()) != 0 {
				t.Fatalf("html: %d missing %q", rr.Code, tc.want)
			}
			rr = get(h, "/?"+tc.q+"&backend=a&view=text&output=plain")
			if rr.Code != 400 || !strings.Contains(strings.ToLower(rr.Body.String()), strings.ToLower(tc.want)) {
				t.Fatalf("plain: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatal("invalid queries must not reach a backend")
	}
}

func TestFormNoiseRedirectsToCanonicalAndHTMXPushesURL(t *testing.T) {
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", serveDoc(ribJSON(1, false))})
	noisy := "/?backend=a&type=prefix&q=&endpoint=rib&prefix=&as=15169&community=&ext-community=&large-community=395089%3A0%3A6&neighbor=&group=&af=&rib=&ovs=&avs=&view=table"
	rr := get(h, noisy)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/?as=15169&large-community=395089%3A0%3A6&backend=a&view=table" {
		t.Fatalf("%d %q", rr.Code, rr.Header().Get("Location"))
	}
	if loc := rr.Header().Get("Location"); get(h, loc).Code != 200 {
		t.Fatal("canonical URL must work")
	}
	// A single-filter submission canonicalizes to the legacy form.
	rr = get(h, "/?backend=a&type=asn&q=64500&prefix=&view=text")
	if rr.Header().Get("Location") != "/?type=asn&q=64500&backend=a&view=text" {
		t.Fatalf("%q", rr.Header().Get("Location"))
	}
	// HTMX: fragment plus the canonical URL for history; no redirect.
	rr = get(h, noisy, "HX-Request", "true")
	if rr.Code != 200 || rr.Header().Get("HX-Push-Url") != "/?as=15169&large-community=395089%3A0%3A6&backend=a&view=table" || strings.Contains(rr.Body.String(), "<!doctype") {
		t.Fatalf("%d push=%q", rr.Code, rr.Header().Get("HX-Push-Url"))
	}
	// History restore after a cache miss needs the whole page, not a fragment.
	rr = get(h, "/?as=15169&backend=a&view=table", "HX-Request", "true", "HX-History-Restore-Request", "true")
	if !strings.Contains(rr.Body.String(), "<!doctype html>") || !strings.Contains(rr.Header().Get("Vary"), "HX-Request") {
		t.Fatal("history restore must return a full page")
	}
}

func TestAdvancedFormShowsOnlyCompatibleControls(t *testing.T) {
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", serveDoc("{}")})
	body := get(h, "/").Body.String()
	for _, want := range []string{"Advanced filters", `name="ext-community"`, `name="large-community"`, `name="community"`, `name="neighbor"`, `name="group"`, `name="ovs"`, `name="avs"`, `name="best"`, `name="or-shorter"`, `name="rib"`, "(choose one)", "no OR lists"} {
		if !strings.Contains(body, want) {
			t.Errorf("form missing %q", want)
		}
	}
	if strings.Contains(body, `required`) {
		t.Fatal("the search box must not be required: filters can be combined without it")
	}
	// Compound searches open the advanced section with their values filled in.
	body = get(h, "/?as=64500&best=1&af=ipv6&backend=a").Body.String()
	for _, want := range []string{`<details class="advanced" id="advanced" >`, `value="64500"`, `<option value="ipv6" selected>`, `name="best" value="1" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("filled form missing %q", want)
		}
	}
}

func TestCompoundLargeAndPartialResponses(t *testing.T) {
	lg := defaults()
	lg.QueryTimeout = 250 * time.Millisecond
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", partialBackend(300)})
	rr := get(h, "/?as=64500&large-community=395089%3A0%3A8&af=ipv4&best=1&backend=a&view=table&page=2")
	body := rr.Body.String()
	if rowCount(body) != 100 || !strings.Contains(body, "Partial results") || !strings.Contains(body, "<b>af</b>=ipv4") {
		t.Fatalf("rows=%d", rowCount(body))
	}
}

func TestExpensiveQueriesAreBoundedAcrossRequests(t *testing.T) {
	var cur, peak atomic.Int32
	lg := defaults()
	lg.MaxConcurrentQueries = 2
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		cur.Add(-1)
		_, _ = io.WriteString(w, "{}")
	}})
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Distinct queries, so the cache and single-flight cannot merge them.
			if rr := get(h, "/?as="+strings.Repeat("1", i)+"&backend=a"); rr.Code != 200 {
				t.Error(rr.Code)
			}
		}()
	}
	wg.Wait()
	if peak.Load() != 2 {
		t.Fatalf("peak concurrent expensive queries = %d, want 2", peak.Load())
	}
	// Prefix results can also be large, so they share the retention limit.
	peak.Store(0)
	for i := 1; i <= 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); get(h, "/?prefix=10.0.0."+strings.Repeat("1", i%3+1)+"&backend=a") }()
	}
	wg.Wait()
	if peak.Load() > 2 || peak.Load() == 0 {
		t.Fatalf("prefix lookups must share the query limit (peak %d)", peak.Load())
	}
}

func TestBusyWhenExpensiveSlotsStayOccupied(t *testing.T) {
	release := make(chan struct{})
	lg := defaults()
	lg.MaxConcurrentQueries = 1
	lg.QueryTimeout = 300 * time.Millisecond
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "{}")
	}})
	defer close(release)
	go get(h, "/?as=1&backend=a")
	time.Sleep(50 * time.Millisecond)
	rr := get(h, "/?as=2&backend=a&view=text&output=plain")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "Server busy") || !strings.HasPrefix(rr.Header().Get("X-Hextet-LG-Backends"), "a=failed;reason=busy") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

func TestClientDisconnectCancelsBackendQuery(t *testing.T) {
	cancelled := make(chan struct{})
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"rib":[`)
		flushW(w)
		<-r.Context().Done()
		close(cancelled)
	}})
	ctx, cancel := contextWithCancel()
	req := httptest.NewRequest("GET", "/?as=1&backend=a&view=text&output=plain", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("backend request was not cancelled when the client went away")
	}
	<-done
}
