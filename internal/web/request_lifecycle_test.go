package web

// M3.2: admission counters, client-disconnect handling, shutdown logging.

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/obs"
)

func TestAdmissionRejectionsAreCountedAndLogged(t *testing.T) {
	lg := defaults()
	lg.MaxConcurrentWork, lg.MaxQueuedWork, lg.QueueWait = 1, 1, 20*time.Millisecond
	h, buf := newObsHandler(t, lg, "info", backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	h.work.active <- struct{}{} // the only slot is busy
	rr := get(h.Routes(), "/summary")
	<-h.work.active
	if rr.Code != 503 || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d", rr.Code)
	}
	wantMetric(t, h, 1, "hextet_lg_work_gate_rejections_total", "reason", "wait_timeout")
	wantMetric(t, h, 1, "hextet_lg_http_requests_total", "route", "summary", "status_class", "5xx")
	if r := find(buf.records(t), "request", "status", 503); r == nil || r["level"] != "WARN" {
		t.Fatalf("503 overload record: %v", r)
	}

	h.StopAdmission()
	if rr := get(h.Routes(), "/summary"); rr.Code != 503 {
		t.Fatalf("status %d", rr.Code)
	}
	wantMetric(t, h, 1, "hextet_lg_work_gate_rejections_total", "reason", "shutdown")
	wantMetric(t, h, 1, "hextet_lg_shutting_down")
}

// failWriter is a ResponseWriter whose connection has gone away.
type failWriter struct {
	hdr         http.Header
	headerCalls int
	writes      int
	body        strings.Builder
}

func (f *failWriter) Header() http.Header { return f.hdr }
func (f *failWriter) WriteHeader(int)     { f.headerCalls++ }
func (f *failWriter) Write(p []byte) (int, error) {
	f.writes++
	return 0, io.ErrClosedPipe
}

// A write that fails after the response is committed must not be answered with
// http.Error: no second WriteHeader, no error text mixed into the body.
func TestWriteFailureAfterCommitDoesNotWriteHeaderAgain(t *testing.T) {
	for _, target := range []string{"/", "/communities", "/summary", "/?q=10.0.0.0/24&type=prefix"} {
		t.Run(target, func(t *testing.T) {
			h, buf := newObsHandler(t, defaults(), "info", backendSpec{id: "a", name: "A", h: serveDoc(ribJSON(2, false))})
			f := &failWriter{hdr: http.Header{}}
			h.Routes().ServeHTTP(f, httptest.NewRequest("GET", target, nil))
			if f.headerCalls != 0 || f.writes != 1 {
				t.Fatalf("headerCalls=%d writes=%d: a second header write or retry was attempted", f.headerCalls, f.writes)
			}
			rec := find(buf.records(t), "request", "route", routeName(strings.SplitN(target, "?", 2)[0]))
			if rec == nil || rec["client"] != "disconnected" || rec["level"] != "INFO" {
				t.Fatalf("disconnect must be classified and quiet: %v", rec)
			}
			if v, _ := metric(t, h, "hextet_lg_http_client_aborts_total", "kind", "disconnected"); v != 1 {
				t.Errorf("client abort counter = %v", v)
			}
			if v, _ := metric(t, h, "hextet_lg_http_requests_total", "status_class", "client_closed"); v != 1 {
				t.Errorf("client_closed requests = %v", v)
			}
			if snap := h.snapshot(); snap.Work != 0 || snap.Retained != int64(cachedBytes(h)) {
				t.Errorf("resources not released: %s", snap)
			}
		})
	}
}

func cachedBytes(h *Handler) int64 { return h.cacheOwnedBytes() }

