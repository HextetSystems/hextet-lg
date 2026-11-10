package web

import (
	"fmt"
	"html"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

type bucket struct {
	tokens float64
	last   time.Time
}
type rateLimiter struct {
	mu          sync.Mutex
	entries     map[string]bucket
	cfg         config.Server
	now         func() time.Time
	lastCleanup time.Time
}

func newRateLimiter(c config.Server) *rateLimiter {
	if c.RatePerSecond <= 0 {
		c.RatePerSecond = 10
	}
	if c.RateBurst <= 0 {
		c.RateBurst = 60
	}
	if c.LiveRatePerSecond <= 0 {
		c.LiveRatePerSecond = 1
	}
	if c.LiveRateBurst <= 0 {
		c.LiveRateBurst = 8
	}
	return &rateLimiter{entries: make(map[string]bucket), cfg: c, now: time.Now}
}
func (l *rateLimiter) allow(key string, live bool) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	// Opportunistic cleanup keeps state bounded without a background goroutine.
	if l.lastCleanup.IsZero() || now.Sub(l.lastCleanup) >= time.Minute {
		for k, v := range l.entries {
			if now.Sub(v.last) > 10*time.Minute {
				delete(l.entries, k)
			}
		}
		l.lastCleanup = now
	}
	rate, burst := l.cfg.RatePerSecond, l.cfg.RateBurst
	if live {
		rate, burst = l.cfg.LiveRatePerSecond, l.cfg.LiveRateBurst
		key += "|live"
	} else {
		key += "|page"
	}
	b, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= 10000 {
			return false, 60
		}
		b = bucket{tokens: float64(burst), last: now}
	}
	b.tokens = math.Min(float64(burst), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		l.entries[key] = b
		return false, int(math.Ceil((1 - b.tokens) / rate))
	}
	b.tokens--
	l.entries[key] = b
	return true, 0
}
func clientPeer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}
func requestError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if r.URL.Query().Get("output") == "plain" || strings.Contains(r.Header.Get("Accept"), "text/plain") {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
		fmt.Fprintf(w, "<div role=\"alert\">%s</div>", html.EscapeString(message))
		return
	}
	fmt.Fprintf(w, "<!doctype html><title>Request error</title><h1>Request error</h1><p>%s</p>", html.EscapeString(message))
}

// responseWriteTimeout bounds each individual response write, so a client
// that stops reading cannot hold a renderer slot (and its result lease) forever.
// It is atomic only so tests can shorten it without racing with handlers.
var responseWriteTimeout atomic.Int64

func init() { responseWriteTimeout.Store(int64(15 * time.Second)) }

// respWriter is the one response wrapper every request goes through. It
//   - gives each write its own deadline (see responseWriteTimeout),
//   - remembers whether the header is committed, so a failure after the first
//     byte is never answered with a second WriteHeader (http.Error),
//   - records status, bytes and the first write error for logs and metrics.
type respWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	werr   error
}

func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *respWriter) committed() bool             { return w.status != 0 }
func (w *respWriter) WriteHeader(code int) {
	if w.status != 0 {
		return // already committed; net/http would only log "superfluous response.WriteHeader"
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *respWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(time.Duration(responseWriteTimeout.Load())))
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	if err != nil && w.werr == nil {
		w.werr = err
	}
	return n, err
}
func (w *respWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (h *Handler) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		hdr.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		maxURL, maxQuery, maxParams := h.cfg.Server.MaxURLBytes, h.cfg.Server.MaxQueryBytes, h.cfg.Server.MaxQueryParams
		if maxURL <= 0 {
			maxURL = 8192
		}
		if maxQuery <= 0 {
			maxQuery = 6144
		}
		if maxParams <= 0 {
			maxParams = 64
		}
		if len(r.RequestURI) > maxURL || len(r.URL.RawQuery) > maxQuery {
			requestError(w, r, http.StatusRequestURITooLong, "Request URL is too long.")
			return
		}
		if r.URL.RawQuery != "" && strings.Count(r.URL.RawQuery, "&")+1 > maxParams {
			requestError(w, r, http.StatusBadRequest, "Too many query parameters.")
			return
		}
		vals, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			requestError(w, r, http.StatusBadRequest, "Malformed query string.")
			return
		}
		allowed := map[string]bool{}
		switch r.URL.Path {
		case "/":
			allowed = knownParams
		case "/neighbors", "/summary", "/rtr":
			allowed["backend"] = true
		case "/communities":
			allowed["backend"] = true
			allowed["q"] = true
			allowed["category"] = true
		}
		if allowed != nil && (r.URL.Path == "/" || r.URL.Path == "/neighbors" || r.URL.Path == "/summary" || r.URL.Path == "/rtr" || r.URL.Path == "/communities") {
			for k, v := range vals {
				if len(v) != 1 && r.URL.Path != "/" {
					requestError(w, r, http.StatusBadRequest, "Repeated query parameter.")
					return
				}
				if !allowed[k] && r.URL.Path != "/" {
					requestError(w, r, http.StatusBadRequest, "Unknown query parameter.")
					return
				}
			}
		}
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			live := r.URL.Path == "/neighbors" || r.URL.Path == "/summary" || r.URL.Path == "/rtr" || (r.URL.Path == "/" && len(vals) > 0)
			if ok, retry := h.limiter.allow(clientPeer(r), live); !ok {
				h.metrics.rateLimited.WithLabelValues(map[bool]string{true: "live", false: "page"}[live]).Inc()
				hdr.Set("Retry-After", fmt.Sprint(retry))
				requestError(w, r, http.StatusTooManyRequests, "Too many requests. Please retry shortly.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// cleanupLoop reclaims idle peer buckets even when the instance receives no traffic.
func (l *rateLimiter) cleanupLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			l.mu.Lock()
			now := l.now()
			for k, v := range l.entries {
				if now.Sub(v.last) > 10*time.Minute {
					delete(l.entries, k)
				}
			}
			l.mu.Unlock()
		}
	}
}
