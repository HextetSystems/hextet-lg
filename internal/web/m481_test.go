package web

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

// M4.8.1: compact search/results layout and dynamic document titles.

var titleElement = regexp.MustCompile(`(?s)<title>.*?</title>`)

// titleOf returns the unescaped text of the single <title> element, failing
// when there is not exactly one.
func titleOf(t *testing.T, body string) string {
	t.Helper()
	m := titleElement.FindAllString(body, -1)
	if len(m) != 1 {
		t.Fatalf("want exactly one <title>, got %d in %.300s", len(m), body)
	}
	return html.UnescapeString(strings.TrimSuffix(strings.TrimPrefix(m[0], "<title>"), "</title>"))
}

func m481Handler(t *testing.T) http.Handler {
	t.Helper()
	return m48Handler(t, nil,
		backendSpec{"a", "Alpha", serveDoc(ribJSON(3, false))},
		backendSpec{"b", "Beta", serveDoc(ribJSON(2, true))},
	)
}

func TestRouteExplorerTitles(t *testing.T) {
	h := m481Handler(t)
	long := strings.Repeat("x", 200)
	for _, tc := range []struct {
		name, target, want string
		status             int
	}{
		{"default", "/", "Route Explorer · Test LG", 200},
		{"prefix", "/?type=prefix&q=10.0.0.0/24&backend=all&view=table", "10.0.0.0/24 · Test LG", 200},
		{"address, no backend param", "/?type=prefix&q=10.0.0.1", "10.0.0.1 · Test LG", 200},
		{"ipv6 is normalized", "/?type=prefix&q=2001:0DB8:0:0:0:0:0:0/32&backend=all", "2001:db8::/32 · Test LG", 200},
		{"asn is normalized", "/?type=asn&q=as64500&backend=all", "AS64500 · Test LG", 200},
		{"large community", "/?type=large-community&q=395089:5:2&backend=all", "395089:5:2 · Test LG", 200},
		{"standard community", "/?community=64500:1&backend=all", "Community 64500:1 · Test LG", 200},
		{"extended community", "/?ext-community=rt%2064500:1&backend=all", "Extended community rt 64500:1 · Test LG", 200},
		{"neighbor ipv4", "/?neighbor=192.0.2.1&backend=a", "Neighbor 192.0.2.1 @ Alpha · Test LG", 200},
		{"neighbor ipv6 is normalized", "/?neighbor=2001:DB8::1&backend=b", "Neighbor 2001:db8::1 @ Beta · Test LG", 200},
		{"peer group", "/?group=transit&backend=a", "Peer group transit @ Alpha · Test LG", 200},
		{"one backend is named, all is not", "/?type=prefix&q=10.0.0.0/24&backend=a", "10.0.0.0/24 @ Alpha · Test LG", 200},
		{"unknown backend id is rejected, never echoed", "/?type=prefix&q=10.0.0.0/24&backend=nope", "Invalid search · Test LG", 400},
		{"advanced filters are counted, not listed", "/?prefix=10.0.0.0/24&ovs=valid&best=1&backend=all", "10.0.0.0/24 +2 filters · Test LG", 200},
		{"one extra filter", "/?prefix=10.0.0.0/24&ovs=valid&backend=all", "10.0.0.0/24 +1 filter · Test LG", 200},
		{"prefix mode", "/?type=prefix&q=10.0.0.0/24&all=1&backend=all", "10.0.0.0/24 (more specifics) · Test LG", 200},
		{"covering prefixes", "/?type=prefix&q=10.0.0.0/24&or-shorter=1&backend=all", "10.0.0.0/24 (covering prefixes) · Test LG", 200},
		{"no primary filter", "/?ovs=invalid&backend=all", "Route search (1 filter) · Test LG", 200},
		{"no primary filters", "/?ovs=invalid&af=ipv4&backend=all", "Route search (2 filters) · Test LG", 200},
		{"invalid prefix is not echoed", "/?type=prefix&q=notaprefix&backend=all", "Invalid search · Test LG", 400},
		{"over-long invalid value", "/?type=prefix&q=" + strings.Repeat("9", 300), "Invalid search · Test LG", 400},
		{"unknown parameter", "/?type=prefix&q=10.0.0.0/24&bogus=1", "Invalid search · Test LG", 400},
		{"bad page", "/?type=prefix&q=10.0.0.0/24&page=zero", "Invalid search · Test LG", 400},
		{"long neighbor description is bounded", "/?neighbor=" + long + "&backend=all", "Neighbor " + strings.Repeat("x", 54) + "… · Test LG", 200},
		{"special characters", "/?neighbor=" + url.QueryEscape(`<b>"x"&y`) + "&backend=all", `Neighbor <b>"x"&y · Test LG`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, hx := range []bool{false, true} {
				var rr = get(h, tc.target)
				if hx {
					rr = get(h, tc.target, "HX-Request", "true")
				}
				if rr.Code != tc.status {
					t.Fatalf("hx=%v: status %d, want %d", hx, rr.Code, tc.status)
				}
				if got := titleOf(t, rr.Body.String()); got != tc.want {
					t.Errorf("hx=%v: title %q, want %q", hx, got, tc.want)
				}
			}
		})
	}
}

