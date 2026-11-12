package web

import (
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
	"github.com/HextetSystems/hextet-lg/internal/obs"
	"github.com/HextetSystems/hextet-lg/internal/visibility"
)

// m44Files are synthetic definitions (documentation ASN 64500 and the private
// range): nothing here is a real operator's policy.
var m44Files = map[string]string{
	"as64500.txt": `# synthetic
64500:100,Learned from customer
64500:200,Learned from peer
64500:4xx,Regional policy $0 & "quotes" <b>bold</b>
64500:1:100,Example large community
64500:1:nnn,Do not announce to AS$0
rt 64500:1,Example route target
soo 64500:nnn,Site of origin $0
`,
	"as64501.txt":    "64501:7,Second operator seven\n64501:1:2,Second operator large\n",
	"well-known.txt": "65535:65281,NO_EXPORT: do not export\n",
}

const m44Route = `{"rib":[{"prefix":"192.0.2.0/24","aspath":"64500","best":true,"neighbor":{"description":"peer-a","remote_addr":"192.0.2.1"},` +
	`"communities":["64500:100","NO_EXPORT","64500:999","64501:7","64500:412"],` +
	`"extended_communities":["rt 64500:1","soo 64500:42","rt 64500:2"],` +
	`"large_communities":["64500:1:100","64500:1:4200000000","64500:2:2","64501:1:2"]}]}`

