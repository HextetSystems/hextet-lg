package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

// M5.1: regression coverage for an OpenBGPD 9.3 backend. The fixtures under
// testdata/openbgpd93 have the exact field structure that a live OpenBGPD 9.3
// bgplgd returned (summary, neighbors, rtr, rib), with every address replaced
// by a documentation address, ASNs by private ASNs, and descriptions by
// placeholders. No private routing data is committed.

func m51Fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "openbgpd93", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// m51Real serves the 9.3-shaped fixtures as bgplgd would.
func m51Real(t *testing.T) http.HandlerFunc {
	body := map[string]string{
		"/summary":   m51Fixture(t, "summary.json"),
		"/neighbors": m51Fixture(t, "neighbors.json"),
		"/rtr":       m51Fixture(t, "rtr.json"),
	}
	v4, v6 := m51Fixture(t, "rib-v4.json"), m51Fixture(t, "rib-v6.json")
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/rib" {
			if strings.Contains(r.URL.Query().Get("prefix"), ":") {
				fmt.Fprint(w, v6)
				return
			}
			fmt.Fprint(w, v4)
			return
		}
		if b, ok := body[r.URL.Path]; ok {
			fmt.Fprint(w, b)
			return
		}
		http.NotFound(w, r)
	}
}

func m51Page(t *testing.T, h http.Handler, target string, want int) string {
	t.Helper()
	rr := get(h, target)
	if rr.Code != want {
		t.Fatalf("%s: status %d, want %d\n%.400s", target, rr.Code, want, rr.Body.String())
	}
	return rr.Body.String()
}

// textOf flattens HTML to visible-ish text for substring assertions.
var m51Tag = regexp.MustCompile(`(?s)<script.*?</script>|<[^>]+>`)

func textOf(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("&#43;", "+", "&amp;", "&", "&#39;", "'", "&lt;", "<", "&gt;", ">").Replace(m51Tag.ReplaceAllString(s, " "))), " ")
}

func TestOpenBGPD93RTRFixtureRendersEverythingTheBackendReports(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m51Real(t)})
	page := m51Page(t, h, "/rtr", 200)
	txt := textOf(page)
	for _, want := range []string{
		"2 sessions", "validator-1 established v1 (min v0) 4321 80 16m56s / 10m / 2h",
		"validator-2 established v1 (min v0) 8765 127 17m14s / 10m / 2h",
		"RTR RefreshTimer in 9m9s", "RTR ExpireTimer in 1h52m",
		// OpenBGPD does not report these; the page says so rather than inventing them.
		"does not report the last successful synchronization time or the number of VRP or ASPA records",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("RTR page lacks %q\n%s", want, txt)
		}
	}
	// Privacy default: no validator address, port or raw document anywhere.
	mustNotContain(t, page, "192.0.2.3", "192.0.2.4", "3323", "rtrs", "remote_addr", "Raw JSON")

	addr := m47Handler(t, func(c *config.Config) { c.Features.RTRSessionAddresses = b3(true) },
		backendSpec{"a", "Alpha", m51Real(t)})
	mustContain(t, m51Page(t, addr, "/rtr", 200), "<code>192.0.2.3:3323</code>", "<code>192.0.2.4:3323</code>")
}

// recordingPTR records every reverse lookup the application attempts.
type recordingPTR struct {
	mu   sync.Mutex
	seen []string
}

func (r *recordingPTR) LookupAddr(_ context.Context, addr string) ([]string, error) {
	r.mu.Lock()
	r.seen = append(r.seen, addr)
	r.mu.Unlock()
	return nil, errors.New("no such host")
}