func TestTitleIsEscapedAndBounded(t *testing.T) {
	h := m481Handler(t)
	rr := get(h, "/?neighbor="+url.QueryEscape(`</title><script>alert(1)</script>`)+"&backend=all")
	body := rr.Body.String()
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;/title&gt;") {
		t.Fatalf("title text was not escaped: %.400s", body)
	}
	if n := strings.Count(body, "<title>"); n != 1 {
		t.Fatalf("%d <title> elements", n)
	}
	// Non-ASCII input is rejected upstream of the title; rune-safe truncation is covered by TestTitleTextSanitizes.
}

func TestTitleTextSanitizes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"plain", 10, "plain"},
		{"  spaced \t out\n", 20, "spaced out"},
		{"a\x00b\x1bc", 10, "abc"},
		{"a‮b​c", 10, "abc"}, // bidi override and zero-width space
		{"abcdefghij", 5, "abcd…"},
		{"ééééé", 3, "éé…"},
		{"日本語日本語", 4, "日本語…"},
		{"", 5, ""},
	} {
		if got := titleText(tc.in, tc.max); got != tc.want {
			t.Errorf("titleText(%q,%d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestPageTitlesOfOtherViews(t *testing.T) {
	h := m481Handler(t)
	for target, want := range map[string]string{
		"/neighbors":                "BGP Neighbors · Test LG",
		"/neighbors?backend=a":      "BGP Neighbors @ Alpha · Test LG",
		"/summary":                  "BGP Summary · Test LG",
		"/summary?backend=b":        "BGP Summary @ Beta · Test LG",
		"/rtr":                      "RTR Sessions · Test LG",
		"/rtr?backend=a":            "RTR Sessions @ Alpha · Test LG",
		"/communities":              "Communities · Test LG",
		"/communities?q=x&category": "Communities · Test LG",
	} {
		rr := get(h, target)
		if got := titleOf(t, rr.Body.String()); got != want {
			t.Errorf("%s: title %q, want %q", target, got, want)
		}
	}
}

func TestSiteNameIsTheOnlyTitleSuffix(t *testing.T) {
	h := m48Handler(t, func(c *config.Config) { c.Server.Title = "Other Name" }, backendSpec{"a", "Alpha", serveDoc(ribJSON(1, false))})
	for target, want := range map[string]string{
		"/":                                   "Route Explorer · Other Name",
		"/?type=prefix&q=10.0.0.0/24":         "10.0.0.0/24 · Other Name",
		"/communities":                        "Communities · Other Name",
		"/?type=prefix&q=bad":                 "Invalid search · Other Name",
		"/neighbors?backend=a":                "BGP Neighbors @ Alpha · Other Name",
		"/?type=prefix&q=10.0.0.0/24&page=1&": "10.0.0.0/24 · Other Name",
	} {
		if got := titleOf(t, get(h, target).Body.String()); got != want {
			t.Errorf("%s: %q, want %q", target, got, want)
		}
	}
}

func TestPagedSearchTitleNamesThePage(t *testing.T) {
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(ribJSON(150, false))})
	if got := titleOf(t, get(h, "/?type=asn&q=64500&backend=a").Body.String()); got != "AS64500 @ Alpha · Test LG" {
		t.Errorf("page 1: %q", got)
	}
	for _, hx := range []bool{false, true} {
		rr := get(h, "/?type=asn&q=64500&backend=a&page=2")
		if hx {
			rr = get(h, "/?type=asn&q=64500&backend=a&page=2", "HX-Request", "true")
		}
		if got := titleOf(t, rr.Body.String()); got != "AS64500 @ Alpha (page 2) · Test LG" {
			t.Errorf("hx=%v page 2: %q", hx, got)
		}
	}
}

