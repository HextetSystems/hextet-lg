package web

// Acceptance A: graceful shutdown with shared queries and slow writers.
// Every test drives the real Handler.Shutdown sequence used by main, over real
// TCP connections, with short configurable grace periods.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type shutdownResult struct {
	err     error
	elapsed time.Duration
}

func startShutdown(h *Handler, srv *httptest.Server, grace time.Duration) <-chan shutdownResult {
	out := make(chan shutdownResult, 1)
	go func() {
		start := time.Now()
		err := h.Shutdown(srv.Config, grace)
		out <- shutdownResult{err, time.Since(start)}
	}()
	return out
}

// gatedBackend streams the first bytes of a route document, announces itself,
// then holds the response until released (or until its request is canceled).
type gatedBackend struct {
	doc      string
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
	requests atomic.Int32
}

func newGatedBackend(routes int) *gatedBackend {
	return &gatedBackend{doc: ribJSON(routes, false), started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
}

func (g *gatedBackend) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/rib" {
		io.WriteString(w, `{}`)
		return
	}
	if g.requests.Add(1) == 1 {
		close(g.started)
	}
	io.WriteString(w, g.doc[:8])
	flushW(w)
	select {
	case <-g.release:
		io.WriteString(w, g.doc[8:])
	case <-r.Context().Done():
		close(g.canceled)
	}
}

type clientOutcome struct {
	status       int
	completeness string
	bodyErr      error
	body         []byte
	err          error
}

func doGet(url string) clientOutcome {
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := c.Get(url)
	if err != nil {
		return clientOutcome{err: err}
	}
	defer res.Body.Close()
	b, berr := io.ReadAll(res.Body)
	return clientOutcome{status: res.StatusCode, completeness: res.Header.Get("X-Hextet-LG-Completeness"), body: b, bodyErr: berr}
}

// startSharedClients runs two real HTTP clients (one plaintext, one HTML) that
// share a single in-flight backend query, and returns once both have joined it.
func startSharedClients(t *testing.T, h *Handler, srv *trackedServer, g *gatedBackend) []<-chan clientOutcome {
	t.Helper()
	var outs []<-chan clientOutcome
	for _, target := range []string{"/?as=64500&view=text&output=plain", "/?as=64500&view=table"} {
		ch := make(chan clientOutcome, 1)
		outs = append(outs, ch)
		go func() { ch <- doGet(srv.URL + target) }()
		if len(outs) == 1 {
			select {
			case <-g.started:
			case <-time.After(5 * time.Second):
				t.Fatal("backend never started")
			}
		}
	}
	waitForFlightWaitersAny(t, h, 2)
	return outs
}

// New work must be refused as soon as shutdown starts: not ready, route work
// rejected before it can reach a backend, liveness unaffected.
func assertNoNewAdmission(t *testing.T, h *Handler, g *gatedBackend) {
	t.Helper()
	waitFor(t, "shutdown to stop admission", 5*time.Second, h.stopping.Load, nil)
	before := g.requests.Load()
	for _, target := range []string{"/?as=64501&view=text&output=plain", "/?as=64500&view=table", "/neighbors"} {
		rr := httptest.NewRecorder()
		h.Routes().ServeHTTP(rr, httptest.NewRequest("GET", target, nil))
		if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
			t.Fatalf("new work %s admitted during shutdown: status=%d retry=%q", target, rr.Code, rr.Header().Get("Retry-After"))
		}
	}
	if got := g.requests.Load(); got != before {
		t.Fatalf("new backend queries started during shutdown: %d -> %d", before, got)
	}
	ready, live := httptest.NewRecorder(), httptest.NewRecorder()
	h.Routes().ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
	h.Routes().ServeHTTP(live, httptest.NewRequest("GET", "/healthz", nil))
	if ready.Code != http.StatusServiceUnavailable || live.Code != http.StatusOK {
		t.Fatalf("readyz=%d healthz=%d", ready.Code, live.Code)
	}
}

