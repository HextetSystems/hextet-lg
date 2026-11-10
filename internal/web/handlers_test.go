package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

func TestMultiBackendPartialFailureAndInterface(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("as") != "15169" {
			t.Errorf("expected native source-AS filter, got %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{"prefix":"8.8.8.0/24","aspath":"15169","origin":"igp","true_nexthop":"192.0.2.1","neighbor":"peer-a","localpref":100,"ovs":"valid","avs":"valid","large_communities":["395089:0:6"]}]`))
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "backend failure", http.StatusBadGateway) }))
	defer bad.Close()
	cfg := config.Config{Server: config.Server{Listen: "127.0.0.1:0", Title: "Test LG"}, LookingGlass: config.LookingGlass{DefaultBackend: "all", QueryTimeout: time.Second, MaxResponseBytes: 1024 * 1024, MaxConcurrentBackends: 1}, Backends: []config.Backend{{ID: "a", Name: "Alpha", URL: good.URL, Enabled: true}, {ID: "b", Name: "Beta", URL: bad.URL, Enabled: true}}}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/?type=asn&q=15169&backend=all", nil)
	h.Routes().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Alpha", "Beta", "8.8.8.0/24", "15169", "192.0.2.1", "395089:0:6", "Query failed"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from rendered response", want)
		}
	}
}

func TestHealthEndpointAndHTMXFragment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `[]`) }))
	defer srv.Close()
	cfg := config.Config{Server: config.Server{Listen: "127.0.0.1:0", Title: "Test"}, LookingGlass: config.LookingGlass{DefaultBackend: "all", QueryTimeout: time.Second, MaxResponseBytes: 4096, MaxConcurrentBackends: 1}, Backends: []config.Backend{{ID: "one", Name: "One", URL: srv.URL, Enabled: true}}}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler := h.Routes()
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest("GET", "/healthz", nil))
	if health.Code != 200 || health.Body.String() != "ok\n" {
		t.Fatalf("health response: %d %q", health.Code, health.Body.String())
	}
	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest("GET", "/static/htmx.min.js", nil))
	if asset.Code != 200 || !strings.Contains(asset.Body.String(), "htmx") {
		t.Fatalf("embedded HTMX asset unavailable: %d", asset.Code)
	}
	frag := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/?type=prefix&q=8.8.8.8&backend=all", nil)
	req.Header.Set("HX-Request", "true")
	handler.ServeHTTP(frag, req)
	if strings.Contains(frag.Body.String(), "<!doctype html>") {
		t.Fatal("HTMX request returned full page")
	}
	if frag.Code != 200 {
		t.Fatal(frag.Code)
	}
}