func writeDefs(t testing.TB, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newDefsHandler(t testing.TB, files map[string]string, mutate func(*config.Config), specs ...backendSpec) http.Handler {
	t.Helper()
	cfg, closeAll := testConfig(defaults(), specs...)
	t.Cleanup(closeAll)
	if files != nil {
		cfg.CommunityDefinitions = config.CommunityDefinitions{Enabled: true, Directory: writeDefs(t, files)}
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h.Routes()
}

func routeBackend(doc string) backendSpec { return backendSpec{"a", "Alpha", serveDoc(doc)} }

const routeQuery = "/?type=prefix&q=192.0.2.0%2F24&backend=a"

func TestM44RouteTooltips(t *testing.T) {
	h := newDefsHandler(t, m44Files, nil, routeBackend(m44Route))
	rr := get(h, routeQuery)
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		// standard, including a well-known name OpenBGPD prints, a second ASN's
		// definition, and a wildcard with a substitution
		`<span class="known" title="Learned from customer">64500:100</span>`,
		`<span class="known" title="NO_EXPORT: do not export">NO_EXPORT</span>`,
		`<span class="known" title="Second operator seven">64501:7</span>`,
		// extended
		`<span class="known" title="Example route target">rt 64500:1</span>`,
		`<span class="known" title="Site of origin 42">soo 64500:42</span>`,
		// large (still a search link)
		`<a class="known" title="Example large community" href="/?type=large-community&amp;q=64500%3A1%3A100&amp;backend=a">64500:1:100</a>`,
		`title="Do not announce to AS4200000000"`,
		`title="Second operator large"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Unknown communities keep their raw value and get no title, no class.
	for _, raw := range []string{`<span>64500:999</span>`, `<span>rt 64500:2</span>`, `<a href="/?type=large-community&amp;q=64500%3A2%3A2&amp;backend=a">64500:2:2</a>`} {
		if !strings.Contains(body, raw) {
			t.Errorf("unknown community not rendered plainly: %s", raw)
		}
	}
	// Kinds are not conflated: the large 64500:1:100 text is not a standard hit.
	if strings.Contains(body, `title="Learned from customer">64500:1:100`) {
		t.Error("kinds conflated")
	}
}

func TestM44DescriptionsAreEscapedEverywhere(t *testing.T) {
	h := newDefsHandler(t, m44Files, nil, routeBackend(m44Route))
	body := get(h, routeQuery).Body.String()
	// 64500:412 matches 64500:4xx whose description holds markup, quotes and &.
	want := `title="Regional policy 1 &amp; &#34;quotes&#34; &lt;b&gt;bold&lt;/b&gt;"`
	if !strings.Contains(body, want) {
		t.Fatalf("tooltip not escaped; looking for %s in route detail", want)
	}
	explorer := get(h, "/communities").Body.String()
	for _, bad := range []string{"<b>bold</b>", `"quotes" <b>`} {
		if strings.Contains(explorer, bad) {
			t.Errorf("explorer rendered raw markup %q", bad)
		}
	}
	if !strings.Contains(explorer, "&lt;b&gt;bold&lt;/b&gt;") {
		t.Error("explorer did not show the escaped description")
	}
	// A description can never inject an attribute.
	files := map[string]string{"as64500.txt": "64500:1,x\" onmouseover=\"alert(1)\n"}
	h = newDefsHandler(t, files, nil, routeBackend(`{"rib":[{"prefix":"192.0.2.0/24","communities":["64500:1"]}]}`))
	body = get(h, routeQuery).Body.String()
	if strings.Contains(body, `" onmouseover="`) || !strings.Contains(body, `title="x&#34; onmouseover=&#34;alert(1)"`) {
		t.Fatalf("attribute injection: %s", body)
	}
}

func TestM44PlaintextIsUnchanged(t *testing.T) {
	on := newDefsHandler(t, m44Files, nil, routeBackend(m44Route))
	off := newDefsHandler(t, nil, nil, routeBackend(m44Route))
	elapsed := regexp.MustCompile(`\d+\.\ds`)
	for _, view := range []string{"text", "detail"} {
		target := routeQuery + "&view=" + view + "&output=plain"
		a, b := get(on, target), get(off, target)
		if a.Code != 200 || a.Header().Get("Content-Type") != b.Header().Get("Content-Type") {
			t.Fatalf("%s: %d %v", view, a.Code, a.Header())
		}
		got, want := elapsed.ReplaceAllString(a.Body.String(), "Ns"), elapsed.ReplaceAllString(b.Body.String(), "Ns")
		if got != want {
			t.Errorf("%s output differs with definitions enabled:\n%s\n--- vs ---\n%s", view, got, want)
		}
		for _, desc := range []string{"Learned from customer", "Example route target", "NO_EXPORT: do not export"} {
			if strings.Contains(got, desc) {
				t.Errorf("%s: description %q leaked into plaintext", view, desc)
			}
		}
	}
	if !strings.Contains(get(on, routeQuery+"&view=detail&output=plain").Body.String(), "Communities: 64500:100 NO_EXPORT 64500:999") {
		t.Error("raw community values missing from plaintext")
	}
}

// With and without definitions, the only difference in a route page is the
// tooltip markup: neighbor links, IPv4 and IPv6 rows and everything else are
// identical.
func TestM44EnrichmentIsPurelyPresentational(t *testing.T) {
	doc := `{"rib":[` + strings.Join([]string{
		`{"prefix":"192.0.2.0/24","aspath":"64500","best":true,"neighbor":{"description":"peer-a","remote_addr":"192.0.2.1"},"communities":["64500:100"],"extended_communities":["rt 64500:1"],"large_communities":["64500:1:100"]}`,
		`{"prefix":"2001:db8::/32","aspath":"64500 64501","neighbor":{"description":"peer-v6","remote_addr":"2001:db8::1"},"communities":["64500:200"],"large_communities":["64501:1:2"]}`,
	}, ",") + `]}`
	on := newDefsHandler(t, m44Files, nil, routeBackend(doc))
	off := newDefsHandler(t, nil, nil, routeBackend(doc))
	strip := regexp.MustCompile(` class="known" title="[^"]*"`)
	elapsed := regexp.MustCompile(`\d+\.\ds|Generated in [^<]+`) // per-request timings legitimately vary
	for _, target := range []string{"/?type=asn&q=64500&backend=a", "/?as=64500&backend=a&view=detail", "/?type=prefix&q=2001%3Adb8%3A%3A%2F32&backend=a", "/?community=64500%3A100&backend=a"} {
		a, b := get(on, target), get(off, target)
		if a.Code != b.Code || a.Header().Get("Location") != b.Header().Get("Location") {
			t.Fatalf("%s: %d vs %d", target, a.Code, b.Code)
		}
		got := elapsed.ReplaceAllString(strip.ReplaceAllString(a.Body.String(), ""), "Ns")
		want := elapsed.ReplaceAllString(b.Body.String(), "Ns")
		if got != want {
			t.Errorf("%s: markup differs beyond tooltips", target)
		}
	}
	if !strings.Contains(get(on, "/?type=asn&q=64500&backend=a").Body.String(), `title="Second operator large"`) {
		t.Error("IPv6 route communities not enriched")
	}
}

func TestM44DisabledReadsNothing(t *testing.T) {
	// Disabled with a directory that would not load: nothing is read.
	bad := writeDefs(t, map[string]string{"as64500.txt": "this is not valid\n"})
	h := newDefsHandler(t, nil, func(c *config.Config) {
		c.CommunityDefinitions = config.CommunityDefinitions{Enabled: false, Directory: bad}
	}, routeBackend(m44Route))
	body := get(h, routeQuery).Body.String()
	if strings.Contains(body, `class="known"`) || !strings.Contains(body, `<span>64500:100</span>`) {
		t.Fatal("tooltips rendered while disabled")
	}
	page := get(h, "/communities").Body.String()
	if !strings.Contains(page, "No community definitions are configured") || strings.Contains(page, "395089:") {
		t.Fatal("explorer must be empty and neutral when disabled")
	}
}

func TestM44StartupPolicy(t *testing.T) {
	build := func(dir string) error {
		cfg, closeAll := testConfig(defaults(), routeBackend(m44Route))
		defer closeAll()
		cfg.CommunityDefinitions = config.CommunityDefinitions{Enabled: true, Directory: dir}
		h, err := NewHandler(cfg)
		if err == nil {
			h.Close()
		}
		return err
	}
	if err := build(filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("missing directory: %v", err)
	}
	if err := build(writeDefs(t, map[string]string{"as64500.txt": "64500:1,ok\nbroken line\n", "as64501.txt": "64501:1\n"})); err == nil || !strings.Contains(err.Error(), "as64500.txt:2:") || !strings.Contains(err.Error(), "as64501.txt:1:") {
		t.Errorf("malformed definitions must fail startup with file and line: %v", err)
	}
	if err := build(writeDefs(t, map[string]string{"asbogus.txt": "64500:1,ok\n"})); err == nil {
		t.Error("invalid file name accepted")
	}
	// An empty directory is valid: no definitions, nothing hardcoded.
	empty := t.TempDir()
	if err := build(empty); err != nil {
		t.Fatalf("empty directory: %v", err)
	}
	cfg, closeAll := testConfig(defaults(), routeBackend(`{"rib":[{"prefix":"192.0.2.0/24","large_communities":["395089:0:8"]}]}`))
	defer closeAll()
	cfg.CommunityDefinitions = config.CommunityDefinitions{Enabled: true, Directory: empty}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	body := get(h.Routes(), routeQuery).Body.String()
	if !strings.Contains(body, "395089:0:8") || strings.Contains(body, "Peering routes") || strings.Contains(body, `class="known"`) {
		t.Fatal("hidden fallback to built-in AS395089 definitions")
	}
	if page := get(h.Routes(), "/communities").Body.String(); strings.Contains(page, "395089:") || !strings.Contains(page, "No matching communities.") {
		t.Fatal("explorer must be empty for an empty directory")
	}
}

func TestM44CommunityExplorer(t *testing.T) {
	h := newDefsHandler(t, m44Files, nil, routeBackend(m44Route))
	body := get(h, "/communities?backend=a").Body.String()
	for _, want := range []string{
		"COMMUNITY REFERENCE", "Learned from customer", "Second operator seven", "NO_EXPORT: do not export",
		`href="/?community=64500%3A100&amp;backend=a&amp;view=table"`,
		`href="/?type=large-community&amp;q=64500%3A1%3A100&amp;backend=a"`,
		`href="/?ext-community=rt&#43;64500%3A1&amp;backend=a&amp;view=table"`,
		"<small>pattern</small>", "as64501.txt", "AS64501", "Well-known", "<th>Source</th>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("explorer missing %q", want)
		}
	}
	for _, unwanted := range []string{"Status / evidence", "Implemented", "evidence", "notes"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(unwanted)) {
			t.Errorf("removed metadata remains in Explorer: %q", unwanted)
		}
	}
	// Patterns have no single value to search for.
	if strings.Contains(body, "q=64500%3A1%3Annn") || strings.Contains(body, "community=64500%3A4xx") {
		t.Error("link generated for a pattern")
	}
	// Source filtering and description search use only the loaded text files.
	if b := get(h, "/communities?category=AS64501").Body.String(); !strings.Contains(b, "Second operator seven") || strings.Contains(b, "Learned from customer") {
		t.Error("source filter")
	}
	if b := get(h, "/communities?q=customer").Body.String(); !strings.Contains(b, "Learned from customer") || strings.Contains(b, "Second operator seven") {
		t.Error("description search")
	}
	// Search links honour the M4.2 flags; the Explorer itself stays visible.
	no := false
	h = newDefsHandler(t, m44Files, func(c *config.Config) {
		c.Features = config.Features{Communities: &no, LargeCommunities: &no, ExtendedCommunities: &no}
	}, routeBackend(m44Route))
	rr := get(h, "/communities")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Learned from customer") || strings.Contains(rr.Body.String(), "view=table") || strings.Contains(rr.Body.String(), "type=large-community") {
		t.Fatal("explorer must stay visible without search links when the search types are disabled")
	}
}

