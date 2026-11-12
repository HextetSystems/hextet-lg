package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

type mockASNResolver struct {
	release <-chan struct{}
	called  chan string
}

func (m mockASNResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	select {
	case m.called <- name:
	default:
	}
	select {
	case <-m.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var n uint32
	fmt.Sscanf(name, "AS%d.", &n)
	return []string{fmt.Sprintf("%d | US | arin | 2010-07-14 | Name <unsafe>", n)}, nil
}
func TestASNPageNonblockingAndStatus(t *testing.T) {
	release := make(chan struct{})
	calls := make(chan string, 10)
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/neighbors" {
			fmt.Fprint(w, `{"neighbors":[{"remote_as":"15169","remote_addr":"192.0.2.1"}]}`)
			return
		}
		if r.URL.Path == "/summary" {
			fmt.Fprint(w, `{"neighbors":[{"remote_as":"15169","remote_addr":"192.0.2.1"}]}`)
			return
		}
		if r.URL.Query().Get("prefix") != "8.8.8.0/24" {
			t.Errorf("upstream query changed: %v", r.URL.Query())
		}
		fmt.Fprint(w, `{"rib":[{"prefix":"8.8.8.0/24","aspath":"174 {15169,13335}"}]}`)
	}})
	defer closeAll()
	cfg.ASNEnrichment = config.ASNDefaults()
	cfg.ASNEnrichment.Enabled = true
	cfg.ASNEnrichment.MaxLookupsPerPage = 1
	h, err := NewHandler(cfg, WithASNResolver(mockASNResolver{release, calls}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	routes := h.Routes()
	result := make(chan string, 1)
	go func() { result <- get(routes, "/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table").Body.String() }()
	var body string
	select {
	case body = <-result:
	case <-time.After(time.Second):
		t.Fatal("page blocked on DNS")
	}
	if !strings.Contains(body, `data-asn="13335"`) || !strings.Contains(body, `>AS13335</a>`) {
		t.Fatal("missing origin")
	}
	select {
	case call := <-calls:
		if call != "AS13335.asn.cymru.com" {
			t.Fatalf("priority: %s", call)
		}
	case <-time.After(time.Second):
		t.Fatal("no lookup")
	}
	select {
	case call := <-calls:
		t.Fatalf("budget exceeded %s", call)
	default:
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		if h.asnTitle("13335") != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no cached answer")
		}
		time.Sleep(time.Millisecond)
	}
	rr := get(routes, "/asn-status?asn=13335")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `Name \u003cunsafe\u003e`) {
		t.Fatalf("status: %d %s", rr.Code, rr.Body.String())
	}
	body = get(routes, "/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table", "HX-Request", "true").Body.String()
	if !strings.Contains(body, `title="Name &lt;unsafe&gt;"`) {
		t.Fatal("cached title not escaped")
	}
	if get(routes, "/asn-status?asn=1,0").Code != 400 {
		t.Fatal("invalid status accepted")
	}
	body = get(routes, "/neighbors?backend=a").Body.String()
	if !strings.Contains(body, `data-asn="15169"`) {
		t.Fatal("neighbor ASN missing")
	}
	deadline = time.Now().Add(time.Second)
	for h.asnTitle("15169") == "" {
		if time.Now().After(deadline) {
			t.Fatal("neighbor lookup missing")
		}
		time.Sleep(time.Millisecond)
	}
	body = get(routes, "/neighbors?backend=a").Body.String()
	if !strings.Contains(body, `title="Name &lt;unsafe&gt;"`) {
		t.Fatal("cached neighbor title missing or unescaped")
	}
}