// A fragment carries the title as its own <title> (which HTMX applies to the
// document); a full page, including the one served for a history restore,
// carries exactly one in <head> and none in the body.
func TestTitleTransportFullPageFragmentAndHistoryRestore(t *testing.T) {
	h := m481Handler(t)
	target := "/?type=asn&q=64500&backend=all&view=table"
	full := get(h, target).Body.String()
	frag := get(h, target, "HX-Request", "true").Body.String()
	restore := get(h, target, "HX-Request", "true", "HX-History-Restore-Request", "true").Body.String()

	want := "AS64500 · Test LG"
	for name, body := range map[string]string{"full": full, "fragment": frag, "restore": restore} {
		if got := titleOf(t, body); got != want {
			t.Errorf("%s: %q", name, got)
		}
	}
	if !strings.HasPrefix(frag, "<title>") {
		t.Errorf("fragment should start with its title: %.60s", frag)
	}
	head := full[:strings.Index(full, "</head>")]
	if !strings.Contains(head, "<title>"+want+"</title>") {
		t.Error("full page title is not in <head>")
	}
	if !strings.Contains(restore, "<html") || strings.HasPrefix(restore, "<title>") {
		t.Error("history restore must be a full document")
	}
	// Clearing the search is a plain link to "/", the default title.
	if !strings.Contains(full, `<a href="/">Clear search</a>`) || titleOf(t, get(h, "/").Body.String()) != "Route Explorer · Test LG" {
		t.Error("clear search must lead to the default title")
	}
}

// Pushed URL and title describe the same canonical query.
func TestFragmentTitleMatchesPushedURL(t *testing.T) {
	h := m481Handler(t)
	rr := get(h, "/?type=asn&q=AS64500&backend=a&view=table", "HX-Request", "true")
	push := rr.Header().Get("HX-Push-Url")
	if !strings.Contains(push, "q=64500") || titleOf(t, rr.Body.String()) != "AS64500 @ Alpha · Test LG" {
		t.Errorf("push %q title %q", push, titleOf(t, rr.Body.String()))
	}
	// A rejected search pushes nothing and says so in its title.
	bad := get(h, "/?type=asn&q=zzz&backend=a", "HX-Request", "true")
	if bad.Header().Get("HX-Push-Url") != "" || titleOf(t, bad.Body.String()) != "Invalid search · Test LG" {
		t.Errorf("invalid: push %q title %q", bad.Header().Get("HX-Push-Url"), titleOf(t, bad.Body.String()))
	}
}

func TestTitlesDoNotLeakBackendAddressesOrRawInput(t *testing.T) {
	h := m481Handler(t)
	for _, target := range []string{"/?type=prefix&q=10.0.0.0/24&backend=a", "/?type=prefix&q=nope&backend=a", "/neighbors?backend=a", "/?neighbor=x&backend=a&secret=1"} {
		title := titleOf(t, get(h, target).Body.String())
		if strings.Contains(title, "127.0.0.1") || strings.Contains(title, "http") || strings.Contains(title, "nope") || strings.Contains(title, "secret") {
			t.Errorf("%s: title leaks input or backend address: %q", target, title)
		}
	}
}

// ---- layout -------------------------------------------------------------------

