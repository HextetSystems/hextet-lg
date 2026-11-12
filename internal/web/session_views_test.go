package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

// ---- fixtures: shapes printed by bgpctl -j (OpenBGPD 9.3) -------------------

const m47Neighbors = `{"neighbors":[
{"remote_as":"64500","description":"edge <script>alert(1)</script>","group":"transit","remote_addr":"192.0.2.1","bgpid":"192.0.2.1","state":"Established","last_updown":"01:23:45","last_updown_sec":5025,
 "stats":{"last_read":"00:00:12","last_write":"00:00:03","prefixes":{"sent":7,"received":0},
  "message":{"sent":{"open":1,"notifications":0,"updates":9,"keepalives":40,"route_refresh":0,"total":50},"received":{"open":1,"notifications":0,"updates":11,"keepalives":41,"route_refresh":0,"total":53}},
  "update":{"sent":{"updates":9,"withdraws":1,"eor":2},"received":{"updates":11,"withdraws":0,"eor":2}}},
 "last_shutdown_reason":"maint <b>window</b>","last_error_sent":"Cease (Administrative Reset)",
 "session":{"holdtime":90,"keepalive":30,"local":{"address":"203.0.113.77","port":179},"capabilities":{"as4byte":true,"multiprotocol":["IPv4 unicast","IPv6 unicast<i>"]}}},
{"remote_as":"64501","remote_addr":"2001:db8::2","state":"Active","last_updown":"Never","stats":{"prefixes":{"sent":0,"received":0}}},
{"remote_addr":"198.51.100.9","state":"Idle"}
]}`

const m47Summary = `{"neighbors":[{"remote_as":"64500","description":"edge","remote_addr":"192.0.2.1","bgpid":"192.0.2.1","state":"Established","last_updown":"1d02h","last_updown_sec":93600},{"remote_as":"64501","remote_addr":"2001:db8::2","state":"Idle","last_updown":"Never"}]}`

const m47RTR = `{"rtrs":[
{"descr":"routinator <b>one</b>","remote_addr":"192.0.2.100","remote_port":323,"local_addr":"203.0.113.77","state":"established","version":2,"minimal_version":1,"session_id":7,"serial":4242,"refresh":3600,"retry":600,"expire":7200,
 "timers":[{"name":"RTR RefreshTimer","due":3100},{"name":"RTR ExpireTimer","due":0}]},
{"remote_addr":"2001:db8::100","remote_port":3323,"state":"closed","refresh":3600,"retry":600,"expire":7200,"last_recv_error":"Transport Error","last_recv_msg":"refused <img src=x onerror=alert(1)>"},
{"descr":"half","remote_addr":"192.0.2.101","state":"established"},
{"remote_addr":"192.0.2.102"}
]}`

// m47Backend serves fixed bodies; an empty string for a path means 404.
func m47Backend(neighbors, summary, rtr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{"/neighbors": neighbors, "/summary": summary, "/rtr": rtr}[r.URL.Path]
		if body == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}
}

func m47Handler(t *testing.T, edit func(*config.Config), specs ...backendSpec) http.Handler {
	t.Helper()
	cfg, closeAll := testConfig(defaults(), specs...)
	t.Cleanup(closeAll)
	if edit != nil {
		edit(&cfg)
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h.Routes()
}

func b3(t bool) *bool { return &t }

func mustContain(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(page, w) {
			t.Errorf("missing %q", w)
		}
	}
}

func mustNotContain(t *testing.T, page string, bads ...string) {
	t.Helper()
	for _, b := range bads {
		if strings.Contains(page, b) {
			t.Errorf("unexpected %q", b)
		}
	}
}

// ---- feature flags ------------------------------------------------------------