func receive[T any](t *testing.T, ch <-chan T, what string, d time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// Two clients share one in-flight query. Shutdown begins; the query finishes
// inside the grace period, so both requests complete normally, nothing new is
// admitted, shutdown reports no deadline error, and every resource returns.
func TestShutdownSharedQueryCompletesWithinGrace(t *testing.T) {
	g := newGatedBackend(50)
	lg := defaults()
	lg.CacheTTL = time.Minute
	cfg, closeBackend := testConfig(lg, backendSpec{id: "a", name: "A", h: g.handler})
	defer closeBackend()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTrackedServer(h)
	defer srv.Close()
	clients := startSharedClients(t, h, srv, g)

	const grace = 20 * time.Second
	done := startShutdown(h, srv.Server, grace)
	assertNoNewAdmission(t, h, g)
	select {
	case r := <-done:
		t.Fatalf("shutdown returned with active requests: %+v", r)
	default:
	}
	close(g.release)
	for i, c := range clients {
		out := receive(t, c, "client", 10*time.Second)
		if out.err != nil || out.bodyErr != nil || out.status != 200 || out.completeness != "complete" {
			t.Fatalf("client %d did not complete during grace: %+v", i, out)
		}
		if !strings.Contains(string(out.body), "10.0.0.0/24") {
			t.Fatalf("client %d body lacks routes", i)
		}
	}
	r := receive(t, done, "shutdown", 10*time.Second)
	if r.err != nil || r.elapsed >= grace {
		t.Fatalf("shutdown = %+v, want graceful completion well inside %s", r, grace)
	}
	if got := g.requests.Load(); got != 1 {
		t.Fatalf("backend saw %d queries for two sharing clients", got)
	}
	waitFor(t, "handlers to exit", 5*time.Second, func() bool { return srv.inflight.Load() == 0 }, nil)
	if s := waitSnapshot(t, h, "idle resources", 5*time.Second, idleSnapshot.idle); !s.idle() {
		t.Fatal(s)
	}
	t.Logf("graceful: elapsed=%s final=%s", r.elapsed.Round(time.Millisecond), h.snapshot())
}

// Same setup, but the backend never answers. At the grace deadline the
// remaining requests are canceled, the shared backend query is terminated, both
// handlers exit and every reservation and work slot is released.
func TestShutdownDeadlineCancelsSharedQuery(t *testing.T) {
	g := newGatedBackend(50)
	lg := defaults()
	lg.QueryTimeout = time.Minute // the deadline, not the query timeout, must end the query
	lg.CacheTTL = time.Minute
	cfg, closeBackend := testConfig(lg, backendSpec{id: "a", name: "A", h: g.handler})
	defer closeBackend()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTrackedServer(h)
	defer srv.Close()
	clients := startSharedClients(t, h, srv, g)
	if s := h.snapshot(); s.Work != 2 || s.Flights != 1 || s.Decodes != 1 || s.Retained == 0 {
		t.Fatalf("expected two renderers on one decode while active: %s", s)
	}

	const grace = 400 * time.Millisecond
	done := startShutdown(h, srv.Server, grace)
	assertNoNewAdmission(t, h, g)
	select {
	case <-g.canceled:
		t.Fatal("backend canceled before the grace deadline")
	default:
	}
	r := receive(t, done, "shutdown", 10*time.Second)
	if !errors.Is(r.err, context.DeadlineExceeded) || r.elapsed < grace || r.elapsed > grace+3*time.Second {
		t.Fatalf("shutdown = %+v, want DeadlineExceeded after ~%s", r, grace)
	}
	receive(t, g.canceled, "backend query cancellation", 5*time.Second)
	for i, c := range clients {
		out := receive(t, c, "client", 5*time.Second)
		if out.err == nil && out.bodyErr == nil && out.status == 200 && out.completeness == "complete" {
			t.Fatalf("client %d reported a complete result for an unfinished query", i)
		}
	}
	waitFor(t, "both handlers to exit", 5*time.Second, func() bool { return srv.inflight.Load() == 0 }, nil)
	s := waitSnapshot(t, h, "idle resources", 5*time.Second, idleSnapshot.idle)
	t.Logf("deadline: shutdown after %s (%v); final=%s", r.elapsed.Round(time.Millisecond), r.err, s)
}

// ---- slow response writers -------------------------------------------------

type slowFixture struct {
	h      *Handler
	srv    *trackedServer
	addr   string
	target string
}

// newSlowFixture serves a cached result several times larger than any socket
// buffer, so a client that stops reading necessarily blocks the writer.
func newSlowFixture(t *testing.T, target string) *slowFixture {
	t.Helper()
	lg := defaults()
	lg.CacheTTL = time.Minute
	lg.PageSize = 1000
	lg.QueryTimeout = 30 * time.Second // the warm-up must be a complete result even under -race
	doc := strings.ReplaceAll(ribJSON(1000, false), `"aspath":"64500 `, `"aspath":"`+strings.Repeat("64500 ", 2000))
	cfg, closeBackend := testConfig(lg, backendSpec{id: "a", name: "A", h: serveDoc(doc)})
	t.Cleanup(closeBackend)
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	srv := newTrackedServer(h)
	t.Cleanup(srv.Close)
	// Warm the cache so the slow phase is render-only.
	out := doGet(srv.URL + target)
	if out.err != nil || out.bodyErr != nil || out.status != 200 || out.completeness != "complete" || len(out.body) < 4<<20 {
		t.Fatalf("warm request: status=%d len=%d err=%v/%v", out.status, len(out.body), out.err, out.bodyErr)
	}
	if n, _ := h.cache.Stats(); n != 1 {
		t.Fatal("result not cached")
	}
	waitSnapshot(t, h, "warm request to finish", 5*time.Second, idleSnapshot.quiescent)
	srv.written.Store(0)
	return &slowFixture{h: h, srv: srv, addr: strings.TrimPrefix(srv.URL, "http://"), target: target}
}

// dial opens a raw TCP connection with a small receive buffer and sends the
// request. A tiny buffer (2 KiB) never drains by itself; a moderate one (32 KiB)
// still sits far below the response size but lets a slow reader make steady
// progress instead of stalling in the kernel's zero-window persist timer.
func (f *slowFixture) dial(t *testing.T, rcvbuf int) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.(*net.TCPConn).SetReadBuffer(rcvbuf); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET "+f.target+" HTTP/1.1\r\nHost: "+f.addr+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	return conn, bufio.NewReaderSize(conn, 4096)
}

