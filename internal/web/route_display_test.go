package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

// ---- fixtures -----------------------------------------------------------------

const rawMarker = "INTERNAL-ROUTER-ID-7f3a"

// m482Route is one bgplgd rib entry. The unmodelled x_marker key proves what
// reaches the page: it exists only in the raw backend object.
func m482Route(prefix, nexthop, neighbor, aspath, ovs, avs string) string {
	return fmt.Sprintf(`{"prefix":%q,"aspath":%q,"true_nexthop":%q,"exit_nexthop":%q,"neighbor":{"description":"peer <b>x</b>","remote_addr":%q},"valid":true,"best":true,"ovs":%q,"avs":%q,"origin":"igp","localpref":100,"metric":5,"last_update_sec":3600,"communities":["64500:1"],"x_marker":%q}`,
		prefix, aspath, nexthop, nexthop, neighbor, ovs, avs, rawMarker)
}

func m482Doc(routes ...string) string { return `{"rib":[` + strings.Join(routes, ",") + `]}` }

// recordingBackend serves doc and records every upstream query string.
type recordingBackend struct {
	mu      sync.Mutex
	queries []string
}

func (b *recordingBackend) handler(doc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.queries = append(b.queries, r.URL.RawQuery)
		b.mu.Unlock()
		_, _ = w.Write([]byte(doc))
	}
}

func m482Config(t *testing.T, edit func(*config.Config), specs ...backendSpec) config.Config {
	t.Helper()
	cfg, closeAll := testConfig(defaults(), specs...)
	t.Cleanup(closeAll)
	if edit != nil {
		edit(&cfg)
	}
	return cfg
}

func m482Handler(t *testing.T, edit func(*config.Config), specs ...backendSpec) http.Handler {
	t.Helper()
	h, err := NewHandler(m482Config(t, edit, specs...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h.Routes()
}

func TestRouteQualificationDisplaySetting(t *testing.T) {
	doc := m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500", "valid", "unknown"))
	page := get(m482Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(doc)}), "/?type=prefix&q=203.0.113.0/24").Body.String()
	if strings.Contains(page, "Route qualification") {
		t.Fatal("qualification visible by default")
	}
	page = get(m482Handler(t, func(c *config.Config) { c.Display.RouteQualification = true }, backendSpec{"a", "Alpha", serveDoc(doc)}), "/?type=prefix&q=203.0.113.0/24").Body.String()
	if !strings.Contains(page, "Route qualification") || !strings.Contains(page, "Qualified") {
		t.Fatal("qualification not shown when enabled")
	}
}

// ---- display.route_columns ----------------------------------------------------

var (
	thRe   = regexp.MustCompile(`<th[ >]`)
	tdRe   = regexp.MustCompile(`<td[ >]`)
	rowRe  = regexp.MustCompile(`(?s)<tbody class="route"><tr>(.*?)</tr>\s*<tr class="details-row">`)
	headRe = regexp.MustCompile(`(?s)<thead><tr>(.*?)</tr></thead>`)
)