func TestASNPathRendering(t *testing.T) {
	release := make(chan struct{})
	close(release)
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", serveDoc(`{"rib":[{"prefix":"8.8.8.0/24","aspath":"174 {15169,13335} (15169 174) [13335,64512]"}]}`)})
	defer closeAll()
	cfg.ASNEnrichment = config.ASNDefaults()
	cfg.ASNEnrichment.Enabled = true
	h, err := NewHandler(cfg, WithASNResolver(mockASNResolver{release, make(chan string, 50)}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, tc := range []struct {
		path   string
		tokens []string
	}{
		{"13335", []string{"13335"}}, {"174 15169 13335", []string{"174", "15169", "13335"}}, {"13335 13335", []string{"13335", "13335"}},
		{"{174,15169}", []string{"174", "15169"}}, {"(174 15169)", []string{"174", "15169"}}, {"[174,15169]", []string{"174", "15169"}},
		{"64512 13335 0", []string{"13335"}}, {"bad13335 1.2 -174 4294967296", nil}, {"", nil},
	} {
		parts := h.pathParts(tc.path)
		var text strings.Builder
		var tokens []string
		for _, p := range parts {
			text.WriteString(p.Text)
			if p.ASN != "" {
				tokens = append(tokens, p.ASN)
			}
		}
		want := tc.path
		if want == "" {
			want = "—"
		}
		if text.String() != want || strings.Join(tokens, ",") != strings.Join(tc.tokens, ",") {
			t.Errorf("%q: text %q tokens %v", tc.path, text.String(), tokens)
		}
	}
	var escaped strings.Builder
	tmpl := template.Must(template.New("path").Parse(`{{range .}}{{.Text}}{{end}}`))
	if err := tmpl.Execute(&escaped, h.pathParts(`<script>alert(13335)</script>`)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(escaped.String(), "<script>") || !strings.Contains(escaped.String(), "&lt;script&gt;") {
		t.Fatal("path markup was not escaped")
	}
	routes := h.Routes()
	url := "/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table"
	uncached := get(routes, url).Body.String()
	if !strings.Contains(uncached, `href="/?type=asn&amp;q=64512&amp;backend=a"`) || !strings.Contains(uncached, `data-asn="15169"`) {
		t.Fatal("uncached ASN links or path missing")
	}
	until := time.Now().Add(time.Second)
	for h.asnTitle("13335") == "" {
		if time.Now().After(until) {
			t.Fatal("lookup timed out")
		}
		time.Sleep(time.Millisecond)
	}
	cached := get(routes, url).Body.String()
	if !strings.Contains(cached, `title="Name &lt;unsafe&gt;"`) || !strings.Contains(cached, `[<span data-asn="13335"`) || !strings.Contains(cached, `,64512]`) {
		t.Fatal("cached title, escaping or path delimiters lost")
	}
}

type outageResolver struct {
	calls  atomic.Int32
	active atomic.Int32
	peak   atomic.Int32
}

func (o *outageResolver) LookupTXT(ctx context.Context, _ string) ([]string, error) {
	o.calls.Add(1)
	v := o.active.Add(1)
	defer o.active.Add(-1)
	for {
		p := o.peak.Load()
		if v <= p || o.peak.CompareAndSwap(p, v) {
			break
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestASNOutageRoutesAndNeighbors(t *testing.T) {
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/neighbors" {
			fmt.Fprint(w, `{"neighbors":[{"remote_as":"15169","remote_addr":"192.0.2.1"}]}`)
			return
		}
		fmt.Fprint(w, `{"rib":[{"prefix":"8.8.8.0/24","aspath":"174 13335"}]}`)
	}})
	defer closeAll()
	cfg.ASNEnrichment = config.ASNDefaults()
	cfg.ASNEnrichment.Enabled = true
	cfg.ASNEnrichment.Timeout = 30 * time.Millisecond
	cfg.ASNEnrichment.MaxConcurrent = 1
	cfg.ASNEnrichment.Cache.Persist = true
	cfg.ASNEnrichment.Cache.Path = filepath.Join(t.TempDir(), "missing", "cache.json")
	resolver := &outageResolver{}
	h, err := NewHandler(cfg, WithASNResolver(resolver))
	if err != nil {
		t.Fatal(err)
	}
	routes := h.Routes()
	start := time.Now()
	for _, url := range []string{"/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table", "/neighbors?backend=a"} {
		rr := get(routes, url)
		if rr.Code != 200 {
			t.Fatalf("outage page %d", rr.Code)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("DNS blocked rendering")
	}
	time.Sleep(120 * time.Millisecond)
	calls := resolver.calls.Load()
	for i := 0; i < 10; i++ {
		get(routes, "/neighbors?backend=a")
	}
	time.Sleep(50 * time.Millisecond)
	if resolver.calls.Load() != calls {
		t.Fatal("retry storm")
	}
	if resolver.peak.Load() > 1 {
		t.Fatal("concurrency limit")
	}
	done := make(chan struct{})
	go func() { h.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown hung")
	}
}

type changingResolver struct{ offline atomic.Bool }

func (r *changingResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	if r.offline.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	var n uint32
	fmt.Sscanf(name, "AS%d.", &n)
	return []string{fmt.Sprintf("%d | US | arin | 2010-07-14 | Cached name", n)}, nil
}
func TestASNWarmCacheDuringOutage(t *testing.T) {
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/neighbors" {
			fmt.Fprint(w, `{"neighbors":[{"remote_as":"13335","remote_addr":"192.0.2.1"},{"remote_as":"15169","remote_addr":"192.0.2.2"}]}`)
			return
		}
		fmt.Fprint(w, `{"rib":[{"prefix":"8.8.8.0/24","aspath":"174 13335"}]}`)
	}})
	defer closeAll()
	cfg.ASNEnrichment = config.ASNDefaults()
	cfg.ASNEnrichment.Enabled = true
	cfg.ASNEnrichment.Timeout = 20 * time.Millisecond
	cfg.ASNEnrichment.Cache.Persist = true
	cfg.ASNEnrichment.Cache.Path = filepath.Join(t.TempDir(), "missing", "cache.json")
	resolver := &changingResolver{}
	h, err := NewHandler(cfg, WithASNResolver(resolver))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.asn.Schedule(13335)
	deadline := time.Now().Add(time.Second)
	for h.asnTitle("13335") == "" {
		if time.Now().After(deadline) {
			t.Fatal("warm lookup failed")
		}
		time.Sleep(time.Millisecond)
	}
	resolver.offline.Store(true)
	for _, u := range []string{"/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table", "/neighbors?backend=a"} {
		rr := get(h.Routes(), u)
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), `title="Cached name"`) {
			t.Fatal("warm cached description missing during outage")
		}
	}
}

func TestASNOnlyVisiblePageSchedules(t *testing.T) {
	release := make(chan struct{})
	calls := make(chan string, 10)
	lg := defaults()
	lg.PageSize = 1
	cfg, closeAll := testConfig(lg, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "prefix=8.8.8.0%2F24" {
			t.Errorf("upstream changed: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"rib":[{"prefix":"8.8.8.0/24","aspath":"13335"},{"prefix":"2001:db8::/32","aspath":"15169"}]}`)
	}})
	defer closeAll()
	cfg.ASNEnrichment = config.ASNDefaults()
	cfg.ASNEnrichment.Enabled = true
	cfg.ASNEnrichment.MaxConcurrent = 1
	h, err := NewHandler(cfg, WithASNResolver(mockASNResolver{release, calls}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rr := get(h.Routes(), "/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table")
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	select {
	case name := <-calls:
		if name != "AS13335.asn.cymru.com" {
			t.Fatal(name)
		}
	case <-time.After(time.Second):
		t.Fatal("missing lookup")
	}
	select {
	case name := <-calls:
		t.Fatalf("off-page lookup %s", name)
	default:
	}
}