func TestSessionFeatureFlagsAreIndependent(t *testing.T) {
	cases := []struct {
		name     string
		bgp, rtr *bool
		wantBGP  bool
		wantRTR  bool
	}{
		{"both omitted", nil, nil, true, true},
		{"both enabled", b3(true), b3(true), true, true},
		{"both disabled", b3(false), b3(false), false, false},
		{"only bgp", b3(true), b3(false), true, false},
		{"only rtr", b3(false), b3(true), false, true},
		{"bgp omitted rtr disabled", nil, b3(false), true, false},
		{"bgp disabled rtr omitted", b3(false), nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := m47Handler(t, func(c *config.Config) { c.Features.BGPNeighbors, c.Features.RTRSessions = tc.bgp, tc.rtr },
				backendSpec{"a", "Alpha", m47Backend(m47Neighbors, m47Summary, m47RTR)})
			for _, path := range []string{"/neighbors"} {
				if got := get(h, path).Code; (got == 200) != tc.wantBGP || (!tc.wantBGP && got != 404) {
					t.Errorf("%s: status %d, want enabled=%v", path, got, tc.wantBGP)
				}
			}
			if got := get(h, "/rtr").Code; (got == 200) != tc.wantRTR || (!tc.wantRTR && got != 404) {
				t.Errorf("/rtr: status %d, want enabled=%v", got, tc.wantRTR)
			}
			// Navigation (on every page kind) mirrors the flags exactly.
			for _, page := range []string{"/", "/communities"} {
				body := get(h, page).Body.String()
				for link, want := range map[string]bool{`href="/neighbors"`: tc.wantBGP, `href="/summary"`: false, `href="/rtr"`: tc.wantRTR} {
					if strings.Contains(body, link) != want {
						t.Errorf("%s: nav %s present=%v want %v", page, link, !want, want)
					}
				}
				mustContain(t, body, `href="/communities"`)
			}
		})
	}
}

func TestDisabledSessionViewDoesNotReachTheBackend(t *testing.T) {
	var hits atomic.Int32
	h := m47Handler(t, func(c *config.Config) { c.Features.RTRSessions = b3(false) },
		backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) { hits.Add(1); fmt.Fprint(w, m47RTR) }})
	for _, target := range []string{"/rtr", "/rtr?backend=a", "/rtr?backend=all"} {
		if rr := get(h, target); rr.Code != 404 || strings.Contains(rr.Body.String(), "Validator") {
			t.Fatalf("%s: %d", target, rr.Code)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("backend contacted %d times for a disabled feature", hits.Load())
	}
}

// ---- BGP neighbors ------------------------------------------------------------

func TestBGPNeighborDetailsAndEscaping(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m47Backend(m47Neighbors, m47Summary, "")})
	page := get(h, "/neighbors").Body.String()
	mustContain(t, page,
		`<span class="pill good">Established</span>`, `<span class="pill warn">Active</span>`, `<span class="pill muted">Idle</span>`,
		`up 01:23:45`,
		`<th>Prefixes received / sent</th>`, `<th>Messages received / sent</th>`,
		`>0 / 7<`, `>53 / 50<`,
		// details grid: only reported values, labelled
		`<span>Group</span><strong>transit</strong>`, `<span>BGP ID</span>`, `<span>Hold time</span><strong>90 s</strong>`,
		`<span>Last read</span>`,
		`open 1 · notifications 0 · updates 11 · keepalives 41 · route refresh 0 · total 53`,
		`updates 9 · withdraws 1 · EOR 2`,
		`Cease (Administrative Reset)`,
		`<span>Address families</span>`,
		// IPv4 and IPv6 neighbors link to route search without an af filter
		`href="/?neighbor=192.0.2.1&amp;backend=a&amp;view=table"`,
		`neighbor=2001%3Adb8%3A%3A2&amp;backend=a`,
	)
	// Backend text is escaped everywhere it is rendered.
	mustContain(t, page, `edge &lt;script&gt;alert(1)&lt;/script&gt;`, `maint &lt;b&gt;window&lt;/b&gt;`, `IPv6 unicast&lt;i&gt;`)
	mustNotContain(t, page, `<script>alert(1)`, `<b>window</b>`, `<i>`)
	// af is never inferred from a neighbor address.
	mustNotContain(t, page, `af=`)
	// A neighbor with nothing reported shows dashes, not zeros or invented rows.
	idle := page[strings.Index(page, "198.51.100.9"):]
	idle = idle[:strings.Index(idle, "</tr>")]
	mustContain(t, idle, `<span class="pill muted">Idle</span>`)
	mustNotContain(t, idle, ">0 / 0<", "Never")
	// Message totals for the Active neighbor (stats without message) stay absent.
	active := page[strings.Index(page, "2001:db8::2"):]
	active = active[:strings.Index(active, "</tr>")]
	mustContain(t, active, `>0 / 0<`, `<td>—</td>`)
}