func TestRouteColumnCombinations(t *testing.T) {
	doc := m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500 64501", "valid", "unknown"), m482Route("2001:db8::/32", "2001:db8::1", "2001:db8:f::1", "64500", "invalid", "valid"))
	var rec recordingBackend
	var want string
	for mask := 0; mask < 8; mask++ {
		nb, rpki, aspa := mask&1 != 0, mask&2 != 0, mask&4 != 0
		t.Run(fmt.Sprintf("neighbor=%v rpki=%v aspa=%v", nb, rpki, aspa), func(t *testing.T) {
			h := m482Handler(t, func(c *config.Config) {
				c.Display.RouteColumns.Neighbor, c.Display.RouteColumns.RPKI, c.Display.RouteColumns.ASPA = nb, &rpki, &aspa
			}, backendSpec{"a", "Alpha", rec.handler(doc)})
			body := get(h, "/?type=prefix&q=203.0.113.0/24&backend=a").Body.String()
			expect := 7
			for _, on := range []bool{nb, rpki, aspa} {
				if on {
					expect++
				}
			}
			head := headRe.FindStringSubmatch(body)
			if head == nil || len(thRe.FindAllString(head[1], -1)) != expect {
				t.Fatalf("header cells: want %d in %v", expect, head)
			}
			rows := rowRe.FindAllStringSubmatch(body, -1)
			if len(rows) != 2 {
				t.Fatalf("rows: %d", len(rows))
			}
			for i, r := range rows {
				if got := len(tdRe.FindAllString(r[1], -1)); got != expect {
					t.Errorf("row %d: %d cells, header has %d (misaligned)", i, got, expect)
				}
			}
			if !strings.Contains(body, fmt.Sprintf(`<td colspan="%d"><div class="disclosures">`, expect)) || !strings.Contains(body, fmt.Sprintf("routes cols-%d", expect)) {
				t.Errorf("details row must span all %d columns", expect)
			}
			for col, on := range map[string]bool{"<th>Neighbor</th>": nb, "<th>RPKI</th>": rpki, "<th>ASPA</th>": aspa} {
				if strings.Contains(body, col) != on {
					t.Errorf("%s present=%v want %v", col, !on, on)
				}
			}
			// Hidden columns keep their values in the Route details.
			mustContain(t, body, "<span>RPKI origin validation</span><strong>valid</strong>", "<span>ASPA validation</span><strong>unknown</strong>", "<span>Neighbor</span>", "198.51.100.1")
			// One collapsed disclosure row per route and no other rows.
			if got := strings.Count(body, `<tr class="details-row">`); got != 2 {
				t.Errorf("details rows: %d", got)
			}
			if got := strings.Count(body, "<tr"); got != 1+2*2 {
				t.Errorf("table rows: %d, want 1 header + 2 routes x 2", got)
			}
			// Presentation never changes the backend request.
			rec.mu.Lock()
			q := rec.queries[len(rec.queries)-1]
			rec.mu.Unlock()
			if want == "" {
				want = q
			} else if q != want {
				t.Errorf("upstream query changed with display settings: %q vs %q", q, want)
			}
		})
	}
}

func TestDefaultTableLayout(t *testing.T) {
	h := m482Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500", "valid", "valid")))})
	body := get(h, "/?type=prefix&q=203.0.113.0/24&backend=a").Body.String()
	mustContain(t, body, "<th>Prefix</th><th>AS path</th><th>Origin ASN</th><th>Exit next hop</th><th>Local pref</th><th>MED</th><th>Age</th><th>RPKI</th><th>ASPA</th>", `routes cols-9`)
	mustNotContain(t, body, "<th>Neighbor</th>", `class="c-nb`, "Raw JSON")
	// Only the Route details disclosure exists by default: one summary per route.
	table := body[strings.Index(body, "<table>"):]
	if strings.Count(table, "<summary>Route details</summary>") != 1 || strings.Count(table, "<summary>") != 1 {
		t.Errorf("disclosures: %d summaries", strings.Count(table, "<summary>"))
	}
}

// ---- display.raw_json ---------------------------------------------------------