func (r *recordingPTR) addrs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func TestOpenBGPD93RTRAddressesDoNotLeakThroughEnrichmentOrOtherPages(t *testing.T) {
	// A validator address must not appear on any other page, be handed to the
	// resolver, or be resolvable through the cache-only status endpoint.
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", m51Real(t)})
	defer closeAll()
	cfg.PTREnrichment = config.PTRDefaults()
	cfg.PTREnrichment.Enabled = true
	cfg.ASNEnrichment = config.ASNDefaults()
	cfg.ASNEnrichment.Enabled = true
	rec := &recordingPTR{}
	h, err := NewHandler(cfg, WithPTRResolver(rec), WithASNResolver(noASN{}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	routes := h.Routes()

	for _, p := range []string{"/rtr", "/summary", "/neighbors", "/?type=prefix&q=198.51.100.0/24", "/communities"} {
		page := m51Page(t, routes, p, 200)
		mustNotContain(t, page, "192.0.2.3", "192.0.2.4", `data-ptr="192.0.2.3"`, `data-ptr="192.0.2.4"`)
	}
	for _, ep := range []string{"/ptr-status?ip=192.0.2.3", "/ptr-status?ip=192.0.2.4"} {
		get(routes, ep)
	}
	// The route page's next hop is resolved (the enrichment really ran), the
	// RTR validators never are.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rec.addrs()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	seen := rec.addrs()
	if len(seen) == 0 {
		t.Fatal("no reverse lookup happened: this test would prove nothing")
	}
	for _, a := range seen {
		if a == "192.0.2.3" || a == "192.0.2.4" {
			t.Errorf("RTR validator address %s was handed to the PTR resolver", a)
		}
	}
}

// noASN answers every ASN lookup with no record.
type noASN struct{}

func (noASN) LookupTXT(context.Context, string) ([]string, error) {
	return nil, errors.New("no such host")
}

func TestOpenBGPD93NeighborAndSummaryFixture(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m51Real(t)})
	nei := m51Page(t, h, "/neighbors", 200)
	txt := textOf(nei)
	for _, want := range []string{
		"192.0.2.1 example-description AS64500 Established up 01:00:37",
		"1358273 / 0",  // prefixes received / sent
		"633374 / 125", // messages received / sent (totals)
		"IPv4 unicast, IPv6 unicast", "Hold time 90 s", "Keepalive 30 s",
		"updates 2047415 · withdraws 2477 · EOR 0",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("neighbors page lacks %q\n%s", want, txt)
		}
	}
	sum := textOf(m51Page(t, h, "/summary", 200))
	for _, want := range []string{"1 Sessions reported", "1 Established", "0 Other states", "192.0.2.1 example-description AS64500 Established"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q\n%s", want, sum)
		}
	}
	// /summary has no counters and the page does not invent any.
	mustNotContain(t, m51Page(t, h, "/summary", 200), "Prefixes received", "Messages received")
	// Raw JSON stays off by default and appears only when opted in.
	mustNotContain(t, nei, "Raw JSON", "bgpid", "&#34;remote_as&#34;")
	raw := m47Handler(t, func(c *config.Config) { c.Display.RawJSON = true }, backendSpec{"a", "Alpha", m51Real(t)})
	mustContain(t, m51Page(t, raw, "/neighbors", 200), "Raw JSON")
}

func TestOpenBGPD93RIBFixtureV4AndV6(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m51Real(t)})
	v4 := m51Page(t, h, "/?type=prefix&q=198.51.100.7", 200)
	txt := textOf(v4)
	for _, want := range []string{
		"COMPLETE Alpha · Test · 1 route",
		"198.51.100.0/24 BEST 64501 64502 AS64502 192.0.2.1 200 0",
		"valid unknown", // RPKI valid, ASPA unknown stay distinct
		"64500:100", "64500:0:8", "64500:5:2",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("IPv4 results lack %q\n%s", want, txt)
		}
	}
	v6 := textOf(m51Page(t, h, "/?type=prefix&q=2001:db8:100::1", 200))
	for _, want := range []string{"COMPLETE Alpha · Test · 1 route", "2001:db8:100::/48 BEST 64501 64502 AS64502 2001:db8::1", "valid unknown"} {
		if !strings.Contains(v6, want) {
			t.Errorf("IPv6 results lack %q\n%s", want, v6)
		}
	}
	// The family comes from the prefix, never from next hop or neighbor.
	mustContain(t, m51Page(t, h, "/?type=prefix&q=2001:db8:100::1", 200), `Address family</span><strong>IPv6`)
	mustContain(t, v4, `Address family</span><strong>IPv4`)
	// Default display: Neighbor column hidden, raw JSON absent.
	mustNotContain(t, v4, "<th>Neighbor</th>", "Raw JSON", "&#34;attributes&#34;")
	mustContain(t, v4, "<th>RPKI</th>", "<th>ASPA</th>")
}