func TestNeighborsIncludeSummaryCountsAndOmitAbsentCounters(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m47Backend(m47Neighbors, m47Summary, "")})
	page := get(h, "/neighbors").Body.String()
	mustContain(t, page, `Sessions reported`, `>2</strong><span>Sessions reported`, `>1</strong><span>Established`, `>1</strong><span>Other states`, `192.0.2.1`, `up 01:23:45`)
	mustContain(t, page, `Prefixes received / sent`, `Messages received / sent`)
	// The detail grid only exists when there is something to show.
	mustContain(t, page, `<span>BGP ID</span>`)
}

func TestSameNeighborAddressOnTwoBackendsStaysSeparate(t *testing.T) {
	h := m47Handler(t, nil,
		backendSpec{"a", "Alpha", m47Backend(m47Neighbors, "", "")},
		backendSpec{"b", "Beta", m47Backend(`{"neighbors":[{"remote_addr":"192.0.2.1","remote_as":"64999","state":"Idle","description":"other"}]}`, "", "")})
	page := get(h, "/neighbors?backend=all").Body.String()
	mustContain(t, page, `neighbor=192.0.2.1&amp;backend=a&amp;view=table`, `neighbor=192.0.2.1&amp;backend=b&amp;view=table`, "Alpha", "Beta")
	if strings.Count(page, `class="panel result-group"`) != 2 {
		t.Fatal("backends must be separate panels")
	}
	// Selecting one backend shows only that backend's neighbor.
	one := get(h, "/neighbors?backend=b").Body.String()
	mustContain(t, one, "other", `<option value="b" selected>`)
	mustNotContain(t, one, "transit")
	if strings.Count(one, `class="panel result-group"`) != 1 {
		t.Fatal("only the selected backend may be queried and shown")
	}
}

func TestSummaryCountsRemainBackendSpecificAndShowForEmptyNeighborList(t *testing.T) {
	h := m47Handler(t, nil,
		backendSpec{"a", "Alpha", m47Backend(`{"neighbors":[]}`, `{"neighbors":[{"state":"Established"},{"state":"Idle"}]}`, "")},
		backendSpec{"b", "Beta", m47Backend(`{"neighbors":[{"remote_addr":"192.0.2.8","state":"Established"}]}`, `{"neighbors":[{"state":"Established"}]}`, "")})
	page := get(h, "/neighbors?backend=all").Body.String()
	if strings.Count(page, `class="summary-stats"`) != 2 {
		t.Fatalf("expected independent count blocks for both backends")
	}
	if strings.Count(page, `>2</strong><span>Sessions reported`) != 1 || strings.Count(page, `>1</strong><span>Sessions reported`) != 1 {
		t.Fatalf("summary counts were not kept separate:\n%s", page)
	}
	if !strings.Contains(page, "No neighbors reported by this backend.") || !strings.Contains(page, "192.0.2.8") {
		t.Fatal("empty and populated neighbor tables should both render")
	}
}