var slowTargets = []struct{ name, target, endMarker string }{
	{"html", "/?as=64500&view=detail", "</html>"},
	{"plain", "/?as=64500&view=detail&output=plain", ""},
}

// A client that keeps reading, slowly, finishes inside the grace period even
// though shutdown started mid-response; shutdown completes without a deadline.
func TestShutdownSlowWriterMakingProgressCompletes(t *testing.T) {
	for _, tc := range slowTargets {
		t.Run(tc.name, func(t *testing.T) {
			f := newSlowFixture(t, tc.target)
			conn, br := f.dial(t, 32<<10)
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			res, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatal(err)
			}
			first := make([]byte, 32<<10)
			if _, err := io.ReadFull(res.Body, first); err != nil {
				t.Fatal(err)
			}
			if f.srv.inflight.Load() != 1 || f.h.snapshot().Work != 1 {
				t.Fatalf("writer should still be mid-response: inflight=%d %s", f.srv.inflight.Load(), f.h.snapshot())
			}
			const grace = 20 * time.Second
			done := startShutdown(f.h, f.srv.Server, grace)
			waitFor(t, "admission to stop", 5*time.Second, f.h.stopping.Load, nil)
			select {
			case r := <-done:
				t.Fatalf("shutdown abandoned a writer that was still progressing: %+v", r)
			default:
			}
			// Keep reading slowly: small reads with a pause, never idle.
			total := len(first)
			buf := make([]byte, 16<<10)
			var tail []byte
			for {
				n, err := res.Body.Read(buf)
				total += n
				if n > 0 {
					tail = append(tail[:0], buf[max(0, n-64):n]...)
					time.Sleep(200 * time.Microsecond)
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("progressing client was cut off after %d bytes: %v", total, err)
				}
			}
			if tc.endMarker != "" && !bytes.Contains(tail, []byte(tc.endMarker)) {
				t.Fatalf("response ended without %q: %q", tc.endMarker, tail)
			}
			r := receive(t, done, "shutdown", 10*time.Second)
			if r.err != nil || r.elapsed >= grace {
				t.Fatalf("shutdown = %+v", r)
			}
			waitFor(t, "handler to exit", 5*time.Second, func() bool { return f.srv.inflight.Load() == 0 }, nil)
			s := waitSnapshot(t, f.h, "idle resources", 5*time.Second, idleSnapshot.idle)
			t.Logf("%s: delivered %d bytes during grace; shutdown=%s; final=%s", tc.name, total, r.elapsed.Round(time.Millisecond), s)
		})
	}
}

