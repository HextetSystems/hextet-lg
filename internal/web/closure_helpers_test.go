package web

// Test-only instrumentation for the M3.1 safety-closure tests. Nothing here is
// compiled into the binary or reachable over HTTP.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// idleSnapshot is one consistent-enough reading of every resource counter the
// closure tests care about.
type idleSnapshot struct {
	Decodes      int   // active route decodes holding an instance-wide expensive-query slot
	Work         int   // active admitted work (renderers and overview handlers)
	WorkWaiters  int   // requests waiting for an admission slot
	QueryQueue   int   // queries inside the upstream-query queue
	Flights      int   // active shared single-flight queries
	CacheEntries int   // entries resident in the result cache
	CacheBytes   int64 // cache-estimated bytes of those entries
	Retained     int64 // accounted retained-result memory (the aggregate budget's used)
}

func (s idleSnapshot) String() string {
	return fmt.Sprintf("decodes=%d work=%d work_waiters=%d query_queue=%d flights=%d cache_entries=%d cache_bytes=%d retained=%d",
		s.Decodes, s.Work, s.WorkWaiters, s.QueryQueue, s.Flights, s.CacheEntries, s.CacheBytes, s.Retained)
}

// quiescent: nothing is running or queued. Cache-resident memory may remain.
func (s idleSnapshot) quiescent() bool {
	return s.Decodes == 0 && s.Work == 0 && s.WorkWaiters == 0 && s.QueryQueue == 0 && s.Flights == 0
}

// idle: quiescent and every byte, including the cache, has been returned.
func (s idleSnapshot) idle() bool {
	return s.quiescent() && s.CacheEntries == 0 && s.CacheBytes == 0 && s.Retained == 0
}

func (h *Handler) snapshot() idleSnapshot {
	h.cache.mu.Lock()
	s := idleSnapshot{Flights: len(h.cache.flights), CacheEntries: len(h.cache.items), CacheBytes: h.cache.used}
	h.cache.mu.Unlock()
	s.Decodes, s.Work, s.WorkWaiters, s.QueryQueue = len(h.expensive), len(h.work.active), len(h.work.queue), len(h.queue)
	h.budget.mu.Lock()
	s.Retained = h.budget.used
	h.budget.mu.Unlock()
	return s
}

// cacheOwnedBytes sums the bytes of every distinct dataset the cache still
// references. When no request is running it must equal the accounted memory:
// the cache is then the only owner left.
func (h *Handler) cacheOwnedBytes() int64 {
	h.cache.mu.Lock()
	defer h.cache.mu.Unlock()
	seen := map[*resultLease]bool{}
	var total int64
	for _, e := range h.cache.items {
		if l := e.res.lease; l != nil && !seen[l] {
			seen[l] = true
			h.budget.mu.Lock()
			total += l.bytes
			h.budget.mu.Unlock()
		}
	}
	return total
}

// waitFor polls cond (a bounded wait on an observable condition, not a sleep
// standing in for synchronization) and fails with detail() on timeout.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool, detail func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			d := ""
			if detail != nil {
				d = ": " + detail()
			}
			t.Fatalf("timed out after %s waiting for %s%s", timeout, what, d)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitSnapshot(t *testing.T, h *Handler, what string, timeout time.Duration, ok func(idleSnapshot) bool) idleSnapshot {
	t.Helper()
	var last idleSnapshot
	waitFor(t, what, timeout, func() bool { last = h.snapshot(); return ok(last) }, func() string { return h.snapshot().String() })
	return last
}

func waitForFlightWaitersAny(t *testing.T, h *Handler, want int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("a shared flight with %d waiters", want), 5*time.Second, func() bool {
		h.cache.mu.Lock()
		defer h.cache.mu.Unlock()
		for _, f := range h.cache.flights {
			if f.waiters == want {
				return true
			}
		}
		return false
	}, nil)
}

// trackedServer is a real TCP server around the application handler that counts
// in-flight HTTP handlers and response bytes handed to the connection.
type trackedServer struct {
	*httptest.Server
	inflight atomic.Int64
	written  atomic.Int64
}

type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (w countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n.Add(int64(n))
	return n, err
}
func (w countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func newTrackedServer(h *Handler) *trackedServer {
	ts := &trackedServer{}
	routes := h.Routes()
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.inflight.Add(1)
		defer ts.inflight.Add(-1)
		routes.ServeHTTP(countingWriter{w, &ts.written}, r)
	}))
	return ts
}