func TestSummaryFailureDoesNotHideNeighborRows(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/neighbors" {
			fmt.Fprint(w, m47Neighbors)
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}})
	page := get(h, "/neighbors").Body.String()
	mustContain(t, page, "Session summary unavailable", "192.0.2.1", "Neighbor details")
}

func TestBGPBackendFailureDoesNotHideOthers(t *testing.T) {
	h := m47Handler(t, nil,
		backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }},
		backendSpec{"b", "Beta", m47Backend(m47Neighbors, "", "")})
	rr := get(h, "/neighbors")
	page := rr.Body.String()
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	mustContain(t, page, "Backend unavailable", "192.0.2.1", `<span class="pill good">Established</span>`)
}

// ---- RTR ---------------------------------------------------------------------

func rtrPage(t *testing.T, edit func(*config.Config), rtr string) string {
	t.Helper()
	h := m47Handler(t, edit, backendSpec{"a", "Alpha", m47Backend(m47Neighbors, m47Summary, rtr)})
	rr := get(h, "/rtr")
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	return rr.Body.String()
}

func TestRTRSessionsRenderedWithoutAddressesByDefault(t *testing.T) {
	page := rtrPage(t, nil, m47RTR)
	mustContain(t, page,
		`4 sessions`,
		`<span class="pill good">established</span>`,
		`<span class="pill bad">closed</span>`,
		`v2 (min v1)`, `<td>7</td>`, `<td>4242</td>`,
		`1h / 10m / 2h`, `title="Refresh / retry / expire interval: 3600 s / 600 s / 7200 s"`,
		`<th>Last error</th>`, `Last error received:`, `Transport Error — refused`,
		`RTR RefreshTimer`, `in 51m40s`, `RTR ExpireTimer`, `<strong>due</strong>`,
		// no description -> ordinal, never an address
		`Validator 2`, `Validator 4`,
	)
	// Addresses, ports and the local address are not published by default.
	mustNotContain(t, page, "192.0.2.100", "2001:db8::100", "203.0.113.77", ":323", "Local address", "192.0.2.101")
	// Backend text is escaped; no raw JSON dump exists for RTR.
	mustContain(t, page, `routinator &lt;b&gt;one&lt;/b&gt;`, `refused &lt;img src=x onerror=alert(1)&gt;`)
	mustNotContain(t, page, `<b>one</b>`, `<img`, `"rtrs"`, `Raw backend response`)
	// Not a BGP page: no neighbor data leaks in.
	mustNotContain(t, page, "Neighbor Explorer", "transit")
}

func TestRTRAddressesOnlyWhenEnabled(t *testing.T) {
	page := rtrPage(t, func(c *config.Config) { c.Features.RTRSessionAddresses = b3(true) }, m47RTR)
	mustContain(t, page, `<code>192.0.2.100:323</code>`, `<code>[2001:db8::100]:3323</code>`, `<code>192.0.2.101</code>`, `<span>Local address</span><strong>203.0.113.77</strong>`)
}

func TestRTREstablishedWithoutIdentityIsNotPresentedAsHealthy(t *testing.T) {
	page := rtrPage(t, nil, m47RTR)
	// "half" is established but reported no session ID or serial.
	row := page[strings.Index(page, "half"):]
	row = row[:strings.Index(row, "</tr>")]
	mustContain(t, row, `class="pill warn"`, `not confirmed as synchronized`)
	mustNotContain(t, row, `pill good`)
	// No state at all stays neutral and says so.
	row = page[strings.Index(page, "Validator 4"):]
	row = row[:strings.Index(row, "</tr>")]
	mustContain(t, row, `class="pill muted"`, `>unknown<`)
	mustNotContain(t, row, `pill good`)
}

func TestRTRZeroSessionsDiffersFromUnsupported(t *testing.T) {
	none := rtrPage(t, nil, `{}`)
	mustContain(t, none, `reports no RTR sessions`, `no sessions`)
	mustNotContain(t, none, "not supported", "Backend unavailable")
}

