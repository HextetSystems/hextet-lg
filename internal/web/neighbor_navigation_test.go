package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"

	"github.com/HextetSystems/hextet-lg/internal/visibility"
)

func TestNeighborNavigationAndVisibility(t *testing.T) {
	var aHits, bHits atomic.Int32
	backend := func(hits *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/summary" {
				fmt.Fprint(w, `{"neighbors":[]}`)
				return
			}
			if r.URL.Path == "/neighbors" {
				fmt.Fprint(w, `{"neighbors":[{"remote_addr":"192.0.2.1","description":"edge \"<script>" ,"remote_as":"64500"},{"remote_addr":"2001:DB8:ffff:1234:5678:abcd:ef01:2345"},{"description":"missing"},{"remote_addr":"bad-address"}]}`)
				return
			}
			hits.Add(1)
			q := r.URL.Query()
			if len(q) != 1 || (q.Get("neighbor") != "192.0.2.1" && q.Get("neighbor") != "2001:db8:ffff:1234:5678:abcd:ef01:2345") {
				t.Errorf("unexpected upstream query: %s", r.URL.RawQuery)
			}
			fmt.Fprintf(w, `{"rib":[{"prefix":"10.1.0.0/16"},{"prefix":"fd12::/16"},{"prefix":"2001:db8::/32","true_nexthop":"198.51.100.1","neighbor":{"remote_addr":%q,"description":"edge \"<script>"}},{"prefix":"198.51.100.0/24","neighbor":{"remote_addr":%q}}]}`, q.Get("neighbor"), q.Get("neighbor"))
		}
	}
	lg := defaults()
	lg.CacheTTL = time.Minute
	lg.PageSize = 1
	cfg, closeAll := testConfig(lg, backendSpec{"a", "Alpha", backend(&aHits)}, backendSpec{"b", "Beta", backend(&bHits)})
	defer closeAll()
	cfg.RouteVisibility.Exclude.IPv4 = []visibility.Rule{{Prefix: "10.0.0.0/8", Match: "or-longer"}}
	cfg.RouteVisibility.Exclude.IPv6 = []visibility.Rule{{Prefix: "fd00::/8", Match: "or-longer"}}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	handler := h.Routes()
	page := get(handler, "/neighbors?backend=all").Body.String()
	for _, want := range []string{`href="/?neighbor=192.0.2.1&amp;backend=a&amp;view=table"`, `href="/?neighbor=192.0.2.1&amp;backend=b&amp;view=table"`, `neighbor=2001%3Adb8%3Affff%3A1234%3A5678%3Aabcd%3Aef01%3A2345&amp;backend=a`, `title="edge &#34;&lt;script&gt; — 192.0.2.1 — AS64500"`, `>missing</td>`, `>bad-address</code>`} {
		if !strings.Contains(page, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	if strings.Contains(page, `neighbor=bad-address`) || strings.Contains(page, `neighbor=missing`) {
		t.Fatal("invalid identity linked")
	}
	for _, address := range []string{"192.0.2.1", "2001:db8:ffff:1234:5678:abcd:ef01:2345"} {
		target := neighborURL(address, "b")
		rr := get(handler, target, "HX-Request", "true")
		body := rr.Body.String()
		if rr.Code != 200 || rr.Header().Get("HX-Push-Url") != target || strings.Contains(body, "10.1.0.0/16") || strings.Contains(body, "fd12::/16") || !strings.Contains(body, "2001:db8::/32") {
			t.Fatalf("fragment %s %d %s", target, rr.Code, body)
		}
		if !strings.Contains(body, `title="edge &#34;&lt;script&gt; — `) || !strings.Contains(body, `>edge &#34;&lt;script&gt; <a href=`) {
			t.Fatalf("description/escaping: %s", body)
		}
		if strings.Contains(body, `neighbor=198.51.100.1`) {
			t.Fatal("next hop linked as neighbor")
		}
		plain := "/?neighbor=" + url.QueryEscape(address) + "&backend=b&view=text&output=plain"
		rr = get(handler, plain)
		if rr.Code != 200 || rr.Header().Get("X-Hextet-LG-Routes") != "2" || strings.Contains(rr.Body.String(), "fd12::/16") || !strings.Contains(rr.Body.String(), "198.51.100.0/24") {
			t.Fatalf("plain %d %v %s", rr.Code, rr.Header(), rr.Body.String())
		}
		rr = get(handler, target+"&page=2", "HX-Request", "true")
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "198.51.100.0/24") || strings.Contains(rr.Body.String(), "fd12::/16") {
			t.Fatal("pagination or visibility")
		}
		full := get(handler, target)
		if full.Code != 200 || !strings.Contains(full.Body.String(), "<!doctype html>") {
			t.Fatal("full page navigation")
		}
	}
	if aHits.Load() != 0 || bHits.Load() != 2 {
		t.Fatalf("backend/cache hits %d %d", aHits.Load(), bHits.Load())
	}
}

func TestNeighborLinkDropsSearchContext(t *testing.T) {
	h := m48Handler(t, func(c *config.Config) { c.Display.RouteColumns.Neighbor = true }, backendSpec{"a", "Alpha", serveDoc(`[{"prefix":"2001:db8::/32","true_nexthop":"198.51.100.1","neighbor":{"remote_addr":"192.0.2.1","description":"example"}}]`)})
	rr := get(h, "/?prefix=2001%3Adb8%3A%3A%2F32&af=ipv6&backend=all&view=table", "HX-Request", "true")
	body := rr.Body.String()
	want := `href="/?neighbor=192.0.2.1&amp;backend=a&amp;view=table"`
	if rr.Code != 200 || strings.Count(body, want) != 2 || !strings.Contains(body, `<code class="nb"><span>192.0.2.1</span></code></a>`) || !strings.Contains(body, `>example <a href=`) {
		t.Fatalf("route links: %s", body)
	}
}

func TestNeighborVisibilitySharedFlight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var hits atomic.Int32
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		close(started)
		<-release
		fmt.Fprint(w, `[{"prefix":"10.1.0.0/16"},{"prefix":"fd12::/16"},{"prefix":"2001:db8::/32"}]`)
	}})
	defer closeAll()
	cfg.RouteVisibility.Exclude.IPv4 = []visibility.Rule{{Prefix: "10.0.0.0/8", Match: "or-longer"}}
	cfg.RouteVisibility.Exclude.IPv6 = []visibility.Rule{{Prefix: "fd00::/8", Match: "or-longer"}}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := get(h.Routes(), neighborURL("192.0.2.1", "a"), "HX-Request", "true")
			if rr.Code != 200 || strings.Contains(rr.Body.String(), "10.1.0.0/16") || strings.Contains(rr.Body.String(), "fd12::/16") || !strings.Contains(rr.Body.String(), "2001:db8::/32") {
				t.Error("visibility on shared flight")
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
		t.Fatal("flight not shared")
	}
}

func TestNeighborURLAddresses(t *testing.T) {
	for _, tc := range []struct{ address, want string }{
		{"192.0.2.1", "/?neighbor=192.0.2.1&backend=a&view=table"},
		{"2001:DB8::1", "/?neighbor=2001%3Adb8%3A%3A1&backend=a&view=table"},
		{"fe80::1%em0", "/?neighbor=fe80%3A%3A1%25em0&backend=a&view=table"},
		{"", ""}, {"peer-a", ""}, {"192.0.2.999", ""},
	} {
		if got := neighborURL(tc.address, "a"); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.address, got, tc.want)
		}
	}
}
