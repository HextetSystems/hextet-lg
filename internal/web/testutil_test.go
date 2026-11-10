package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

// ribJSON builds a bgpctl-style document with n routes (IPv4, or IPv6 when v6).
func ribJSON(n int, v6 bool) string {
	var b strings.Builder
	b.WriteString("{\n\"rib\": [\n")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",\n")
		}
		prefix := fmt.Sprintf("10.%d.%d.0/24", (i>>8)&255, i&255)
		if v6 {
			prefix = fmt.Sprintf("2001:db8:%x::/48", i)
		}
		fmt.Fprintf(&b, `{"prefix":%q,"aspath":"64500 %d","true_nexthop":"192.0.2.1","exit_nexthop":"192.0.2.1","neighbor":{"description":"peer-a","remote_addr":"192.0.2.1"},"valid":true,"best":true,"ovs":"valid","avs":"unknown","origin":"igp","localpref":100,"metric":0,"last_update_sec":60,"communities":["64500:1"],"extended_communities":["rt 64500:1"],"large_communities":["395089:0:8","395089:5:2"]}`, prefix, 65000+i%50)
	}
	b.WriteString("\n]\n}\n")
	return b.String()
}

func flushW(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

type backendSpec struct {
	id, name string
	h        http.HandlerFunc
}

func testConfig(lg config.LookingGlass, specs ...backendSpec) (config.Config, func()) {
	lg.DefaultBackend = "all"
	cfg := config.Config{Server: config.Server{Title: "Test LG", Listen: "127.0.0.1:0", RatePerSecond: 10000, RateBurst: 10000, LiveRatePerSecond: 10000, LiveRateBurst: 10000}, LookingGlass: lg}
	var servers []*httptest.Server
	for _, s := range specs {
		srv := httptest.NewServer(s.h)
		servers = append(servers, srv)
		cfg.Backends = append(cfg.Backends, config.Backend{ID: s.id, Name: s.name, Location: "Test", URL: srv.URL, Enabled: true})
	}
	return cfg, func() {
		for _, s := range servers {
			s.Close()
		}
	}
}

func defaults() config.LookingGlass {
	lg := config.Defaults()
	lg.QueryTimeout = 2 * time.Second
	lg.MaxResponseBytes = 64 << 20
	lg.CacheTTL = 0
	return lg
}

func newTestHandler(t *testing.T, lg config.LookingGlass, specs ...backendSpec) http.Handler {
	t.Helper()
	cfg, closeAll := testConfig(lg, specs...)
	t.Cleanup(closeAll)
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h.Routes()
}

func serveDoc(doc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, doc) }
}

func get(h http.Handler, target string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