func TestRenderFailedBeforeAndAfterCommit(t *testing.T) {
	h, _ := newObsHandler(t, defaults(), "error", backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	req := httptest.NewRequest("GET", "/", nil)

	rec := httptest.NewRecorder()
	h.renderFailed(&respWriter{ResponseWriter: rec}, req, errors.New("boom"))
	if rec.Code != 500 {
		t.Errorf("before commit: %d", rec.Code)
	}

	// Committed, write error: the client is gone, say nothing more.
	rec = httptest.NewRecorder()
	rw := &respWriter{ResponseWriter: rec}
	rw.WriteHeader(200)
	rw.werr = io.ErrClosedPipe
	h.renderFailed(rw, req, errors.New("write failed"))
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("after commit with write error: %d %q", rec.Code, rec.Body.String())
	}

	// Committed, template failed: the page is truncated, so the connection is
	// aborted instead of letting it look complete.
	rec = httptest.NewRecorder()
	rw = &respWriter{ResponseWriter: rec}
	rw.WriteHeader(200)
	func() {
		defer func() {
			if p := recover(); p != http.ErrAbortHandler {
				t.Errorf("expected ErrAbortHandler, got %v", p)
			}
		}()
		h.renderFailed(rw, req, errors.New("template failed"))
	}()
	// Duplicate WriteHeader calls never reach the underlying writer.
	rw.WriteHeader(500)
	if rec.Code != 200 {
		t.Errorf("second WriteHeader leaked: %d", rec.Code)
	}
}

type syncLog struct{ buf syncBuf }

func (s *syncLog) logger() *log.Logger { return log.New(&s.buf, "", 0) }

// With a real TCP client that stops reading and disconnects, net/http must not
// complain about a superfluous WriteHeader, the access log must classify the
// event without alarm, and every lease and slot must come back.
func TestRealClientDisconnectIsQuietAndReleasesResources(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"html", "/?as=64500&view=detail"},
		{"plain", "/?as=64500&view=detail&output=plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg := defaults()
			lg.CacheTTL = time.Minute
			lg.PageSize = 1000
			lg.QueryTimeout = 30 * time.Second // the warm-up must complete even under -race
			doc := strings.ReplaceAll(ribJSON(1000, false), `"aspath":"64500 `, `"aspath":"`+strings.Repeat("64500 ", 2000))
			h, logs := newObsHandler(t, lg, "info", backendSpec{id: "a", name: "A", h: serveDoc(doc)})
			var srvLog syncLog
			srv := httptest.NewUnstartedServer(h.Routes())
			srv.Config.ErrorLog = srvLog.logger()
			srv.Start()
			defer srv.Close()
			res, err := http.Get(srv.URL + tc.target) // warm the cache
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			addr := strings.TrimPrefix(srv.URL, "http://")
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			io.WriteString(conn, "GET "+tc.target+" HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
			waitFor(t, "renderer to start", 5*time.Second, func() bool { return len(h.work.active) > 0 }, nil)
			time.Sleep(100 * time.Millisecond) // let the socket buffers fill
			conn.Close()
			waitFor(t, "writer to exit", 5*time.Second, func() bool { return len(h.work.active) == 0 && h.activeHTTP.Load() == 0 }, func() string { return h.snapshot().String() })
			if s := srvLog.buf.String(); strings.Contains(s, "superfluous") {
				t.Fatalf("net/http complained: %s", s)
			}
			var disc map[string]any
			waitFor(t, "disconnect record", 5*time.Second, func() bool {
				disc = find(logs.records(t), "request", "client", "disconnected")
				if disc == nil {
					disc = find(logs.records(t), "request", "client", "canceled")
				}
				return disc != nil
			}, func() string { return logs.String() })
			if disc["level"] != "INFO" {
				t.Errorf("client disconnect logged at %v: %v", disc["level"], disc)
			}
			for _, r := range logs.records(t) {
				if r["level"] == "ERROR" || r["level"] == "WARN" {
					t.Errorf("alarming record for a client disconnect: %v", r)
				}
			}
			h.Close()
			if used, _ := h.budget.usage(); used != 0 {
				t.Errorf("retained %d after close", used)
			}
		})
	}
}

