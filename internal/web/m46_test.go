package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
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
	doc := `{"rib":[{"prefix":"192.0.2.0/24","true_nexthop":"2001:db8::1","exit_nexthop":"10.0.0.1"},{"prefix":"2001:db8::/32","true_nexthop":"???","exit_nexthop":"192.0.2.1"},{"prefix":"203.0.113.0/24","true_nexthop":"192.0.2.99"}]}`
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
	for _, want := range []string{`data-ptr="2001:db8::1"`, `data-ptr="192.0.2.1"`, `data-ptr="10.0.0.1"`} {
		if !strings.Contains(body, want) {
			t.Fatal(want)
		}
	}
	if strings.Contains(body, `data-ptr="192.0.2.99"`) {
		t.Fatal("off-page route rendered")
	}
	close(release)
	for _, want := range []string{"2001:db8::1", "192.0.2.1"} {
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
	rr := get(routes, "/ptr-status?ip=2001%3Adb8%3A%3A1,192.0.2.1")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Router.Example") {
		t.Fatal(rr.Code, rr.Body.String())
	}
	for _, query := range []string{"example.org", "192.0.2.1%2F24", "192.0.2.1:53"} {
		if get(routes, "/ptr-status?ip="+query).Code != 400 {
			t.Fatal("invalid accepted", query)
		}
	}
}