func TestM44SearchIsIndependentOfDefinitions(t *testing.T) {
	record := func(files map[string]string) (*[]string, http.Handler) {
		var mu sync.Mutex
		var seen []string
		spec := backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.URL.RequestURI())
			mu.Unlock()
			fmt.Fprint(w, m44Route)
		}}
		return &seen, newDefsHandler(t, files, nil, spec)
	}
	queries := []string{
		"/?type=prefix&q=192.0.2.0%2F24&backend=a",
		"/?community=64500%3A100&backend=a",
		"/?community=64500%3A999&backend=a", // no description: still searchable by raw value
		"/?type=community&q=NO_EXPORT&backend=a",
		"/?type=large-community&q=64500%3A1%3A100&backend=a",
		"/?type=large-community&q=64500%3A2%3A2&backend=a",
		"/?ext-community=rt+64500%3A1&backend=a",
		"/?as=64500&community=64500%3A100&best=1&backend=a",
	}
	seenOn, on := record(m44Files)
	seenOff, off := record(nil)
	for _, q := range queries {
		a, b := get(on, q), get(off, q)
		if a.Code != b.Code || a.Header().Get("Location") != b.Header().Get("Location") {
			t.Errorf("%s: status/redirect differ (%d %q vs %d %q)", q, a.Code, a.Header().Get("Location"), b.Code, b.Header().Get("Location"))
		}
		if a.Code != 200 && a.Code != 303 {
			t.Errorf("%s: %d", q, a.Code)
		}
	}
	if strings.Join(*seenOn, "\n") != strings.Join(*seenOff, "\n") || len(*seenOn) == 0 {
		t.Errorf("upstream queries differ:\n%v\n%v", *seenOn, *seenOff)
	}
	// Descriptions are not searchable: searching for a description is not a
	// community search and is rejected like any other malformed value.
	rr := get(on, "/?type=community&q=Learned+from+customer&backend=a")
	if rr.Code == 200 && len(*seenOn) > len(*seenOff) {
		t.Error("a description reached the backend as a search")
	}
	// M4.2: disabled search types stay disabled with definitions present.
	no := false
	h := newDefsHandler(t, m44Files, func(c *config.Config) {
		c.Features = config.Features{Communities: &no, LargeCommunities: &no, ExtendedCommunities: &no}
	}, routeBackend(m44Route))
	for _, q := range []string{"/?community=64500%3A100&backend=a", "/?type=large-community&q=64500%3A1%3A100&backend=a", "/?ext-community=rt+64500%3A1&backend=a"} {
		if rr := get(h, q); rr.Code != 400 {
			t.Errorf("%s: %d, want 400", q, rr.Code)
		}
	}
	// Enrichment still applies to displayed attributes of other searches.
	if !strings.Contains(get(h, routeQuery).Body.String(), `title="Learned from customer"`) {
		t.Error("feature flags must not strip enrichment")
	}
}