// pills extracts (class, text) of the RPKI and ASPA cells of every route row.
func m51Pills(page, col string) [][2]string {
	re := regexp.MustCompile(`class="c-` + col + `"[^>]*><span class="pill (\w+)">([^<]*)</span>`)
	var out [][2]string
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}

func TestRPKIAndASPAStatesAreNotConflated(t *testing.T) {
	// Four routes with different validation outcomes. An unreported state is a
	// dash and never a success; "not-found" and "unknown" are not "valid".
	var doc struct {
		Rib []map[string]any `json:"rib"`
	}
	if err := json.Unmarshal([]byte(m51Fixture(t, "rib-v4.json")), &doc); err != nil {
		t.Fatal(err)
	}
	base := doc.Rib[0]
	mk := func(prefix, ovs, avs string) map[string]any {
		r := map[string]any{}
		for k, v := range base {
			r[k] = v
		}
		r["prefix"] = prefix
		for k, v := range map[string]string{"ovs": ovs, "avs": avs} {
			if v == "-" {
				delete(r, k)
			} else {
				r[k] = v
			}
		}
		return r
	}
	doc.Rib = []map[string]any{
		mk("198.51.100.0/24", "valid", "valid"),
		mk("198.51.101.0/24", "invalid", "invalid"),
		mk("198.51.102.0/24", "not-found", "unknown"),
		mk("198.51.103.0/24", "-", "-"),
	}
	body, _ := json.Marshal(doc)
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(string(body))})
	page := m51Page(t, h, "/?type=prefix&q=198.51.100.0/22&all=1", 200)
	wantOVS := [][2]string{{"good", "valid"}, {"bad", "invalid"}, {"muted", "not-found"}, {"muted", "—"}}
	wantAVS := [][2]string{{"good", "valid"}, {"bad", "invalid"}, {"muted", "unknown"}, {"muted", "—"}}
	if got := m51Pills(page, "rpki"); fmt.Sprint(got) != fmt.Sprint(wantOVS) {
		t.Errorf("RPKI pills %v, want %v", got, wantOVS)
	}
	if got := m51Pills(page, "aspa"); fmt.Sprint(got) != fmt.Sprint(wantAVS) {
		t.Errorf("ASPA pills %v, want %v", got, wantAVS)
	}
	// Route validity is a third, separate field.
	mustContain(t, page, "Route validity")
}

// ---- multi-backend matrix ----

type m51Counter struct{ rib, rtr atomic.Int64 }

func (c *m51Counter) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rib":
			c.rib.Add(1)
		case "/rtr":
			c.rtr.Add(1)
		}
		h(w, r)
	}
}

func abort(w http.ResponseWriter) { panic(http.ErrAbortHandler) }

