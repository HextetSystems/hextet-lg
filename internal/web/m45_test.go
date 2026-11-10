package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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