func TestCompactSearchControls(t *testing.T) {
	h := m481Handler(t)
	body := get(h, "/").Body.String()
	form := body[strings.Index(body, `<form id="search-form"`):strings.Index(body, "</form>")]

	order := []string{`name="backend"`, `name="type"`, `name="q"`, `name="view"`, `type="submit"`}
	last := -1
	for _, want := range order {
		i := strings.Index(form, want)
		if i < 0 || i < last {
			t.Fatalf("main controls must be in the order backend, type, value, view, submit; %s misplaced", want)
		}
		last = i
	}
	// Every control keeps an accessible name through a real <label>, the form is
	// a labelled search landmark and the submit button is named.
	for _, want := range []string{
		`role="search"`, `aria-label="Route search"`,
		`<label class="c-backend"><span class="sr-only">Backend</span>`,
		`<label class="c-type"><span class="sr-only">Search by</span>`,
		`<label class="c-query"><span class="sr-only">Search value</span>`,
		`<label class="c-view"><span class="sr-only">View</span>`,
		`aria-label="Search routes"`,
	} {
		if !strings.Contains(form, want) {
			t.Errorf("missing %s", want)
		}
	}
	// One heading, no intro eyebrow, no large explanatory paragraph under the form.
	if strings.Count(body, "<h1>") != 1 || strings.Contains(body, "ROUTING INTELLIGENCE") || strings.Contains(body, `class="intro"`) {
		t.Error("page heading must be a single compact line")
	}
	if strings.Contains(body, "Results are live queries; the same URL") {
		t.Error("long explanatory paragraph must not sit under the form")
	}
	// Secondary row: Advanced filters (closed), Clear search, Copy link.
	if !strings.Contains(form, `<details class="advanced" id="advanced" >`) {
		t.Error("Advanced filters must be collapsed by default")
	}
	for _, want := range []string{`<a href="/">Clear search</a>`, `<button type="button" data-copy-link>Copy link</button>`, `id="copy-status"`, `role="status"`} {
		if !strings.Contains(form, want) {
			t.Errorf("missing secondary control %s", want)
		}
	}
	// The route explorer has exactly one Copy link: the one beside the search.
	if strings.Count(body, ">Copy link<") != 1 || strings.Contains(body, `id="copy-link"`) {
		t.Errorf("want one Copy link on the explorer, got %d", strings.Count(body, ">Copy link<"))
	}
	// Other pages keep the navigation Copy link.
	if o := get(h, "/communities").Body.String(); strings.Count(o, ">Copy link<") != 1 || !strings.Contains(o, `id="copy-link" data-copy-link`) {
		t.Error("non-explorer pages keep the navigation Copy link")
	}
}

func TestAdvancedFiltersStayCollapsedAfterSearch(t *testing.T) {
	h := m481Handler(t)
	body := get(h, "/?prefix=10.0.0.0/24&ovs=valid&best=1&backend=all").Body.String()
	if !strings.Contains(body, `<details class="advanced" id="advanced" >`) || !strings.Contains(body, "(3 active)") {
		t.Errorf("active filters are indicated but the panel is not forced open")
	}
	// A rejected search opens it so the problem can be fixed.
	bad := get(h, "/?prefix=bad&ovs=valid&backend=all").Body.String()
	if !strings.Contains(bad, `id="advanced" open`) {
		t.Error("invalid search should open Advanced filters")
	}
	// The syntax help moved into the panel.
	if !strings.Contains(body, "CIDR searches match the exact prefix by default") || !strings.Contains(body, "no OR lists") {
		t.Error("search help must remain available in Advanced filters")
	}
}

func TestQueryContextIsInTheSearchPanel(t *testing.T) {
	h := m481Handler(t)
	// Simple search: the box shows the value, the context adds Loc-RIB but never repeats it.
	body := get(h, "/?type=prefix&q=10.0.0.0/24&backend=all").Body.String()
	ctx := regexp.MustCompile(`(?s)<span id="query-context".*?</details></span>`).FindString(body)
	if strings.Count(body, `id="query-context"`) != 1 || ctx == "" || !strings.Contains(ctx, "Loc-RIB") || strings.Contains(ctx, `class="chip"`) {
		t.Errorf("simple search context: %q", ctx)
	}
	if !strings.Contains(body, `value="10.0.0.0/24"`) {
		t.Error("search box lost the value")
	}
	// Compound search: advanced filters appear as chips; the one in the box does not.
	body = get(h, "/?prefix=10.0.0.0/24&ovs=valid&best=1&backend=all").Body.String()
	for _, want := range []string{`<b>prefix</b>=10.0.0.0/24`, `<b>ovs</b>=valid`, `<b>best</b>=1`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing chip %s", want)
		}
	}
	body = get(h, "/?type=prefix&q=10.0.0.0/24&all=1&backend=all").Body.String()
	if strings.Contains(body, `<b>prefix</b>`) || !strings.Contains(body, `<b>all</b>=1`) {
		t.Error("only filters not shown in the box belong in the context")
	}
	// Explanations live in the help popover, not in a results panel.
	for _, want := range []string{`<details class="help">`, "Live query sent to each backend as", "They are not snapshots."} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "meta-note") || strings.Contains(body, `class="eyebrow">QUERY`) {
		t.Error("the query summary panel must be gone")
	}
	// No context without a search, nor for an invalid one.
	for _, target := range []string{"/", "/?type=prefix&q=bad"} {
		b := get(h, target).Body.String()
		if strings.Contains(b, `class="tag"`) || !strings.Contains(b, `<span id="query-context" class="ctx"></span>`) {
			t.Errorf("%s: context must be empty", target)
		}
	}
}