// m51Backends returns the ten-scenario backend set of milestone 5.1 section 8.
func m51Backends(t *testing.T) (specs []backendSpec, counters map[string]*m51Counter) {
	counters = map[string]*m51Counter{}
	add := func(id string, h http.HandlerFunc) {
		c := &m51Counter{}
		counters[id] = c
		specs = append(specs, backendSpec{id, "B-" + id, c.wrap(h)})
	}
	full := func(rib http.HandlerFunc, rtr string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/rib":
				rib(w, r)
			case "/rtr":
				if rtr == "" {
					http.NotFound(w, r)
					return
				}
				fmt.Fprint(w, rtr)
			case "/neighbors":
				fmt.Fprint(w, m47Neighbors)
			case "/summary":
				fmt.Fprint(w, m47Summary)
			default:
				http.NotFound(w, r)
			}
		}
	}
	doc := func(s string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, s) }
	}
	fix := m51Fixture(t, "rtr.json")

	add("ok", full(doc(ribJSON(3, false)), fix))                                             // 1 healthy, RTR sessions
	add("down", func(w http.ResponseWriter, r *http.Request) { abort(w) })                   // 2 unavailable
	add("e500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }) // 3 HTTP 500
	add("bad", full(doc("this is not json"), "<<<garbage"))                                  // 4 malformed JSON
	add("empty", full(doc("{\n\n}\n"), `{}`))                                                // 5 no routes, RTR none
	add("part", full(func(w http.ResponseWriter, r *http.Request) {                          // 6 partial: connection dies mid-stream
		s := ribJSON(4, false)
		fmt.Fprint(w, s[:strings.Index(s, `{"prefix":"10.0.2.0/24"`)])
		flushW(w)
		abort(w)
	}, ""))
	add("slow", func(w http.ResponseWriter, r *http.Request) { // 7 slower than the query timeout
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	add("v6", full(doc(ribJSON(2, true)), `{"rtrs":[{"descr":"v6-validator","remote_addr":"2001:db8::53","state":"connect","retry":600}]}`)) // 8 IPv6 results
	return specs, counters
}

var (
	m51SectionSplit = regexp.MustCompile(`<section class="(?:panel result-group|result-group panel)">`)
	m51SectionName  = regexp.MustCompile(`class="bname"[^>]*>([^<]+)<|<h2>([^<]+?) <small>`)
)

// m51Sections splits a route, neighbor or RTR page into its per-backend
// sections, keyed by backend name.
func m51Sections(page string) map[string]string {
	out := map[string]string{}
	for _, s := range m51SectionSplit.Split(page, -1)[1:] {
		if i := strings.Index(s, "</section>"); i >= 0 {
			s = s[:i]
		}
		if m := m51SectionName.FindStringSubmatch(s); m != nil {
			out[m[1]+m[2]] = s
		}
	}
	return out
}

func m51Matrix(t *testing.T, ttl time.Duration) (http.Handler, map[string]*m51Counter) {
	lg := defaults()
	lg.QueryTimeout = 400 * time.Millisecond
	lg.CacheTTL = ttl
	specs, counters := m51Backends(t)
	cfg, closeAll := testConfig(lg, specs...)
	t.Cleanup(closeAll)
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h.Routes(), counters
}

func TestMultiBackendMatrixKeepsEveryOutcomeSeparateAndAttributed(t *testing.T) {
	h, _ := m51Matrix(t, 0)
	page := m51Page(t, h, "/?type=prefix&q=10.0.0.0/24&backend=all", 200)
	sec := m51Sections(page)
	if len(sec) != 8 {
		t.Fatalf("%d backend sections, want 8: %v", len(sec), keys(sec))
	}
	routes := func(s string) int { return strings.Count(s, `<tbody class="route">`) }
	cases := []struct {
		name   string
		status string // statushead label
		rows   int
		want   []string
		not    []string
	}{
		{"B-ok", "COMPLETE", 3, []string{"3 routes"}, []string{"2001:db8", "Query failed", "Partial results"}},
		{"B-down", "FAILED", 0, []string{"Query failed", "not evidence that no matching routes exist"}, []string{"No matching routes"}},
		{"B-e500", "FAILED", 0, []string{"Query failed", "HTTP 500"}, []string{"No matching routes"}},
		{"B-bad", "FAILED", 0, []string{"Query failed"}, []string{"No matching routes"}},
		{"B-empty", "COMPLETE", 0, []string{"No matching routes", "0 routes"}, []string{"Query failed"}},
		{"B-part", "PARTIAL", 2, []string{"Partial results", "2 routes", "2 complete routes were decoded"}, []string{"No matching routes"}},
		{"B-slow", "FAILED", 0, []string{"Query failed"}, []string{"No matching routes"}},
		{"B-v6", "COMPLETE", 2, []string{"2 routes", "2001:db8:0::/48"}, []string{"10.0.", "Query failed"}},
	}
	for _, c := range cases {
		s, ok := sec[c.name]
		if !ok {
			t.Errorf("%s: no section", c.name)
			continue
		}
		if !strings.Contains(s, `">`+c.status+`</span>`) {
			t.Errorf("%s: status %s not shown:\n%.300s", c.name, c.status, textOf(s))
		}
		if got := routes(s); got != c.rows {
			t.Errorf("%s: %d route rows, want %d", c.name, got, c.rows)
		}
		for _, w := range c.want {
			if !strings.Contains(textOf(s), w) {
				t.Errorf("%s lacks %q:\n%s", c.name, w, textOf(s))
			}
		}
		for _, n := range c.not {
			if strings.Contains(textOf(s), n) {
				t.Errorf("%s must not contain %q (backends merged or mislabelled):\n%s", c.name, n, textOf(s))
			}
		}
	}
	// One overall warning, honest counts, one footer figure, one title.
	mustContain(t, page, "Incomplete results", "3 of 8 backends completed")
	if n := strings.Count(page, "Generated in"); n != 1 {
		t.Errorf("%d timing figures, want 1", n)
	}
	if n := strings.Count(page, "<title>"); n != 1 {
		t.Errorf("%d titles", n)
	}
	// A failed backend never reads as "no matching routes" anywhere on the page.
	if n := strings.Count(page, "No matching routes"); n != 1 {
		t.Errorf("'No matching routes' appears %d times, want exactly once (the genuinely empty backend)", n)
	}
}

func keys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

func TestMultiBackendMatrixCacheKeysAreBackendAwareAndFailuresAreNotCached(t *testing.T) {
	h, c := m51Matrix(t, 20*time.Second)
	target := "/?type=prefix&q=10.0.0.0/24&backend=all"
	first := m51Page(t, h, target, 200)
	second := m51Page(t, h, target, 200)
	sec1, sec2 := m51Sections(first), m51Sections(second)
	for _, name := range []string{"B-ok", "B-empty", "B-part", "B-v6"} {
		if strings.Contains(sec1[name], "cached") {
			t.Errorf("%s: a cold search must not be labelled cached", name)
		}
		if !strings.Contains(sec2[name], "cached ") || !strings.Contains(sec2[name], "not a historical snapshot") {
			t.Errorf("%s: warm result is not labelled cached (and as not a snapshot)", name)
		}
	}
	for _, name := range []string{"B-down", "B-e500", "B-bad", "B-slow"} {
		if strings.Contains(sec2[name], "cached ") {
			t.Errorf("%s: a failed outcome was served from cache", name)
		}
	}
	want := map[string]int64{"ok": 1, "empty": 1, "part": 1, "v6": 1, "down": 2, "e500": 2, "bad": 2, "slow": 2}
	for id, n := range want {
		if got := c[id].rib.Load(); got != n {
			t.Errorf("backend %s served %d /rib queries, want %d", id, got, n)
		}
	}
	// A different backend selection is a different cache key: asking only the
	// healthy backend must not return the other backends' cached routes, and
	// one backend's cached entry never answers for another.
	one := m51Page(t, h, "/?type=prefix&q=10.0.0.0/24&backend=ok", 200)
	if s := m51Sections(one); len(s) != 1 || s["B-ok"] == "" {
		t.Errorf("backend=ok produced sections %v", keys(s))
	}
	mustNotContain(t, one[strings.Index(one, `id="results"`):], "2001:db8", "B-v6", "B-down")
	if got := c["ok"].rib.Load(); got != 1 {
		t.Errorf("single-backend request re-queried the cached backend (%d queries)", got)
	}
	// Same query, different prefix: cache miss for every backend.
	m51Page(t, h, "/?type=prefix&q=10.9.9.0/24&backend=ok", 200)
	if got := c["ok"].rib.Load(); got != 2 {
		t.Errorf("new query was not sent upstream (%d)", got)
	}
}

func TestMultiBackendMatrixPaginationIsPerBackend(t *testing.T) {
	lg := defaults()
	lg.PageSize = 2
	specs := []backendSpec{
		{"one", "One", serveDoc(ribJSON(3, false))}, // 2 pages
		{"two", "Two", serveDoc(ribJSON(5, true))},  // 3 pages
		{"none", "None", serveDoc("{\n\n}\n")},      // no routes
	}
	h := newTestHandler(t, lg, specs...)
	p1 := m51Page(t, h, "/?type=prefix&q=10.0.0.0/24&backend=all", 200)
	// Pages are numbered over the combined list; every backend section stays
	// its own and says what it is showing.
	sec := m51Sections(p1)
	if len(sec) != 3 {
		t.Fatalf("sections %v", keys(sec))
	}
	mustContain(t, p1, "Page 1 of 4", "8 routes decoded")
	for _, name := range []string{"One", "Two", "None"} {
		if sec[name] == "" {
			t.Errorf("no section for %s", name)
		}
	}
	if strings.Contains(sec["One"], "2001:db8") || strings.Contains(sec["Two"], "10.0.") {
		t.Error("routes crossed backends")
	}
	// Page 2 continues the combined list: the empty backend keeps its empty status.
	p2 := m51Page(t, h, "/?type=prefix&q=10.0.0.0/24&backend=all&page=2", 200)
	mustContain(t, p2, "Page 2 of 4")
	// A single backend is paged on its own.
	b := m51Page(t, h, "/?type=prefix&q=10.0.0.0/24&backend=two&page=3", 200)
	mustContain(t, b, "Page 3 of 3")
	if s := m51Sections(b); len(s) != 1 {
		t.Errorf("backend=two page 3 sections %v", keys(s))
	}
	// Out of range and invalid pages.
	mustContain(t, m51Page(t, h, "/?type=prefix&q=10.0.0.0/24&backend=two&page=9", 200), "Page 9 does not exist")
	for _, bad := range []string{"0", "-1", "x", "1.5"} {
		rr := get(h, "/?type=prefix&q=10.0.0.0/24&backend=two&page="+bad)
		if rr.Code != 400 {
			t.Errorf("page=%s: status %d, want 400", bad, rr.Code)
		}
	}
	// Exactly one page: no pager.
	one := m51Page(t, h, "/?type=prefix&q=10.0.0.0/24&backend=none", 200)
	mustNotContain(t, one, `class="pager"`)
}

func TestMultiBackendRTRStatesStaySeparatePerBackend(t *testing.T) {
	h, c := m51Matrix(t, 0)
	page := m51Page(t, h, "/rtr", 200)
	sec := m51Sections(page)
	want := map[string]string{
		"B-ok":    "2 sessions",
		"B-down":  "unavailable",
		"B-e500":  "unavailable",
		"B-bad":   "malformed response",
		"B-empty": "no sessions",
		"B-part":  "unsupported", // bgplgd without /rtr (404)
		"B-slow":  "unavailable",
		"B-v6":    "1 session",
	}
	for name, badge := range want {
		s, ok := sec[name]
		if !ok {
			t.Errorf("%s: no RTR section", name)
			continue
		}
		if !strings.Contains(textOf(s), badge) {
			t.Errorf("%s: want badge %q, got:\n%s", name, badge, textOf(s))
		}
	}
	// "no sessions" is only for the backend that answered with a valid empty
	// document; unsupported, unavailable and malformed never say it.
	if n := strings.Count(page, "no sessions"); n != 1 {
		t.Errorf(`"no sessions" shown %d times, want 1`, n)
	}
	// Different outcomes never leak across backends.
	if strings.Contains(sec["B-v6"], "validator-1") || strings.Contains(sec["B-ok"], "v6-validator") {
		t.Error("RTR sessions crossed backends")
	}
	// The IPv6 validator's state is shown as the backend reported it, not as healthy.
	if s := textOf(sec["B-v6"]); !strings.Contains(s, "connect") || strings.Contains(sec["B-v6"], `pill good`) {
		t.Errorf("a connecting session must not be presented as good:\n%s", s)
	}
	mustNotContain(t, page, "2001:db8::53", "192.0.2.3")
	if n := strings.Count(page, "Generated in"); n != 1 {
		t.Errorf("%d timing figures", n)
	}
	// RTR goes to /rtr only: no route query is made for it.
	for id, k := range c {
		if k.rib.Load() != 0 {
			t.Errorf("%s: /rtr page caused %d /rib queries", id, k.rib.Load())
		}
	}
}

// ---- browser history: server contract for a rejected search ----

func TestHistoryRestoreOfRejectedSearchKeepsHTTP400AndServesTheFullPage(t *testing.T) {
	// Root cause of the M4.9 report: HTMX 2 swaps a history-restore response
	// only for 2xx/3xx, so a rejected search (400) left the old page under the
	// restored URL. The server contract is unchanged and must stay so: the
	// status remains 400 (never masked as success) and the body is a complete,
	// self-sufficient document; app.js falls back to an ordinary navigation of
	// the URL (testdata/m51-history.test.js, testdata/browser/regression.js).
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m51Real(t)})
	bad := "/?backend=all&type=prefix&q=999.1.1.1&view=table&prefix=&as="
	for name, hdr := range map[string][]string{
		"history restore":  {"HX-Request", "true", "HX-History-Restore-Request", "true"},
		"plain navigation": nil,
	} {
		rr := get(h, bad, hdr...)
		if rr.Code != 400 {
			t.Errorf("%s: status %d, want 400 (an invalid search must stay invalid)", name, rr.Code)
		}
		body := rr.Body.String()
		if n := strings.Count(body, "<title>"); n != 1 || !strings.Contains(body, "<title>Invalid search · Test LG</title>") {
			t.Errorf("%s: want exactly one Invalid search title in <head>, got %d\n%.300s", name, n, body)
		}
		if !strings.HasPrefix(strings.TrimSpace(body), "<!DOCTYPE html>") && !strings.HasPrefix(strings.TrimSpace(body), "<!doctype html>") {
			t.Errorf("%s: not a full document: %.80s", name, body)
		}
		mustContain(t, body, `id="search-form"`, `value="999.1.1.1"`, "Search could not be run", `id="advanced"`)
		mustNotContain(t, body, "hx-swap-oob", "Routes from")
		if strings.Count(body, `id="page-timing"`) != 1 {
			t.Errorf("%s: footer timing element count = %d", name, strings.Count(body, `id="page-timing"`))
		}
	}
	// The ordinary HTMX submit still gets a 400 fragment that begins with its title.
	rr := get(h, bad, "HX-Request", "true")
	if rr.Code != 400 || !strings.HasPrefix(rr.Body.String(), "<title>Invalid search · Test LG</title>") {
		t.Errorf("HTMX fragment: status %d, body %.120s", rr.Code, rr.Body.String())
	}
	// Invalid input is never converted into a success anywhere.
	for _, p := range []string{"/?type=prefix&q=999.1.1.1", "/?type=asn&q=AS-nope", "/?type=large-community&q=1:2", "/?prefix=10.0.0.0/8&neighbor=1.1.1.1&group=g", "/?page=0&type=prefix&q=10.0.0.0/8"} {
		if rr := get(h, p, "HX-Request", "true", "HX-History-Restore-Request", "true"); rr.Code != 400 {
			t.Errorf("%s: history restore status %d, want 400", p, rr.Code)
		}
	}
}