func TestRTRCapabilityStatesAreNeverConflated(t *testing.T) {
	h := m47Handler(t, nil,
		backendSpec{"a", "Active", m47Backend("", "", m47RTR)},
		backendSpec{"b", "Bare", m47Backend("", "", `{}`)},
		backendSpec{"c", "Old", m47Backend("", "", "")}, // 404: bgplgd without /rtr
		backendSpec{"d", "Down", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 503) }},
		backendSpec{"e", "Empty", func(w http.ResponseWriter, r *http.Request) {}}, // 200, empty body
		backendSpec{"f", "Junk", m47Backend("", "", `<html>proxy</html>`)},
	)
	rr := get(h, "/rtr?backend=all")
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	page := rr.Body.String()
	section := func(name string) string {
		i := strings.Index(page, "<h2>"+name)
		if i < 0 {
			t.Fatalf("no panel for %s", name)
		}
		s := page[i:]
		if j := strings.Index(s[4:], `<section class="panel`); j >= 0 {
			s = s[:j+4]
		}
		return s
	}
	mustContain(t, section("Active"), "4 sessions", "Validator 2")
	mustContain(t, section("Bare"), "no sessions", "reports no RTR sessions")
	old := section("Old")
	mustContain(t, old, "unsupported", "does not provide /rtr (HTTP 404)", "not the same as having no RTR sessions")
	mustNotContain(t, old, "reports no RTR sessions")
	down := section("Down")
	mustContain(t, down, "unavailable", "Backend returned HTTP 503")
	mustNotContain(t, down, "reports no RTR sessions", "not supported")
	for _, name := range []string{"Empty", "Junk"} {
		s := section(name)
		mustContain(t, s, "malformed response", "Unexpected backend response")
		mustNotContain(t, s, "reports no RTR sessions", "not supported")
	}
	// Failures of other backends never hide the working one.
	mustContain(t, page, "Validator 2")
}

func TestRTRRequestsOnlyRTR(t *testing.T) {
	var paths []string
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		fmt.Fprint(w, m47RTR)
	}})
	get(h, "/rtr?backend=a")
	if len(paths) != 1 || paths[0] != "/rtr" {
		t.Fatalf("%v", paths)
	}
}

func TestRTRParametersAndBackendSelection(t *testing.T) {
	h := m47Handler(t, nil,
		backendSpec{"a", "Alpha", m47Backend("", "", m47RTR)},
		backendSpec{"b", "Beta", m47Backend("", "", `{}`)})
	if rr := get(h, "/rtr?x=1"); rr.Code != 400 {
		t.Errorf("unknown parameter: %d", rr.Code)
	}
	if rr := get(h, "/rtr?backend=a&backend=b"); rr.Code != 400 {
		t.Errorf("repeated parameter: %d", rr.Code)
	}
	one := get(h, "/rtr?backend=b").Body.String()
	mustContain(t, one, `<option value="b" selected>`, "Beta", "no sessions")
	mustNotContain(t, one, "Validator")
	if strings.Count(one, `class="panel result-group"`) != 1 {
		t.Fatal("only the selected backend may be shown")
	}
	// Backend values are IDs; an address is never accepted as a selector.
	bad := get(h, "/rtr?backend=http://192.0.2.1:9099").Body.String()
	mustContain(t, bad, "Select an enabled backend.")
	mustNotContain(t, bad, "192.0.2.1:9099/rtr")
}

// ---- navigation, HTMX and history ---------------------------------------------