// The context is outside #results, so a fragment replaces it out of band.
func TestFragmentReplacesContextOutOfBand(t *testing.T) {
	h := m481Handler(t)
	frag := get(h, "/?prefix=10.0.0.0/24&ovs=valid&backend=all", "HX-Request", "true").Body.String()
	if strings.Count(frag, `<span id="query-context" class="ctx" hx-swap-oob="true">`) != 1 || strings.Count(frag, `hx-swap-oob`) != 2 {
		t.Errorf("fragment must carry exactly the context and timing OOB elements: %d", strings.Count(frag, "hx-swap-oob"))
	}
	if !strings.Contains(frag, `<b>ovs</b>=valid`) || strings.Contains(frag, "<form") || strings.Contains(frag, "<footer") {
		t.Error("fragment content")
	}
	// An invalid search clears it.
	bad := get(h, "/?prefix=bad", "HX-Request", "true").Body.String()
	if !strings.Contains(bad, `<span id="query-context" class="ctx" hx-swap-oob="true"></span>`) {
		t.Error("stale context must be cleared")
	}
	// Full pages have no OOB markers at all.
	if strings.Contains(get(h, "/?prefix=10.0.0.0/24&backend=all").Body.String(), "hx-swap-oob") {
		t.Error("full page must not carry OOB attributes")
	}
}

func TestSingleBackendStatusLineAboveTable(t *testing.T) {
	h := m481Handler(t)
	body := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a").Body.String()
	if strings.Count(body, `class="backend-line"`) != 1 || strings.Count(body, "<table>") != 1 || strings.Count(body, "COMPLETE") != 1 {
		t.Fatalf("one status line, one table: %d %d %d", strings.Count(body, `class="backend-line"`), strings.Count(body, "<table>"), strings.Count(body, "COMPLETE"))
	}
	line := regexp.MustCompile(`(?s)<h2 class="backend-line">.*?</h2>`).FindString(body)
	for _, want := range []string{"COMPLETE", "Alpha", "Test", "3 routes", " · "} {
		if !strings.Contains(line, want) {
			t.Errorf("status line lacks %q: %s", want, line)
		}
	}
	if !regexp.MustCompile(`· \d+\.\ds`).MatchString(line) || !regexp.MustCompile(`· [\d.]+ (B|KiB|MiB)`).MatchString(line) {
		t.Errorf("query duration and response size missing: %s", line)
	}
	if strings.Index(body, `class="backend-line"`) > strings.Index(body, "<table>") {
		t.Error("status must precede the table")
	}
	for _, gone := range []string{`class="eyebrow">BACKEND`, `class="count"`, `class="group-head"`, "Showing routes", "routes decoded", `class="pager"`} {
		if strings.Contains(body, gone) {
			t.Errorf("redundant element %q still rendered for a one-page result", gone)
		}
	}
	// Query duration is not page-generation time: they are different elements.
	if timingCount(body) != 1 || strings.Contains(line, "Generated in") {
		t.Error("timing must stay in the footer only")
	}
}

func TestSingularRouteCount(t *testing.T) {
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(ribJSON(1, false))})
	if body := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a").Body.String(); !strings.Contains(body, " · 1 route ·") {
		t.Error("one route must not read '1 routes'")
	}
}

