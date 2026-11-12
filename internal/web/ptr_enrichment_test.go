package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
	"github.com/HextetSystems/hextet-lg/internal/ptr"
	"github.com/HextetSystems/hextet-lg/internal/visibility"
)

type mockPTR struct {
	called  chan string
	release <-chan struct{}
}

func (m mockPTR) LookupAddr(ctx context.Context, s string) ([]string, error) {
	m.called <- s
	select {
	case <-m.release:
		return []string{"Router.Example."}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestPTRPageVisiblePriorityAndCache(t *testing.T) {
	lg := defaults()
	lg.PageSize = 2
	doc := `{"rib":[{"prefix":"192.0.2.0/24","true_nexthop":"172.18.0.1","exit_nexthop":"10.0.0.1"},{"prefix":"2001:db8::/32","true_nexthop":"172.18.0.2","exit_nexthop":"192.0.2.1"},{"prefix":"203.0.113.0/24","true_nexthop":"192.0.2.99","exit_nexthop":"192.0.2.99"}]}`
	cfg, closeAll := testConfig(lg, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("prefix") != "8.8.8.0/24" {
			t.Error("upstream query changed")
		}
		_, _ = w.Write([]byte(doc))
	}})
	defer closeAll()
	cfg.PTREnrichment = config.PTRDefaults()
	cfg.PTREnrichment.Enabled = true
	cfg.PTREnrichment.MaxConcurrent = 1
	cfg.PTREnrichment.MaxLookupsPerPage = 2
	release := make(chan struct{})
	calls := make(chan string, 10)
	h, err := NewHandler(cfg, WithPTRResolver(mockPTR{calls, release}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	routes := h.Routes()
	done := make(chan string, 1)
	url := "/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table"
	go func() { done <- get(routes, url).Body.String() }()
	var body string
	select {
	case body = <-done:
	case <-time.After(time.Second):
		t.Fatal("DNS blocked rendering")
	}
	for _, want := range []string{`data-ptr="10.0.0.1"`, `data-ptr="192.0.2.1"`} {
		if !strings.Contains(body, want) {
			t.Fatal(want)
		}
	}
	if strings.Contains(body, "172.18.0.") {
		t.Fatal("true next-hop address leaked into the default page")
	}
	if strings.Contains(body, `data-ptr="192.0.2.99"`) {
		t.Fatal("off-page route rendered")
	}
	close(release)
	for _, want := range []string{"10.0.0.1", "192.0.2.1"} {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("lookup missing")
		}
	}
	select {
	case got := <-calls:
		t.Fatal("budget exceeded", got)
	default:
	}
	until := time.Now().Add(time.Second)
	for h.ptrTitle("192.0.2.1") == "" {
		if time.Now().After(until) {
			t.Fatal("cache missing")
		}
		time.Sleep(time.Millisecond)
	}
	body = get(routes, url, "HX-Request", "true").Body.String()
	if !strings.Contains(body, `title="Router.Example"`) {
		t.Fatal("cached tooltip missing")
	}
	rr := get(routes, "/ptr-status?ip=10.0.0.1,192.0.2.1")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Router.Example") {
		t.Fatal(rr.Code, rr.Body.String())
	}
	for _, query := range []string{"example.org", "192.0.2.1%2F24", "192.0.2.1:53"} {
		if get(routes, "/ptr-status?ip="+query).Code != 400 {
			t.Fatal("invalid accepted", query)
		}
	}
}