func TestRawJSONIsOptIn(t *testing.T) {
	routes := m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500", "valid", "valid"))
	neighbors := `{"neighbors":[{"remote_addr":"198.51.100.1","remote_as":"64500","description":"<script>alert(1)</script>","state":"Established","last_updown":"1d","x_marker":"` + rawMarker + `","stats":{}}]}`
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rib":
			_, _ = w.Write([]byte(routes))
		case "/neighbors", "/summary":
			_, _ = w.Write([]byte(neighbors))
		case "/rtr":
			_, _ = w.Write([]byte(`{"rtrs":[{"descr":"v1","state":"Established","x_marker":"` + rawMarker + `"}]}`))
		default:
			http.NotFound(w, r)
		}
	}
	paths := []string{"/?type=prefix&q=203.0.113.0/24&backend=a", "/?type=prefix&q=203.0.113.0/24&backend=a&view=table", "/neighbors", "/rtr"}
	for _, enabled := range []bool{false, true} {
		h := m482Handler(t, func(c *config.Config) { c.Display.RawJSON = enabled }, backendSpec{"a", "Alpha", backend})
		for _, p := range paths {
			for _, hdr := range [][]string{nil, {"HX-Request", "true"}} {
				rr := get(h, p, hdr...)
				body := rr.Body.String()
				if rr.Code != 200 {
					t.Fatalf("%s: %d", p, rr.Code)
				}
				has := strings.Contains(body, rawMarker) || strings.Contains(body, "Raw JSON") || strings.Contains(body, "Raw backend response") || strings.Contains(body, `class="d-raw"`)
				wantRaw := enabled && p != "/rtr"
				if has != wantRaw {
					t.Errorf("raw_json=%v %s %v: raw content present=%v", enabled, p, hdr, has)
				}
				if wantRaw && strings.Contains(body, "<script>alert(1)</script>") {
					t.Errorf("%s: backend text is not escaped", p)
				}
			}
		}
		if enabled {
			body := get(h, "/neighbors").Body.String()
			mustContain(t, body, `&lt;script&gt;alert(1)&lt;/script&gt;`, `<summary>Raw JSON</summary>`, `<summary>Raw backend response</summary>`)
			rt := get(h, paths[0]).Body.String()
			mustContain(t, rt, `<details class="d-raw"><summary>Raw JSON</summary><pre>`, `&#34;x_marker&#34;`)
			// The route text views never carried raw JSON, and still do not.
			for _, view := range []string{"text", "detail"} {
				if b := get(h, paths[0]+"&view="+view+"&output=plain").Body.String(); strings.Contains(b, rawMarker) {
					t.Errorf("view=%s exposes raw JSON", view)
				}
			}
		}
	}
	// No frontend endpoint serves raw data, enabled or not.
	h := m482Handler(t, func(c *config.Config) { c.Display.RawJSON = true }, backendSpec{"a", "Alpha", backend})
	for _, p := range []string{"/raw", "/json", "/rib", "/api/raw", "/neighbors.json", "/asn-status?raw=1", "/ptr-status?ip=198.51.100.1&raw=1"} {
		if b := get(h, p).Body.String(); strings.Contains(b, rawMarker) {
			t.Errorf("%s exposes raw JSON", p)
		}
	}
}

