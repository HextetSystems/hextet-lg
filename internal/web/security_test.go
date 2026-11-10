package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

func TestRateLimiterBurstIdentityAndCleanup(t *testing.T) {
	l := newRateLimiter(config.Server{LiveRatePerSecond: 1, LiveRateBurst: 2})
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("a", true); !ok {
			t.Fatal("burst rejected")
		}
	}
	if ok, retry := l.allow("a", true); ok || retry != 1 {
		t.Fatalf("excess: %v %d", ok, retry)
	}
	if ok, _ := l.allow("b", true); !ok {
		t.Fatal("other peer rejected")
	}
	now = now.Add(11 * time.Minute)
	l.allow("c", true)
	if len(l.entries) != 1 {
		t.Fatalf("stale entries: %d", len(l.entries))
	}
}
func TestHTTPProtection(t *testing.T) {
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", serveDoc("{}")})
	for _, tc := range []struct {
		target string
		code   int
	}{{"/?q=" + strings.Repeat("x", 9000), 414}, {"/?" + strings.Repeat("a=1&", 65), 400}, {"/?q=%zz", 400}, {"/neighbors?unknown=1", 400}} {
		req := httptest.NewRequest("GET", "/", nil)
		req.URL.RawQuery = strings.TrimPrefix(tc.target, "/?")
		if strings.HasPrefix(tc.target, "/neighbors") {
			req = httptest.NewRequest("GET", tc.target, nil)
		}
		req.RequestURI = tc.target
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Errorf("%s: %d", tc.target[:min(30, len(tc.target))], w.Code)
		}
	}
	w := get(h, "/healthz")
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("missing security headers")
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("method: %d", w.Code)
	}
}
func TestForwardedHeadersIgnored(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.1:99"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if clientPeer(r) != "192.0.2.1" {
		t.Fatal("trusted spoofed address")
	}
}

func TestRateLimitHTTPAndSpoofing(t *testing.T) {
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", serveDoc("{}")})
	defer closeAll()
	cfg.Server.LiveRatePerSecond = 1
	cfg.Server.LiveRateBurst = 1
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	handler := h.Routes()
	request := func(peer, forward string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/?prefix=192.0.2.1&view=text&output=plain", nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Forwarded-For", forward)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("192.0.2.10:1", "198.51.100.1"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request("192.0.2.10:2", "198.51.100.2"); w.Code != 429 || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "Too many") {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if w := request("192.0.2.11:1", "198.51.100.1"); w.Code != 200 {
		t.Fatal(w.Code)
	}
}