func TestMultiBackendRowsStaySeparateAndFailuresDoNotHideSuccess(t *testing.T) {
	lg := defaults()
	lg.QueryTimeout = 400 * time.Millisecond
	cfg, closeAll := testConfig(lg,
		backendSpec{"a", "Alpha", serveDoc(ribJSON(3, false))},
		backendSpec{"b", "Beta-with-an-extremely-long-name-that-must-not-break-the-layout-at-all-ever", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }},
		backendSpec{"c", "Gamma", partialBackend(2)},
		backendSpec{"d", "Delta", serveDoc(`{"rib":[]}`)},
	)
	t.Cleanup(closeAll)
	hh, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hh.Close)
	h := hh.Routes()
	body := get(h, "/?type=prefix&q=10.0.0.0/24&backend=all").Body.String()
	if strings.Count(body, `class="backend-line"`) != 4 {
		t.Fatalf("one status line per backend, got %d", strings.Count(body, `class="backend-line"`))
	}
	lines := regexp.MustCompile(`(?s)<h2 class="backend-line">.*?</h2>`).FindAllString(body, -1)
	// Every backend has its own line whose text (not just colour) names its outcome.
	for name, want := range map[string]string{">Alpha<": "COMPLETE", ">Beta-with": "FAILED", ">Gamma<": "PARTIAL", ">Delta<": "COMPLETE"} {
		found := false
		for _, l := range lines {
			if strings.Contains(l, name) {
				found = strings.Contains(l, want)
			}
		}
		if !found {
			t.Errorf("%s: want status text %q on its own line", name, want)
		}
	}
	// Success is shown, the failure is explained in place, the partial keeps its routes, the empty one says so.
	for _, want := range []string{`Query failed`, `Partial results`, `No matching routes`, "Incomplete results", "backends completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if got := strings.Count(body, "<table>"); got != 2 {
		t.Errorf("tables: %d, want 2 (Alpha, partial Gamma); backends are never merged", got)
	}
	// Long names are bounded by CSS but remain complete in text and tooltip.
	if !strings.Contains(body, `title="Beta-with-an-extremely-long-name-that-must-not-break-the-layout-at-all-ever · Test"`) {
		t.Error("long backend name must stay available as a tooltip")
	}
	// Each table follows its own backend line (Alpha, Beta, Delta, Gamma are listed by name).
	ia, ib := strings.Index(body, `<strong class="bname" title="Alpha`), strings.Index(body, `<strong class="bname" title="Beta-with`)
	if ia < 0 || ib < ia || strings.Count(body[ia:ib], "<table>") != 1 {
		t.Error("each table must follow its own backend line")
	}
}

func TestEmptyResultIsOneCompactLine(t *testing.T) {
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(`{"rib":[]}`)})
	body := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a").Body.String()
	if !strings.Contains(body, `<div class="empty"><strong>No matching routes</strong>`) || strings.Contains(body, "<table>") || strings.Contains(body, "0 routes decoded") {
		t.Error("empty result")
	}
	if !strings.Contains(body, "COMPLETE") || !strings.Contains(body, " · 0 routes ·") {
		t.Error("an empty completed result still says COMPLETE with a zero count")
	}
}

func TestPaginationIsOnlyShownWhenThereIsAnotherPage(t *testing.T) {
	h := m48Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(ribJSON(150, false))})
	one := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a&page=1").Body.String()
	if strings.Count(one, `class="pager"`) != 2 || !strings.Contains(one, "Page 1 of 2") || !strings.Contains(one, `rel="next"`) || strings.Contains(one, `rel="prev"`) {
		t.Errorf("multi-page: pagers=%d", strings.Count(one, `class="pager"`))
	}
	if !strings.Contains(one, "showing 1–100") || !strings.Contains(one, "150 routes") {
		t.Error("page range and total must be visible on a multi-page result")
	}
	two := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a&page=2").Body.String()
	if !strings.Contains(two, `rel="prev"`) || strings.Contains(two, `rel="next"`) || !strings.Contains(two, "showing 101–150") || !strings.Contains(two, `aria-label="Result pages"`) {
		t.Error("page 2 navigation")
	}
	if strings.Count(two, "<tbody>") != 1 || strings.Count(two, "<tr><td><a href") != 50 {
		t.Error("page size semantics changed")
	}
	// A short result has no pager and no duplicated count.
	short := m48Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(ribJSON(3, false))})
	if b := get(short, "/?type=prefix&q=10.0.0.0/24&backend=a").Body.String(); strings.Contains(b, `class="pager"`) || strings.Contains(b, "Page 1 of 1") || strings.Count(b, "3 routes") != 1 {
		t.Errorf("single page: pager=%d count=%d", strings.Count(b, `class="pager"`), strings.Count(b, "3 routes"))
	}
	// Out of range still explains itself and offers the way back.
	if b := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a&page=9").Body.String(); !strings.Contains(b, "Page 9 does not exist") || !strings.Contains(b, `class="pager"`) {
		t.Error("out-of-range page")
	}
}