// A client that cancels while its query is in flight is logged quietly, kept
// out of the 5xx rate, and releases the upstream query.
func TestClientCancellationIsQuiet(t *testing.T) {
	started := make(chan struct{})
	h, logs := newObsHandler(t, defaults(), "info", backendSpec{id: "a", name: "A", h: func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"rib":[`)
		flushW(w)
		close(started)
		<-r.Context().Done()
	}})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/?as=64500&view=text&output=plain", nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
		}
	}()
	<-started
	cancel()
	<-done
	var rec map[string]any
	waitFor(t, "cancellation record", 5*time.Second, func() bool { rec = find(logs.records(t), "request", "client", "canceled"); return rec != nil }, func() string { return logs.String() })
	if rec["level"] != "INFO" {
		t.Errorf("level %v", rec["level"])
	}
	for _, r := range logs.records(t) {
		if r["level"] == "ERROR" || r["level"] == "WARN" {
			t.Errorf("alarming record for a client cancellation: %v", r)
		}
	}
	waitSnapshot(t, h, "idle", 5*time.Second, idleSnapshot.idle)
	wantMetric(t, h, 1, "hextet_lg_http_client_aborts_total", "route", "index", "kind", "canceled")
	wantMetric(t, h, 1, "hextet_lg_http_requests_total", "route", "index", "status_class", "client_closed")
	if v, ok := metric(t, h, "hextet_lg_http_requests_total", "route", "index", "status_class", "5xx"); ok && v != 0 {
		t.Errorf("client cancellation counted as server error: %v", v)
	}
	// The backend did not misbehave: a cancellation is no backend failure.
	if _, ok := metric(t, h, "hextet_lg_backend_last_failure_timestamp_seconds", "backend", "a"); ok {
		t.Error("cancellation recorded as backend failure")
	}
	wantMetric(t, h, 1, "hextet_lg_backend_failures_total", "backend", "a", "operation", "rib", "reason", "canceled")
}

func TestShutdownLogging(t *testing.T) {
	t.Run("graceful", func(t *testing.T) {
		h, logs := newObsHandler(t, defaults(), "info", backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
		srv := httptest.NewServer(h.Routes())
		defer srv.Close()
		if err := h.Shutdown(srv.Config, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		recs := logs.records(t)
		for _, msg := range []string{"shutdown initiated", "admission disabled", "shutdown complete"} {
			if find(recs, msg) == nil {
				t.Fatalf("missing %q in %s", msg, logs.String())
			}
		}
		done := find(recs, "shutdown complete")
		if done["graceful"] != true || done["active_http"].(float64) != 0 || done["active_work"].(float64) != 0 || done["active_flights"].(float64) != 0 || done["retained_bytes"].(float64) != 0 {
			t.Errorf("final counts: %v", done)
		}
		if _, ok := done["duration_ms"]; !ok {
			t.Errorf("no duration: %v", done)
		}
		if find(recs, "shutdown initiated")["grace"] != "5s" {
			t.Errorf("grace not logged: %v", find(recs, "shutdown initiated"))
		}
	})
	t.Run("deadline", func(t *testing.T) {
		started := make(chan struct{})
		h, logs := newObsHandler(t, defaults(), "info", backendSpec{id: "a", name: "A", h: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"rib":[`)
			flushW(w)
			close(started)
			<-r.Context().Done()
		}})
		srv := httptest.NewServer(h.Routes())
		defer srv.Close()
		go func() {
			if res, err := http.Get(srv.URL + "/?as=64500&output=plain&view=text"); err == nil {
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
			}
		}()
		<-started
		if err := h.Shutdown(srv.Config, 100*time.Millisecond); err == nil {
			t.Fatal("expected the grace period to expire")
		}
		recs := logs.records(t)
		adm := find(recs, "admission disabled")
		if adm == nil || adm["active_http"].(float64) != 1 || adm["active_work"].(float64) != 1 || adm["active_flights"].(float64) != 1 {
			t.Fatalf("outstanding work at shutdown start: %v", adm)
		}
		exp := find(recs, "shutdown grace expired; closing remaining connections")
		fin := find(recs, "shutdown finished after deadline")
		if exp == nil || exp["level"] != "WARN" || fin == nil || fin["graceful"] != false || fin["level"] != "WARN" {
			t.Fatalf("deadline records: %v / %v", exp, fin)
		}
		// The final record is read after the forced-closed handlers unwound.
		if fin["settled"] != true || fin["active_http"].(float64) != 0 || fin["active_work"].(float64) != 0 || fin["active_decodes"].(float64) != 0 {
			t.Errorf("final counts not final: %v", fin)
		}
		waitSnapshot(t, h, "idle after forced close", 5*time.Second, idleSnapshot.idle)
	})
}

var _ = obs.Discard
