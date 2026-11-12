package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
	"github.com/HextetSystems/hextet-lg/internal/obs"
)

// reqInfo is the per-request scratch record that handlers fill in so the one
// access-log line can say what the request did. It is touched only by the
// goroutine serving the request.
type reqInfo struct {
	id string
	// start is the monotonic reading taken when protect began handling the
	// request. The access log, the HTTP duration metric and the footer's page
	// generation time all measure from this one instant.
	start                time.Time
	result               bgp.Status // overall route-query outcome, if the request ran one
	backends             int
	cacheHits, cacheMiss int
	searched             bool
	aborted              bool // response deliberately cut off (see renderFailed)
	renderErr            string
}

type reqInfoKey struct{}

func infoFrom(ctx context.Context) *reqInfo {
	i, _ := ctx.Value(reqInfoKey{}).(*reqInfo)
	return i
}

// requestID returns the id of the request that started ctx's work, or "".
func requestID(ctx context.Context) string {
	if i := infoFrom(ctx); i != nil {
		return i.id
	}
	return ""
}

// newRequestID is 64 random bits. Client-supplied ids are never accepted: they
// would let a caller forge log correlation.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// routeName maps a path to a fixed route label. Raw paths and query strings
// never reach logs or metric labels.
func routeName(path string) string {
	switch path {
	case "/":
		return "index"
	case "/neighbors", "/summary", "/rtr", "/communities", "/healthz", "/readyz":
		return path[1:]
	case "/map":
		return "map"
	}
	if strings.HasPrefix(path, "/static/") {
		return "static"
	}
	if strings.HasPrefix(path, "/branding/") || path == "/favicon.ico" {
		return "branding"
	}
	return "other"
}

func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "OTHER"
}

func statusClass(code int) string {
	if code < 100 || code > 599 {
		return "unknown"
	}
	return string(rune('0'+code/100)) + "xx"
}

// protect is the outermost handler: request id, active-request accounting, the
// guard (limits, rate limiting, security headers) and one access-log line plus
// the HTTP metrics when the request ends.
func (h *Handler) protect(next http.Handler) http.Handler {
	inner := h.guard(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &reqInfo{id: newRequestID(), start: start}
		r = r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, info))
		rw := &respWriter{ResponseWriter: w}
		rw.Header().Set("X-Request-Id", info.id)
		route, method := routeName(r.URL.Path), methodLabel(r.Method)
		h.activeHTTP.Add(1)
		finished := false
		defer func() {
			h.activeHTTP.Add(-1)
			h.finishRequest(r, rw, info, route, method, time.Since(start), !finished)
		}()
		inner.ServeHTTP(rw, r)
		finished = true
	})
}

// finishRequest classifies the outcome, updates HTTP metrics and logs once.
func (h *Handler) finishRequest(r *http.Request, rw *respWriter, info *reqInfo, route, method string, dur time.Duration, panicked bool) {
	status := rw.status
	if status == 0 {
		status = http.StatusOK
		if panicked {
			status = http.StatusInternalServerError
		}
	}
	// Did the client go away? Only the request context and write errors say so;
	// a write deadline expiry is a client that stopped reading.
	client := ""
	switch {
	case rw.werr != nil && isTimeout(rw.werr):
		client = "write_timeout"
	case rw.werr != nil:
		client = "disconnected"
	case errors.Is(r.Context().Err(), context.Canceled):
		client = "canceled"
	}
	class := statusClass(status)
	if client == "canceled" || client == "disconnected" {
		// The status was written to nobody; keep it out of the server-error rate.
		class = "client_closed"
		h.metrics.clientAborts.WithLabelValues(route, client).Inc()
	} else if client == "write_timeout" {
		h.metrics.clientAborts.WithLabelValues(route, client).Inc()
	}
	if info.aborted || panicked {
		class = "5xx" // a truncated or crashed response is a server fault whatever status went out first
	}
	h.metrics.httpRequests.WithLabelValues(method, route, class).Inc()
	h.metrics.httpDuration.WithLabelValues(method, route).Observe(dur.Seconds())

	level := slog.LevelInfo
	switch {
	case client == "canceled" || client == "disconnected":
		level = slog.LevelInfo // expected; deliberately not a warning
	case panicked || info.aborted || status == http.StatusInternalServerError:
		level = slog.LevelError
	case route == "readyz" && status == http.StatusServiceUnavailable:
		level = slog.LevelInfo // the expected answer once shutdown has begun
	case status >= 500 || client == "write_timeout":
		level = slog.LevelWarn
	case status == http.StatusTooManyRequests:
		level = slog.LevelDebug // counted in hextet_lg_http_rate_limited_total; a flood must not flood the log
	case status < 400 && (route == "healthz" || route == "readyz" || route == "static" || route == "branding"):
		level = slog.LevelDebug
	}
	if !h.log.Enabled(r.Context(), level) {
		return
	}
	attrs := []slog.Attr{
		slog.String("request_id", info.id),
		slog.String("method", method),
		slog.String("route", route),
		slog.Int("status", status),
		slog.Float64("duration_ms", float64(dur.Microseconds())/1000),
		slog.Int64("bytes", rw.bytes),
	}
	if info.searched {
		attrs = append(attrs, slog.String("result", string(info.result)), slog.Int("backends", info.backends),
			slog.Int("cache_hits", info.cacheHits), slog.Int("cache_misses", info.cacheMiss))
	}
	if client != "" {
		attrs = append(attrs, slog.String("client", client))
	}
	if info.aborted {
		attrs = append(attrs, slog.Bool("aborted", true), slog.String("error", info.renderErr))
	}
	if panicked && !info.aborted {
		attrs = append(attrs, slog.Bool("panicked", true))
	}
	h.log.LogAttrs(r.Context(), level, "request", attrs...)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// renderFailed handles an error from rendering a page. Before the first byte it
// answers 500. After the header is committed there is nothing correct left to
// say: a second WriteHeader would only be logged by net/http as superfluous.
//   - If the write failed, the client is gone; just note it (the access log
//     classifies it), without an alarming message.
//   - If the template itself failed mid-stream, the already-sent 200 page is
//     truncated; abort the connection so the client sees a broken response
//     rather than a page that looks complete.
func (h *Handler) renderFailed(w http.ResponseWriter, r *http.Request, err error) {
	rw, ok := w.(*respWriter)
	if !ok || !rw.committed() {
		http.Error(w, "render page", http.StatusInternalServerError)
		h.log.LogAttrs(r.Context(), slog.LevelError, "render failed", slog.String("request_id", requestID(r.Context())), slog.String("route", routeName(r.URL.Path)), slog.String("error", obs.Clip(err.Error(), 200)))
		return
	}
	if rw.werr != nil || r.Context().Err() != nil {
		return
	}
	if i := infoFrom(r.Context()); i != nil {
		i.aborted, i.renderErr = true, obs.Clip(err.Error(), 200)
	}
	panic(http.ErrAbortHandler)
}

// logOutput is where the default logger writes. Tests replace it.
var logOutput io.Writer = os.Stderr

// defaultLogger is used when no logger is supplied: the configured level and
// format on stderr.
func (h *Handler) defaultLogger() *slog.Logger {
	return obs.NewLogger(logOutput, h.cfg.Logging.Level, h.cfg.Logging.Format)
}