func TestDisclosuresAreIndependentNativeDetails(t *testing.T) {
	h := m482Handler(t, func(c *config.Config) { c.Display.RawJSON = true }, backendSpec{"a", "Alpha", serveDoc(m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500", "valid", "valid")))})
	body := get(h, "/?type=prefix&q=203.0.113.0/24&backend=a").Body.String()
	// Two sibling <details>, neither open, in one container inside the single details row.
	mustContain(t, body, `<div class="disclosures"><details class="d-route"><summary>Route details</summary>`, `</details><details class="d-raw"><summary>Raw JSON</summary>`)
	if strings.Contains(body, "<details open") || strings.Count(body, `<tr class="details-row">`) != 1 {
		t.Error("disclosures must start collapsed in one row")
	}
	// Raw JSON is no longer nested in Route details, and appears once per route.
	i, j := strings.Index(body, `<details class="d-route">`), strings.Index(body, `<details class="d-raw">`)
	if strings.Contains(body[i:j], "Raw") || strings.Count(body, rawMarker) != 1 {
		t.Error("raw JSON must be outside Route details and not duplicated")
	}
}

// ---- mobile card structure ----------------------------------------------------

func TestCardStructureSharesOneDOM(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("64500 ", 40)) + " 395611"
	a := m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", long, "valid", "valid"), m482Route("2001:db8:1234::/48", "2001:db8::1", "2001:db8:ffff::1", "18451 395611", "not-found", "unknown"))
	b := m482Doc(m482Route("203.0.113.0/24", "192.0.2.9", "198.51.100.1", "64500", "valid", "invalid"))
	h := m482Handler(t, func(c *config.Config) { c.Display.RouteColumns.Neighbor = true }, backendSpec{"a", "Alpha", serveDoc(a)}, backendSpec{"b", "Beta", serveDoc(b)}, backendSpec{"c", "Gamma", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) }})
	body := get(h, "/?type=prefix&q=203.0.113.0/24&backend=all").Body.String()
	// One <table> per backend with routes; one <tbody class="route"> per route (IPv4 and IPv6).
	if strings.Count(body, "<table>") != 2 || strings.Count(body, `<tbody class="route">`) != 3 {
		t.Errorf("tables=%d routes=%d", strings.Count(body, "<table>"), strings.Count(body, `<tbody class="route">`))
	}
	mustContain(t, body, "2001:db8:1234::/48", "Query failed", "Alpha", "Beta", "Gamma")
	// Card labels come from the same cells; secondary cells are marked for hiding on phones.
	mustContain(t, body, `class="c-path" data-label="AS path"`, `class="c-origin" data-label="Origin"`, `class="c-nh" data-label="Exit next hop"`, `class="c-rpki" data-label="RPKI"`, `class="c-aspa" data-label="ASPA"`, `c-lp m-hide`, `c-med m-hide`, `c-age m-hide`)
	// The whole AS path is in the expanded details (the collapsed card clamps it).
	if !strings.Contains(body, "<span>AS path</span><strong>"+long+"</strong>") {
		t.Error("full long AS path missing from the details")
	}
	// No duplicated enrichment targets, IDs or timing elements.
	ids := regexp.MustCompile(` id="([^"]+)"`).FindAllStringSubmatch(body, -1)
	seen := map[string]bool{}
	for _, m := range ids {
		if seen[m[1]] {
			t.Errorf("duplicate id %q", m[1])
		}
		seen[m[1]] = true
	}
	if timingCount(body) != 1 {
		t.Errorf("timing elements: %d", timingCount(body))
	}
	// Presentation markup carries no inline styles (CSP style-src 'self').
	mustNotContain(t, body, ` style=`)
}

func TestCardsForEmptyPartialAndPagedResults(t *testing.T) {
	// Empty result: no table, no cards.
	h := m482Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(`{"rib":[]}`)})
	if b := get(h, "/?type=prefix&q=203.0.113.0/24&backend=a").Body.String(); strings.Contains(b, "<table>") || !strings.Contains(b, "No matching routes") {
		t.Error("empty result")
	}
	// Pagination keeps one tbody per displayed route.
	lg := defaults()
	lg.PageSize = 3
	cfg, closeAll := testConfig(lg, backendSpec{"a", "Alpha", serveDoc(ribJSON(8, false))})
	defer closeAll()
	hh, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer hh.Close()
	body := get(hh.Routes(), "/?type=prefix&q=10.0.0.0/24&backend=a&page=2").Body.String()
	if strings.Count(body, `<tbody class="route">`) != 3 || !strings.Contains(body, `class="pager"`) {
		t.Error("paged cards")
	}
}

// ---- neighbor names (PTR reuse) -----------------------------------------------

// countingPTR answers from a table and counts lookups per address.
type countingPTR struct {
	mu      sync.Mutex
	answers map[string][]string
	calls   map[string]int
	block   chan struct{} // when non-nil, lookups wait here (or for their context)
}

func (c *countingPTR) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[addr]++
	block := c.block
	c.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if a, ok := c.answers[addr]; ok {
		return a, nil
	}
	// A real NXDOMAIN, which the shared cache stores as a negative entry.
	return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
}

func (c *countingPTR) count(addr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[addr]
}

func (c *countingPTR) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

