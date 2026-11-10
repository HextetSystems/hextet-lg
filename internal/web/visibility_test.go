package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
	"github.com/HextetSystems/hextet-lg/internal/visibility"
)

func TestVisibilityAcrossOutputsAndCache(t *testing.T) {
	var hits atomic.Int32
	doc := `{"rib":[{"prefix":"10.1.0.0/16"},{"prefix":"192.0.2.0/24"},{"prefix":"fd12::/16"},{"prefix":"2001:db8::/32"},{"prefix":"198.51.100.0/24"}]}`
	lg := defaults()
	lg.CacheTTL = time.Minute
	lg.PageSize = 2
	serve := func(w http.ResponseWriter, r *http.Request) { hits.Add(1); fmt.Fprint(w, doc) }
	cfg, closeAll := testConfig(lg, backendSpec{"a", "Alpha", serve}, backendSpec{"b", "Beta", serve})
	defer closeAll()
	cfg.RouteVisibility.Exclude.IPv4 = []visibility.Rule{{Prefix: "10.0.0.0/8", Match: "or-longer"}}
	cfg.RouteVisibility.Exclude.IPv6 = []visibility.Rule{{Prefix: "fd00::/8", Match: "or-longer"}}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	handler := h.Routes()
	cases := []string{
		"type=prefix&q=10.0.0.0%2F8", "type=prefix&q=10.0.0.0%2F8&all=1", "type=prefix&q=10.1.0.0%2F16", "prefix=10.1.0.0%2F16&or-shorter=1", "type=prefix&q=fd00%3A%3A%2F8", "type=prefix&q=fd00%3A%3A%2F8&all=1", "type=prefix&q=fd00%3A%3A%2F8&or-shorter=1",
		"type=asn&q=64500", "community=64500%3A1", "type=large-community&q=64500%3A1%3A2", "ext-community=rt%2064500%3A1", "as=64500&af=ipv6&best=1",
	}
	for _, query := range cases {
		for _, view := range []string{"table", "detail", "text"} {
			target := "/?" + query + "&backend=a&view=" + view
			if view == "text" {
				target += "&output=plain"
			}
			rr := get(handler, target, "HX-Request", "true")
			body := rr.Body.String()
			if view != "text" {
				// The searched value itself is echoed in the canonical document title
				// (and, for compound searches, the query context); nothing else may be.
				body = titleElement.ReplaceAllString(body, "")
				body = strings.ReplaceAll(body, "<span class=\"chip\"><b>prefix</b>=10.1.0.0/16</span>", "")
			}
			if rr.Code != 200 || strings.Contains(body, "10.1.0.0/16") || strings.Contains(body, "fd12::/16") {
				t.Fatalf("%s: %d %s", target, rr.Code, rr.Body.String())
			}
		}
	}
	base := "/?type=asn&q=64500&backend=a&view=text&output=plain"
	first := get(handler, base)
	second := get(handler, base)
	if first.Header().Get("X-Hextet-LG-Routes") != "3" || second.Header().Get("X-Hextet-LG-Routes") != "3" || !strings.Contains(second.Header().Get("X-Hextet-LG-Backends"), "cached=1") {
		t.Fatalf("cache/count: %v %v", first.Header(), second.Header())
	}
	rr := get(handler, "/?type=asn&q=64500&backend=all&view=table&page=2", "HX-Request", "true")
	if rr.Code != 200 || rowCount(rr.Body.String()) != 2 || strings.Contains(rr.Body.String(), "10.1.0.0/16") || strings.Contains(rr.Body.String(), "fd12::/16") {
		t.Fatalf("pagination/htmx: %d %s", rr.Code, rr.Body.String())
	}
	if hits.Load() < 2 {
		t.Fatal("both backends should be queried")
	}
}

func TestVisibilityStreamMemoryAndPartial(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		status    bgp.Status
		count     int
	}{
		{"complete", `{"rib":[{"prefix":"10.1.0.0/16"},{"prefix":"192.0.2.0/24"}]}`, bgp.StatusComplete, 1},
		{"all hidden", `{"rib":[{"prefix":"10.1.0.0/16"}]}`, bgp.StatusComplete, 0},
		{"partial hidden", `{"rib":[{"prefix":"10.1.0.0/16"},`, bgp.StatusPartial, 0},
		{"empty body", "", bgp.StatusFailed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", serveDoc(tc.doc)})
			defer closeAll()
			cfg.RouteVisibility.Exclude.IPv4 = []visibility.Rule{{Prefix: "10.0.0.0/8", Match: "or-longer"}}
			h, err := NewHandler(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			r := h.fetch(context.Background(), h.backends[0], bgp.Query{AS: "64500"})
			defer r.lease.release()
			if r.Status != tc.status || len(r.Routes) != tc.count || r.RouteCount != tc.count {
				t.Fatalf("result: %+v", r.Result)
			}
			var want int64
			for _, route := range r.Routes {
				want += route.MemSize()
			}
			if r.MemBytes != want {
				t.Fatalf("memory %d want %d", r.MemBytes, want)
			}
			if tc.count == 0 && r.MemBytes != 0 {
				t.Fatal("hidden route retained")
			}
			if tc.name == "all hidden" {
				rr := get(h.Routes(), "/?type=asn&q=64500&backend=a&view=text&output=plain")
				if rr.Code != 200 || rr.Header().Get("X-Hextet-LG-Completeness") != "complete" || rr.Header().Get("X-Hextet-LG-Routes") != "0" || !strings.Contains(rr.Body.String(), "No matching routes.") {
					t.Fatalf("empty filtered response: %d %v %s", rr.Code, rr.Header(), rr.Body.String())
				}
			}
		})
	}
}

func TestVisibilitySharedFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int32
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		close(started)
		<-release
		fmt.Fprint(w, `[{"prefix":"10.1.0.0/16"},{"prefix":"192.0.2.0/24"}]`)
	}})
	defer closeAll()
	cfg.RouteVisibility.Exclude.IPv4 = []visibility.Rule{{Prefix: "10.0.0.0/8", Match: "or-longer"}}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			r := h.fetch(context.Background(), h.backends[0], bgp.Query{AS: "64500"})
			defer r.lease.release()
			if len(r.Routes) != 1 || r.Routes[0].Prefix != "192.0.2.0/24" {
				t.Errorf("shared routes: %+v", r.Routes)
			}
		}()
		if i == 0 {
			<-started
		}
	}
	deadline := time.Now().Add(time.Second)
	for h.cache.shared.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if hits.Load() != 1 || h.cache.shared.Load() != 1 {
		t.Fatal("query was not shared")
	}
}