// TestPTRVisibilityIntegration exercises the decoder's visibility policy, page
// construction, scheduling, cache and public status endpoint together.
func TestPTRVisibilityIntegration(t *testing.T) {
	const doc = `{"rib":[
		{"prefix":"10.0.0.0/8","exit_nexthop":"192.0.2.101"},
		{"prefix":"172.16.1.0/24","exit_nexthop":"192.0.2.102"},
		{"prefix":"fd00:1::/48","exit_nexthop":"2001:db8::101"},
		{"prefix":"2001:db8:dead::/48","exit_nexthop":"2001:db8::102"},
		{"prefix":"10.1.0.0/16","exit_nexthop":"192.0.2.1"},
		{"prefix":"192.0.2.0/24","exit_nexthop":"192.0.2.1"},
		{"prefix":"2001:db8:abcd::/48","exit_nexthop":"2001:db8::1"},
		{"prefix":"172.16.2.0/24","exit_nexthop":"192.0.2.1"}
	]}`
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", serveDoc(doc)})
	defer closeAll()
	cfg.RouteVisibility.Exclude.IPv4 = []visibility.Rule{
		{Prefix: "10.0.0.0/8", Match: "exact"},
		{Prefix: "172.16.0.0/12", Match: "or-longer"},
	}
	cfg.RouteVisibility.Exclude.IPv6 = []visibility.Rule{
		{Prefix: "fd00::/8", Match: "or-longer"},
		{Prefix: "2001:db8:dead::/48", Match: "exact"},
	}
	cfg.PTREnrichment = config.PTRDefaults()
	cfg.PTREnrichment.Enabled = true
	cfg.PTREnrichment.MaxConcurrent = 1
	cfg.PTREnrichment.MaxLookupsPerPage = 2
	release := make(chan struct{})
	calls := make(chan string, 16)
	h, err := NewHandler(cfg, WithPTRResolver(mockPTR{calls, release}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	routes := h.Routes()
	rr := get(routes, "/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table")
	if rr.Code != http.StatusOK {
		t.Fatalf("search status: %d", rr.Code)
	}
	body := rr.Body.String()
	for _, visible := range []string{"10.1.0.0/16", "2001:db8:abcd::/48", "192.0.2.0/24"} {
		if !strings.Contains(body, visible) {
			t.Errorf("visible route %s absent", visible)
		}
	}
	for _, hidden := range []string{"10.0.0.0/8", "172.16.1.0/24", "fd00:1::/48", "2001:db8:dead::/48", "172.16.2.0/24"} {
		if strings.Contains(body, hidden) {
			t.Errorf("excluded route %s rendered", hidden)
		}
	}
	close(release)
	for _, want := range []string{"192.0.2.1", "2001:db8::1"} {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("resolver call %q, want %q; excluded routes must not consume the visible-page budget", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("visible next-hop lookup not scheduled")
		}
	}
	deadline := time.Now().Add(time.Second)
	for h.ptrTitle("192.0.2.1") == "" || h.ptrTitle("2001:db8::1") == "" {
		if time.Now().After(deadline) {
			t.Fatal("visible next-hop results were not cached")
		}
		time.Sleep(time.Millisecond)
	}
	hiddenIPs := []string{"192.0.2.101", "192.0.2.102", "2001:db8::101", "2001:db8::102"}
	for _, ip := range hiddenIPs {
		addr, ok := ptr.Address(ip)
		if !ok {
			t.Fatalf("invalid test address %q", ip)
		}
		if _, ok := h.ptr.Get(addr); ok {
			t.Errorf("hidden-only address %s entered PTR cache", ip)
		}
	}
	status := get(routes, "/ptr-status?ip=192.0.2.1,2001%3Adb8%3A%3A1,192.0.2.101,192.0.2.102,2001%3Adb8%3A%3A101,2001%3Adb8%3A%3A102")
	var got map[string]string
	if status.Code != http.StatusOK || json.Unmarshal(status.Body.Bytes(), &got) != nil {
		t.Fatalf("PTR status: %d %s", status.Code, status.Body.String())
	}
	if len(got) != 2 || got["192.0.2.1"] != "Router.Example" || got["2001:db8::1"] != "Router.Example" {
		t.Fatalf("status must contain only visible next hops: %v", got)
	}
	select {
	case ip := <-calls:
		t.Fatalf("unexpected lookup for %s", ip)
	default:
	}
}