func TestM44VisibilityCacheAndSharedFlights(t *testing.T) {
	var hits atomic.Int32
	doc := `{"rib":[{"prefix":"10.1.0.0/16","communities":["64500:100"]},{"prefix":"192.0.2.0/24","communities":["64500:200"]}]}`
	spec := backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, doc)
	}}
	h := newDefsHandler(t, m44Files, func(c *config.Config) {
		c.LookingGlass.CacheTTL = time.Minute
		c.RouteVisibility.Exclude.IPv4 = []visibility.Rule{{Prefix: "10.0.0.0/8", Match: "or-longer"}}
	}, spec)
	target := "/?type=asn&q=64500&backend=a&view=table"
	var wg sync.WaitGroup
	bodies := make([]string, 8)
	for i := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bodies[i] = get(h, target).Body.String()
		}()
	}
	wg.Wait()
	for i, b := range bodies {
		if strings.Contains(b, "10.1.0.0/16") || strings.Contains(b, "Learned from customer") {
			t.Fatalf("response %d shows an excluded route or its community description", i)
		}
		if !strings.Contains(b, "192.0.2.0/24") || !strings.Contains(b, "Learned from peer") {
			t.Fatalf("response %d lacks the visible route/tooltip", i)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("backend hit %d times: shared flight/cache not preserved", n)
	}
}

