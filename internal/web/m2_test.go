package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

func TestM2Pages(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/neighbors", "/summary":
			w.Write([]byte(`{"neighbors":[{"remote_as":"15169","remote_addr":"192.0.2.1","description":"<script>alert(1)</script>","state":"Established","stats":{"prefixes":{"received":42}}}]}`))
		default:
			w.Write([]byte(`{"rib":[{"prefix":"8.8.8.0/24","aspath":"15169","best":true,"large_communities":["395089:0:6"],"unknown":"<script>"}]}`))
		}
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret backend address", 502) }))
	defer bad.Close()
	cfg := config.Config{Server: config.Server{Title: "Test"}, LookingGlass: config.LookingGlass{DefaultBackend: "all", QueryTimeout: time.Second, MaxResponseBytes: 4096, MaxConcurrentBackends: 1}, Backends: []config.Backend{{ID: "a", Name: "Alpha", URL: good.URL, Enabled: true}, {ID: "b", Name: "Beta", URL: bad.URL, Enabled: true}}}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		url   string
		wants []string
	}{{"/?type=prefix&q=8.8.8.8", []string{"BEST", "Raw OpenBGPD JSON", "395089%3A0%3A6", "Query failed"}}, {"/neighbors", []string{"192.0.2.1", "42", "&lt;script&gt;", "Backend unavailable"}}, {"/summary", []string{"Established", "Sessions reported", "Backend unavailable"}}, {"/communities?category=Route+classification&q=transit", []string{"No community definitions are configured", "No matching communities."}}} {
		t.Run(tc.url, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.Routes().ServeHTTP(rr, httptest.NewRequest("GET", tc.url, nil))
			if rr.Code != 200 {
				t.Fatal(rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			for _, want := range tc.wants {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q", want)
				}
			}
			if strings.Contains(body, "<script>alert(1)</script>") || strings.Contains(body, "secret backend address") {
				t.Error("unsafe data exposed")
			}
		})
	}
}

func TestBookmarkAndPlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rib" {
			w.Write([]byte(`{"rib":[{"prefix":"2001:db8::/32","aspath":"64500","large_communities":["395089:0:6"]}]}`))
		} else {
			w.Write([]byte(`{"neighbors":[]}`))
		}
	}))
	defer srv.Close()
	cfg := config.Config{Server: config.Server{Title: "Test"}, LookingGlass: config.LookingGlass{DefaultBackend: "all", QueryTimeout: time.Second, MaxResponseBytes: 4096, MaxConcurrentBackends: 1}, Backends: []config.Backend{{ID: "stable", Name: "Test", URL: srv.URL, Enabled: true}}}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/?type=prefix&q=2001%3Adb8%3A%3A%2F32&backend=stable&view=table", "/?type=prefix&q=2001%3Adb8%3A%3A%2F32&backend=stable&view=text", "/?type=prefix&q=2001%3Adb8%3A%3A%2F32&backend=stable&view=detail&output=plain", "/neighbors?backend=stable", "/summary?backend=stable", "/communities?q=transit&category=Route+classification"} {
		rr := httptest.NewRecorder()
		h.Routes().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("%s: %d", path, rr.Code)
		}
		if strings.Contains(path, "type=prefix") && !strings.Contains(rr.Body.String(), "2001:db8::/32") {
			t.Errorf("route missing for %s", path)
		}
		if strings.Contains(path, "output=plain") && !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/plain") {
			t.Error("incorrect content type")
		}
	}
}