func TestTextViewKeepsStatusAndActions(t *testing.T) {
	h := m481Handler(t)
	body := get(h, "/?type=prefix&q=10.0.0.0/24&backend=all&view=detail").Body.String()
	for _, want := range []string{`<ul class="status-list">`, `class="toolbar"`, `data-copy="route-text"`, `href="/?type=prefix&amp;q=10.0.0.0%2F24&amp;backend=all&amp;view=detail&amp;output=plain"`, `id="route-text"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(body, `class="pill good">COMPLETE`) != 2 {
		t.Error("one status line per backend in the text view")
	}
	if table := get(h, "/?type=prefix&q=10.0.0.0/24&backend=all").Body.String(); strings.Contains(table, "Plain text (all routes)") {
		t.Error("the plain-text link belongs to the text views")
	}
}

func TestFooterTimingSurvivesTheNewLayout(t *testing.T) {
	h := m481Handler(t)
	for _, target := range []string{"/", "/?type=prefix&q=10.0.0.0/24&backend=all", "/?type=prefix&q=bad", "/?type=prefix&q=10.0.0.0/24&backend=all&view=text"} {
		body := get(h, target).Body.String()
		if timingCount(body) != 1 || strings.Count(body, "<footer>") != 1 || strings.Index(body, "<footer>") > strings.Index(body, `id="page-timing"`) {
			t.Errorf("%s: footer timing must appear once, inside the footer", target)
		}
		frag := get(h, target, "HX-Request", "true").Body.String()
		if timingCount(frag) > 1 || strings.Contains(frag, "<footer") {
			t.Errorf("%s: fragment timing", target)
		}
		if target != "/" && timingCount(frag) != 1 {
			t.Errorf("%s: fragment must carry its timing", target)
		}
	}
	// Fragment layout: title first, then content, then the two out-of-band updates.
	frag := get(h, "/?type=prefix&q=10.0.0.0/24&backend=all", "HX-Request", "true").Body.String()
	iT, iC, iM := strings.Index(frag, "<title>"), strings.Index(frag, `id="query-context"`), strings.Index(frag, `id="page-timing"`)
	if iT != 0 || !(iT < iC && iC < iM) {
		t.Errorf("fragment order title=%d context=%d timing=%d", iT, iC, iM)
	}
	// Disabled: no footer indicator anywhere, titles unaffected.
	off := m48Handler(t, func(c *config.Config) { c.Features.PageTiming = b3(false) }, backendSpec{"a", "Alpha", serveDoc(ribJSON(1, false))})
	for _, hx := range []bool{false, true} {
		rr := get(off, "/?type=prefix&q=10.0.0.0/24&backend=a")
		if hx {
			rr = get(off, "/?type=prefix&q=10.0.0.0/24&backend=a", "HX-Request", "true")
		}
		if timingCount(rr.Body.String()) != 0 || titleOf(t, rr.Body.String()) != "10.0.0.0/24 @ Alpha · Test LG" {
			t.Errorf("hx=%v: disabled timing", hx)
		}
	}
}

// The unchanged parts of a result stay unchanged.
func TestRouteTablePreserved(t *testing.T) {
	h := m481Handler(t)
	body := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a").Body.String()
	for _, want := range []string{"<th>Prefix</th><th>AS path</th><th>Origin ASN</th><th>Next hop</th><th>Neighbor</th><th>Local pref</th><th>MED</th><th>Age</th><th>RPKI</th><th>ASPA</th>", "BEST", "Route details", "Raw OpenBGPD JSON", "395089%3A0%3A8", `<caption class="sr-only">Routes from Alpha</caption>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if fmt.Sprint(strings.Count(body, "<tr><td><a href")) != "3" {
		t.Error("row count")
	}
}