func TestSessionPagesAreFullDocumentsForHTMXAndHistoryRestore(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m47Backend(m47Neighbors, m47Summary, m47RTR)})
	for _, path := range []string{"/rtr", "/neighbors"} {
		direct := get(h, path)
		for name, hdrs := range map[string][]string{
			"htmx":             {"HX-Request", "true"},
			"history restore":  {"HX-Request", "true", "HX-History-Restore-Request", "true"},
			"htmx with target": {"HX-Request", "true", "HX-Target", "results"},
		} {
			rr := get(h, path, hdrs...)
			body := rr.Body.String()
			if rr.Code != 200 || !strings.HasPrefix(body, "<!doctype html>") || !strings.Contains(body, "<nav") || !strings.Contains(body, `<link rel="stylesheet" href="/static/theme.css`) {
				t.Errorf("%s %s: not a complete document (%d)", path, name, rr.Code)
			}
			if rr.Header().Get("Content-Security-Policy") == "" || direct.Header().Get("Content-Security-Policy") != rr.Header().Get("Content-Security-Policy") {
				t.Errorf("%s %s: CSP differs", path, name)
			}
		}
		page := direct.Body.String()
		// The theme lifecycle script stays in <head> after the stylesheets.
		mustContain(t, page, `theme-init.js`)
		if strings.Index(page, "theme.css") > strings.Index(page, "theme-init.js") {
			t.Errorf("%s: theme-init.js must follow the stylesheets", path)
		}
		// No inline styles or colors on the new pages (CSP style-src 'self').
		mustNotContain(t, page, ` style="`, "<style")
	}
	// The nav marks exactly the active page.
	mustContain(t, get(h, "/rtr").Body.String(), `<a href="/rtr" aria-current="page">RTR sessions</a>`)
	mustContain(t, get(h, "/neighbors").Body.String(), `<a href="/neighbors" aria-current="page">Neighbors</a>`)
}

func TestSessionPagesAddNoClientScriptOrPolling(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m47Backend(m47Neighbors, m47Summary, m47RTR)})
	for _, path := range []string{"/rtr", "/neighbors"} {
		page := get(h, path).Body.String()
		mustNotContain(t, page, "hx-trigger", "hx-get", "setInterval", "EventSource")
	}
}

func TestExistingRouteNavigationUnaffectedBySessionFlags(t *testing.T) {
	h := m47Handler(t, func(c *config.Config) { c.Features.BGPNeighbors, c.Features.RTRSessions = b3(false), b3(false) },
		backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"rib":[{"prefix":"192.0.2.0/24","neighbor":{"remote_addr":"192.0.2.1"}}]}`)
		}})
	rr := get(h, "/?type=prefix&q=192.0.2.0%2F24&backend=a")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "192.0.2.0/24") {
		t.Fatalf("%d", rr.Code)
	}
	if rr := get(h, neighborURL("192.0.2.1", "a")); rr.Code != 200 {
		t.Fatalf("neighbor route link must keep working with session views disabled: %d", rr.Code)
	}
}

// ---- observability ------------------------------------------------------------

func TestRTRUsesFixedRouteAndOperationLabels(t *testing.T) {
	if routeName("/rtr") != "rtr" {
		t.Fatal(routeName("/rtr"))
	}
	found := false
	for _, op := range backendOperations {
		found = found || op == "rtr"
	}
	if !found {
		t.Fatal("rtr missing from fixed backend operations")
	}
	// Unsupported / unavailable outcomes are recorded without a query string
	// or backend address anywhere in the metric output.
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", m47Backend("", "", "")})
	defer closeAll()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	get(h.Routes(), "/rtr")
	rr := httptest.NewRecorder()
	h.MetricsHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	mustContain(t, body, `operation="rtr"`, `route="rtr"`)
	// An unsupported backend is not a backend failure.
	mustNotContain(t, body, `hextet_lg_backend_failures_total{backend="a",operation="rtr"`)
}

func TestSummaryFrontendRouteIsRemoved(t *testing.T) {
	h := m47Handler(t, nil, backendSpec{"a", "Alpha", m47Backend(m47Neighbors, m47Summary, "")})
	rr := get(h, "/summary")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/summary status %d, want 404", rr.Code)
	}
}