func (c *countingPTR) addrs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k := range c.calls {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func m482PTRHandler(t *testing.T, format string, neighbor bool, r *countingPTR, maxLookups int, specs ...backendSpec) (*Handler, http.Handler) {
	t.Helper()
	cfg := m482Config(t, func(c *config.Config) {
		c.PTREnrichment = config.PTRDefaults()
		c.PTREnrichment.Enabled = true
		if maxLookups > 0 {
			c.PTREnrichment.MaxLookupsPerPage = maxLookups
		}
		c.Display.RouteColumns.Neighbor, c.Display.RouteColumns.NeighborFormat = neighbor, format
	}, specs...)
	h, err := NewHandler(cfg, WithPTRResolver(r))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, h.Routes()
}

// settle waits until addr has a cache entry (positive or negative).
func settlePTR(t *testing.T, h *Handler, addr string) {
	t.Helper()
	a, err := netip.ParseAddr(addr)
	if err != nil {
		t.Fatal("bad address", addr)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := h.ptr.Get(a); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no PTR result for %s", addr)
}

var nbSpanRe = regexp.MustCompile(`(?s)<td class="c-nb[^"]*">(.*?)</td>`)

func nbCells(body string) []string {
	var out []string
	for _, m := range nbSpanRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestNeighborNameFormats(t *testing.T) {
	doc := m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500", "valid", "valid"))
	for _, tc := range []struct {
		format, wantText, wantTitle string
	}{
		{"ip", "198.51.100.1", ""},
		{"name", "peer1.example.net", "peer1.example.net (198.51.100.1)"},
		{"both", "peer1.example.net (198.51.100.1)", "peer1.example.net (198.51.100.1)"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			r := &countingPTR{answers: map[string][]string{"198.51.100.1": {"peer1.example.net."}}, block: make(chan struct{})}
			h, routes := m482PTRHandler(t, tc.format, true, r, 0, backendSpec{"a", "Alpha", serveDoc(doc)})
			url := "/?type=prefix&q=203.0.113.0/24&backend=a"
			first := get(routes, url).Body.String() // DNS is held back: a pure cache miss
			close(r.block)
			cell := nbCells(first)
			if len(cell) != 1 {
				t.Fatalf("neighbor cells: %d", len(cell))
			}
			// Cache miss: the page never waits for DNS and shows the address.
			mustContain(t, cell[0], `<a class="neighbor-address" href="/?neighbor=198.51.100.1&amp;backend=a&amp;view=table"`, ">198.51.100.1</span>")
			if tc.format == "ip" {
				mustNotContain(t, cell[0], "data-ptr")
				if r.count("198.51.100.1") != 0 {
					t.Error("ip format must not resolve neighbors")
				}
				return
			}
			mustContain(t, cell[0], `data-ptr="198.51.100.1" data-ptr-format="`+tc.format+`"`)
			settlePTR(t, h, "198.51.100.1")
			// Cache hit: name rendered by the server, no enrichment target left, link unchanged.
			cell = nbCells(get(routes, url).Body.String())
			mustContain(t, cell[0], `<span title="`+tc.wantTitle+`">`+tc.wantText+`</span>`, `href="/?neighbor=198.51.100.1&amp;backend=a&amp;view=table"`)
			mustNotContain(t, cell[0], "data-ptr")
			if strings.Contains(strings.SplitN(cell[0], ">", 2)[0], "peer1") {
				t.Error("hostname leaked into the link")
			}
			if r.count("198.51.100.1") != 1 {
				t.Errorf("lookups: %d, want 1 (cache reuse)", r.count("198.51.100.1"))
			}
			// The cache-only endpoint already supports neighbor addresses.
			if b := get(routes, "/ptr-status?ip=198.51.100.1").Body.String(); !strings.Contains(b, "peer1.example.net") {
				t.Errorf("status endpoint: %s", b)
			}
		})
	}
}

func TestNeighborNameFallbacksAndNegativeCaching(t *testing.T) {
	doc := m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500", "valid", "valid"), m482Route("2001:db8::/32", "2001:db8::1", "2001:db8:ffff::1", "64500", "valid", "valid"))
	r := &countingPTR{answers: map[string][]string{"2001:db8:ffff::1": {"v6-peer.example.net."}}}
	h, routes := m482PTRHandler(t, "name", true, r, 0, backendSpec{"a", "Alpha", serveDoc(doc)})
	url := "/?type=prefix&q=203.0.113.0/24&backend=a"
	get(routes, url)
	settlePTR(t, h, "198.51.100.1")
	settlePTR(t, h, "2001:db8:ffff::1")
	cells := nbCells(get(routes, url).Body.String())
	if len(cells) != 2 {
		t.Fatal(len(cells))
	}
	// Negative result: the address stays, with no name tooltip, and is not re-looked-up.
	if !strings.Contains(cells[0], ">198.51.100.1</span>") || strings.Contains(cells[0], `<span title=`) {
		t.Errorf("negative fallback: %s", cells[0])
	}
	// IPv6 neighbor resolves like IPv4.
	if !strings.Contains(cells[1], `<span title="v6-peer.example.net (2001:db8:ffff::1)">v6-peer.example.net</span>`) || !strings.Contains(cells[1], "/?neighbor=2001%3Adb8%3Affff%3A%3A1&amp;backend=a") {
		t.Errorf("ipv6: %s", cells[1])
	}
	get(routes, url)
	if r.count("198.51.100.1") != 1 {
		t.Errorf("negative result was looked up %d times", r.count("198.51.100.1"))
	}
}

func TestNeighborLookupTimeoutNeverBlocksRendering(t *testing.T) {
	doc := m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500", "valid", "valid"))
	r := &countingPTR{block: make(chan struct{})}
	defer close(r.block)
	_, routes := m482PTRHandler(t, "both", true, r, 0, backendSpec{"a", "Alpha", serveDoc(doc)})
	done := make(chan string, 1)
	go func() { done <- get(routes, "/?type=prefix&q=203.0.113.0/24&backend=a").Body.String() }()
	select {
	case body := <-done:
		mustContain(t, body, ">198.51.100.1</span>", `data-ptr-format="both"`)
	case <-time.After(2 * time.Second):
		t.Fatal("rendering waited for DNS")
	}
}

func TestNeighborLookupsAreDeduplicatedAndBounded(t *testing.T) {
	// One neighbor shared by many routes and two backends, plus next hops equal
	// to other neighbors: every address is looked up once.
	var routes1, routes2 []string
	for i := 0; i < 20; i++ {
		routes1 = append(routes1, m482Route(fmt.Sprintf("203.0.113.%d/32", i), "192.0.2.1", "198.51.100.1", "64500", "valid", "valid"))
		routes2 = append(routes2, m482Route(fmt.Sprintf("203.0.113.%d/32", i), "198.51.100.2", "198.51.100.1", "64500", "valid", "valid"))
	}
	r := &countingPTR{answers: map[string][]string{"198.51.100.1": {"peer1.example.net."}}, block: make(chan struct{})}
	h, routes := m482PTRHandler(t, "name", true, r, 0, backendSpec{"a", "Alpha", serveDoc(m482Doc(routes1...))}, backendSpec{"b", "Beta", serveDoc(m482Doc(routes2...))})
	body := get(routes, "/?type=prefix&q=203.0.113.0/24&backend=all").Body.String()
	close(r.block)
	settlePTR(t, h, "198.51.100.1")
	settlePTR(t, h, "192.0.2.1")
	settlePTR(t, h, "198.51.100.2")
	// 40 table cells plus 40 details labels are targets, all served by one lookup.
	if got := strings.Count(body, `data-ptr="198.51.100.1" data-ptr-format="name"`); got != 80 {
		t.Errorf("neighbor targets: %d, want 80", got)
	}
	for _, a := range []string{"198.51.100.1", "192.0.2.1", "198.51.100.2"} {
		if r.count(a) != 1 {
			t.Errorf("%s looked up %d times, want 1", a, r.count(a))
		}
	}
	if r.total() != 3 {
		t.Errorf("total lookups %d (%v), want the 3 distinct addresses", r.total(), r.addrs())
	}
}

func TestNeighborEnrichmentSharesThePerPageLimit(t *testing.T) {
	// Budget of 2 with three distinct next hops and three distinct neighbors:
	// visible next hops come first, neighbors never exceed the shared cap.
	var rs []string
	for i := 1; i <= 3; i++ {
		rs = append(rs, m482Route(fmt.Sprintf("203.0.113.%d/32", i), fmt.Sprintf("192.0.2.%d", i), fmt.Sprintf("198.51.100.%d", i), "64500", "valid", "valid"))
	}
	r := &countingPTR{}
	h, routes := m482PTRHandler(t, "name", true, r, 2, backendSpec{"a", "Alpha", serveDoc(m482Doc(rs...))})
	get(routes, "/?type=prefix&q=203.0.113.0/24&backend=a")
	settlePTR(t, h, "192.0.2.1")
	settlePTR(t, h, "192.0.2.2")
	time.Sleep(100 * time.Millisecond)
	if r.total() != 2 || r.count("192.0.2.1") != 1 || r.count("192.0.2.2") != 1 {
		t.Errorf("lookups %v: the per-page cap must be shared, next hops first", r.addrs())
	}
	// With a budget that also covers neighbors, they are looked up after the next hops.
	r2 := &countingPTR{}
	h2, routes2 := m482PTRHandler(t, "name", true, r2, 4, backendSpec{"a", "Alpha", serveDoc(m482Doc(rs...))})
	get(routes2, "/?type=prefix&q=203.0.113.0/24&backend=a")
	settlePTR(t, h2, "198.51.100.1")
	if r2.total() != 4 || r2.count("192.0.2.1")+r2.count("192.0.2.2")+r2.count("192.0.2.3") != 3 || r2.count("198.51.100.1") != 1 {
		t.Errorf("lookups %v", r2.addrs())
	}
}

func TestHiddenNeighborColumnDoesNoNeighborDNS(t *testing.T) {
	doc := m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500", "valid", "valid"))
	r := &countingPTR{answers: map[string][]string{"198.51.100.1": {"peer1.example.net."}}}
	h, routes := m482PTRHandler(t, "both", false, r, 0, backendSpec{"a", "Alpha", serveDoc(doc)})
	body := get(routes, "/?type=prefix&q=203.0.113.0/24&backend=a").Body.String()
	settlePTR(t, h, "192.0.2.1")
	time.Sleep(50 * time.Millisecond)
	if r.count("198.51.100.1") != 0 {
		t.Error("a hidden Neighbor column must not trigger neighbor lookups")
	}
	// Route details still show the neighbor, as its plain address and link.
	mustContain(t, body, "<span>Neighbor</span>", `href="/?neighbor=198.51.100.1&amp;backend=a&amp;view=table"`)
	mustNotContain(t, body, `data-ptr="198.51.100.1"`, `data-ptr-format`)
	// The description is escaped data.
	mustContain(t, body, "peer &lt;b&gt;x&lt;/b&gt;")
}

func TestNeighborNamesNeedPTRAndNeverChangeIdentity(t *testing.T) {
	// ptr_enrichment disabled: names cannot exist, the address is shown, the link is unchanged.
	doc := m482Doc(m482Route("203.0.113.0/24", "192.0.2.1", "198.51.100.1", "64500", "valid", "valid"))
	h := m482Handler(t, func(c *config.Config) {
		c.Display.RouteColumns.Neighbor = true
		c.Display.RouteColumns.NeighborFormat = "both"
	}, backendSpec{"a", "Alpha", serveDoc(doc)})
	cell := nbCells(get(h, "/?type=prefix&q=203.0.113.0/24&backend=a").Body.String())
	if len(cell) != 1 || !strings.Contains(cell[0], ">198.51.100.1</span>") || strings.Contains(cell[0], "data-ptr") {
		t.Errorf("without PTR enrichment: %v", cell)
	}
	// Same neighbor address on two backends: separate rows, links name their own backend.
	h2 := m482Handler(t, func(c *config.Config) { c.Display.RouteColumns.Neighbor = true }, backendSpec{"a", "Alpha", serveDoc(doc)}, backendSpec{"b", "Beta", serveDoc(doc)})
	body := get(h2, "/?type=prefix&q=203.0.113.0/24&backend=all").Body.String()
	mustContain(t, body, `href="/?neighbor=198.51.100.1&amp;backend=a&amp;view=table"`, `href="/?neighbor=198.51.100.1&amp;backend=b&amp;view=table"`)
}

// ---- history ------------------------------------------------------------------

func TestHistoryRestoreRendersTheFormFromTheCanonicalURL(t *testing.T) {
	h := m482Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(m482Doc(m482Route("2001:db8::/32", "2001:db8::1", "2001:db8:f::1", "64500", "valid", "valid")))}, backendSpec{"b", "Beta", serveDoc(m482Doc(m482Route("2001:db8::/32", "2001:db8::1", "2001:db8:f::1", "64500", "valid", "valid")))})
	// htmx must not restore page snapshots (their form values are stale);
	// every Back/Forward is re-rendered by the server from the canonical URL.
	mustContain(t, get(h, "/").Body.String(), `"historyCacheSize":0`)
	for _, target := range []string{
		"/?type=prefix&q=2001%3Adb8%3A%3A%2F32&backend=b&view=detail",
		"/?prefix=2001%3Adb8%3A%3A%2F32&af=ipv6&ovs=valid&best=1&backend=a&view=table",
	} {
		rr := get(h, target, "HX-Request", "true", "HX-History-Restore-Request", "true")
		body := rr.Body.String()
		if rr.Code != 200 || !strings.HasPrefix(strings.TrimSpace(body), "<!doctype html>") {
			t.Fatalf("%s: restore must be a full page (%d)", target, rr.Code)
		}
		if strings.Count(body, "<title>") != 1 || !strings.Contains(titleOf(t, body), "2001:db8::/32") || timingCount(body) != 1 {
			t.Errorf("%s: title/timing", target)
		}
		fresh := get(h, target).Body.String()
		for _, want := range []string{`<option value="b" selected>`, `<option value="detail" selected>`, `name="q" type="search" value="2001:db8::/32"`} {
			if strings.Contains(target, "backend=b") && strings.Contains(fresh, want) != strings.Contains(body, want) {
				t.Errorf("%s: restored form differs from a fresh load at %q", target, want)
			}
		}
		if strings.Contains(target, "backend=b") {
			mustContain(t, body, `<option value="b" selected>`, `<option value="detail" selected>`, `value="2001:db8::/32"`)
		} else {
			mustContain(t, body, `<option value="a" selected>`, `<option value="ipv6" selected>`, `<option value="valid" selected>`, `name="best" value="1" checked`, `<details class="advanced" id="advanced" >`)
		}
	}
	// A HX fragment is still a fragment: only history restore asks for the full page.
	if frag := get(h, "/?type=prefix&q=2001%3Adb8%3A%3A%2F32&backend=b", "HX-Request", "true").Body.String(); strings.Contains(frag, "<!doctype") {
		t.Error("fragment became a full page")
	}
	// Clearing the search is a plain navigation to / : the default form state.
	clear := get(h, "/").Body.String()
	mustContain(t, clear, `name="q" type="search" value=""`, `<option value="all" selected>`, `<option value="table" selected>`)
}