func TestM44LargeResultLookup(t *testing.T) {
	const n = 1000
	h := newDefsHandler(t, map[string]string{
		"as64500.txt": "64500:1,Std\nrt 64500:1,Ext\n395089:0:8,Large 8\n395089:5:2,Large 52\n",
	}, func(c *config.Config) { c.LookingGlass.PageSize = n }, backendSpec{"a", "Alpha", serveDoc(ribJSON(n, false))})
	start := time.Now()
	rr := get(h, "/?type=asn&q=64500&backend=a&view=table")
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	if got := strings.Count(rr.Body.String(), `class="known"`); got != 4*n {
		t.Fatalf("%d tooltips, want %d", got, 4*n)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("rendering %d enriched routes took %s", n, d)
	}
}

func TestM44ExampleDefinitionsComeFromTextFile(t *testing.T) {
	data, err := os.ReadFile("../../examples/communities/as395089.txt")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"as395089.txt": string(data)}
	doc := strings.Replace(m44Route, `"large_communities":[`, `"large_communities":["395089:0:8",`, 1)
	cfg, closeAll := testConfig(defaults(), routeBackend(doc))
	t.Cleanup(closeAll)
	cfg.CommunityDefinitions = config.CommunityDefinitions{Enabled: true, Directory: writeDefs(t, files)}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	defs := h.defs.Definitions()
	if len(defs) != 32 {
		t.Fatalf("loaded %d definitions, want 32", len(defs))
	}
	for _, d := range defs {
		if d.Source != "AS395089" || d.File != "as395089.txt" {
			t.Errorf("definition source is not derived from its text file: %+v", d)
		}
	}
	page := get(h.Routes(), "/communities?category=AS395089").Body.String()
	if !strings.Contains(page, "Peering routes") || !strings.Contains(page, "as395089.txt") || strings.Contains(page, "Status / evidence") {
		t.Error("Explorer did not render the text-file definitions as expected")
	}
	if body := get(h.Routes(), routeQuery).Body.String(); !strings.Contains(body, `title="Peering routes" href="/?type=large-community&amp;q=395089%3A0%3A8`) {
		t.Error("text-file definition not used for the route tooltip")
	}
}

func TestM44StartupDiagnostics(t *testing.T) {
	cfg, closeAll := testConfig(defaults(), routeBackend(m44Route))
	defer closeAll()
	dir := writeDefs(t, map[string]string{"as64500.txt": "64500:1,ok\n64500:nnn,wild $0\n", "notes.txt": "x"})
	cfg.CommunityDefinitions = config.CommunityDefinitions{Enabled: true, Directory: dir}
	buf := &syncBuf{}
	h, err := NewHandler(cfg, WithLogger(obs.NewLogger(buf, "info", "json")))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	loaded := find(buf.records(t), "community definitions loaded")
	if loaded == nil || fmt.Sprint(loaded["definitions"]) != "2" || fmt.Sprint(loaded["exact"]) != "1" || fmt.Sprint(loaded["patterns"]) != "1" || fmt.Sprint(loaded["files"]) != "1" {
		t.Fatalf("startup diagnostics: %s", buf.String())
	}
	if find(buf.records(t), "community definition directory contains .txt files that are not asN.txt or well-known.txt and were not loaded") == nil {
		t.Fatalf("ignored files not reported: %s", buf.String())
	}
	// Request handling logs nothing about communities.
	before := buf.String()
	get(h.Routes(), routeQuery)
	if strings.Contains(strings.TrimPrefix(buf.String(), before), "64500") {
		t.Fatal("request log leaked community data")
	}
}

func TestM44ExplorerIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxExplorerRows+500; i++ {
		fmt.Fprintf(&b, "64500:%d,Definition number %d\n", i, i)
	}
	h := newDefsHandler(t, map[string]string{"as64500.txt": b.String()}, nil, routeBackend(m44Route))
	body := get(h, "/communities").Body.String()
	if got := strings.Count(body, "Definition number"); got != maxExplorerRows {
		t.Fatalf("%d rows, want %d", got, maxExplorerRows)
	}
	if !strings.Contains(body, "Showing the first 1,000 of 1,500 definitions") {
		t.Fatal("truncation not announced")
	}
	// Filters reach definitions beyond the cap.
	body = get(h, "/communities?q=64500%3A1499").Body.String()
	if !strings.Contains(body, "Definition number 1499") || strings.Contains(body, "Showing the first") {
		t.Fatal("filter must reach every definition")
	}
}