func TestHistoryFallbackScriptIsServedAndHistorySnapshotsStayDisabled(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m51Real(t)})
	js := get(h, "/static/app.js")
	if js.Code != 200 || !strings.Contains(js.Body.String(), "htmx:historyCacheMissLoadError") {
		t.Fatalf("app.js does not carry the history fallback (status %d)", js.Code)
	}
	mustContain(t, m51Page(t, h, "/", 200), `"historyCacheSize":0`)
}

// ---- hostile strings, read-only access ----

func TestHostileBackendTextIsEscapedEndToEnd(t *testing.T) {
	const x = `"><script>alert(1)</script><img src=x onerror=alert(2)>`
	var doc struct {
		Rib []map[string]any `json:"rib"`
	}
	if err := json.Unmarshal([]byte(m51Fixture(t, "rib-v4.json")), &doc); err != nil {
		t.Fatal(err)
	}
	r := doc.Rib[0]
	r["neighbor"] = map[string]any{"description": x, "remote_addr": "192.0.2.1", "bgp_id": "192.0.2.254"}
	r["communities"] = []string{x}
	r["large_communities"] = []string{x}
	r["source"] = x
	r["origin"] = x
	body, _ := json.Marshal(doc)
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", x, serveDoc(string(body))})
	defer closeAll()
	cfg.Site.Name = x
	cfg.Site.Operator = x
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	routes := h.Routes()
	for _, target := range []string{
		"/?type=prefix&q=198.51.100.0/24", "/?type=prefix&q=198.51.100.0/24&view=text", "/?type=prefix&q=198.51.100.0/24&view=detail",
		"/?type=prefix&q=" + "%22%3E%3Cscript%3Ealert(1)%3C%2Fscript%3E", "/rtr", "/neighbors", "/summary", "/communities?q=%3Cscript%3E",
	} {
		rr := get(routes, target)
		page := rr.Body.String()
		if strings.Contains(page, "<script>alert") || strings.Contains(page, "<img src=x") {
			t.Errorf("%s: unescaped hostile text in the page", target)
		}
		if n := strings.Count(page, "<title>"); n > 1 {
			t.Errorf("%s: %d titles", target, n)
		}
		csp := rr.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") {
			t.Errorf("%s: CSP %q", target, csp)
		}
		if strings.Contains(page, " style=") || regexp.MustCompile(`<script[^>]*>[^<]`).MatchString(page) {
			t.Errorf("%s: inline style or script (CSP would block it)", target)
		}
	}
}

func TestOnlyReadOnlyEndpointsReachTheBackend(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	inner := m51Real(t)
	rec := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = true
		mu.Unlock()
		inner(w, r)
	}
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", rec})
	for _, p := range []string{
		"/", "/?type=prefix&q=198.51.100.0/24", "/?type=asn&q=64502", "/?type=large-community&q=64500:0:8",
		"/?prefix=198.51.100.0/24&all=1", "/?prefix=198.51.100.0/24&view=text&output=plain", "/neighbors", "/summary", "/rtr",
		"/communities", "/healthz", "/readyz", "/ptr-status?ip=192.0.2.1", "/asn-status?asn=64502",
	} {
		get(h, p)
	}
	mu.Lock()
	defer mu.Unlock()
	allowed := map[string]bool{"GET /rib": true, "GET /neighbors": true, "GET /summary": true, "GET /rtr": true}
	for k := range seen {
		if !allowed[k] {
			t.Errorf("backend received %q: only read-only GET /rib, /neighbors, /summary and /rtr are allowed", k)
		}
	}
	for k := range allowed {
		if !seen[k] {
			t.Errorf("expected %q to be exercised", k)
		}
	}
}