// A client that stops reading cannot hold shutdown past the grace deadline:
// the outstanding write is aborted, the lease and the renderer slot are released.
func TestShutdownBlockedWriterTerminatesAtDeadline(t *testing.T) {
	for _, tc := range slowTargets {
		t.Run(tc.name, func(t *testing.T) {
			f := newSlowFixture(t, tc.target)
			f.dial(t, 2048)
			waitFor(t, "writer to start and fill the socket", 5*time.Second, func() bool { return f.srv.written.Load() > 0 && f.h.snapshot().Work == 1 }, nil)
			// The response is far larger than the socket buffers: with nobody
			// reading it, the handler cannot have finished.
			if f.srv.inflight.Load() != 1 {
				t.Fatal("writer finished despite a non-reading client")
			}
			if s := f.h.snapshot(); s.Work != 1 || s.Retained == 0 {
				t.Fatalf("blocked writer should hold a renderer slot and a lease: %s", s)
			}
			const grace = 500 * time.Millisecond
			r := receive(t, startShutdown(f.h, f.srv.Server, grace), "shutdown", 10*time.Second)
			if !errors.Is(r.err, context.DeadlineExceeded) || r.elapsed < grace || r.elapsed > grace+3*time.Second {
				t.Fatalf("shutdown = %+v, want DeadlineExceeded after ~%s", r, grace)
			}
			waitFor(t, "blocked handler to exit", 5*time.Second, func() bool { return f.srv.inflight.Load() == 0 }, nil)
			s := waitSnapshot(t, f.h, "idle resources", 5*time.Second, idleSnapshot.idle)
			t.Logf("%s: shutdown after %s (%v); final=%s", tc.name, r.elapsed.Round(time.Millisecond), r.err, s)
		})
	}
}

// Independent of shutdown, a write that makes no progress is aborted by the
// per-write deadline, which also releases the lease and the renderer slot while
// the cache keeps (and alone owns) the dataset.
func TestBlockedWriterWriteDeadlineReleasesRenderer(t *testing.T) {
	old := responseWriteTimeout.Load()
	responseWriteTimeout.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { responseWriteTimeout.Store(old) })
	for _, tc := range slowTargets {
		t.Run(tc.name, func(t *testing.T) {
			f := newSlowFixture(t, tc.target)
			f.dial(t, 2048)
			waitFor(t, "writer to start", 5*time.Second, func() bool { return f.srv.written.Load() > 0 }, nil)
			start := time.Now()
			waitFor(t, "write deadline to free the handler", 10*time.Second, func() bool { return f.srv.inflight.Load() == 0 }, func() string { return f.h.snapshot().String() })
			s := waitSnapshot(t, f.h, "renderer release", 5*time.Second, idleSnapshot.quiescent)
			if s.CacheEntries != 1 || s.Retained != f.h.cacheOwnedBytes() || s.Retained == 0 {
				t.Fatalf("after the write deadline only the cache should own the dataset: %s owned_by_cache=%d", s, f.h.cacheOwnedBytes())
			}
			t.Logf("%s: handler freed %s after the writer blocked; %s", tc.name, time.Since(start).Round(time.Millisecond), s)
			if err := f.h.Shutdown(f.srv.Config, 5*time.Second); err != nil {
				t.Fatalf("shutdown with no active requests: %v", err)
			}
			if s := f.h.snapshot(); !s.idle() {
				t.Fatal(s)
			}
		})
	}
}

// Many simultaneous blocked and slow writers: shutdown still ends at the
// deadline and leaves nothing behind.
func TestShutdownManyBlockedWritersReleaseEverything(t *testing.T) {
	f := newSlowFixture(t, "/?as=64500&view=detail&output=plain")
	var conns []net.Conn
	for i := 0; i < 6; i++ {
		c, _ := f.dial(t, 2048)
		conns = append(conns, c)
	}
	// MaxConcurrentWork bounds how many can be active; the rest wait or are rejected.
	waitFor(t, "writers to start", 5*time.Second, func() bool { return f.h.snapshot().Work >= 2 }, nil)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); doGet(f.srv.URL + "/neighbors") }()
	r := receive(t, startShutdown(f.h, f.srv.Server, 500*time.Millisecond), "shutdown", 10*time.Second)
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("shutdown = %+v", r)
	}
	wg.Wait()
	waitFor(t, "handlers to exit", 5*time.Second, func() bool { return f.srv.inflight.Load() == 0 }, nil)
	s := waitSnapshot(t, f.h, "idle resources", 5*time.Second, idleSnapshot.idle)
	t.Logf("%d blocked writers: shutdown after %s; final=%s", len(conns), r.elapsed.Round(time.Millisecond), s)
}
