package web

// M3.2: cache admission policy for partial results. Transient conditions
// (aggregate memory capacity, queueing, cancellation) must never become
// reusable cache entries; deterministic partials keep their M3.1 behavior.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

func (c *resultCache) skipped(reason int) int64 { return c.skips[reason].Load() }

func TestCacheAdmissionPolicyByReason(t *testing.T) {
	mk := func(s bgp.Status, r bgp.Reason) *BackendResult {
		return &BackendResult{Result: bgp.Result{Completion: bgp.Completion{Status: s, Reason: r}}}
	}
	for _, tc := range []struct {
		res  *BackendResult
		skip int // -1: cacheable
	}{
		{mk(bgp.StatusComplete, bgp.ReasonNone), -1},
		{mk(bgp.StatusPartial, bgp.ReasonMemoryLimit), -1}, // per-query limit: deterministic
		{mk(bgp.StatusPartial, bgp.ReasonResponseLimit), -1},
		{mk(bgp.StatusPartial, bgp.ReasonTimeout), -1},
		{mk(bgp.StatusPartial, bgp.ReasonConnectionClosed), -1},
		{mk(bgp.StatusPartial, bgp.ReasonInvalidJSON), -1},
		{mk(bgp.StatusPartial, bgp.ReasonBackendError), -1},
		{mk(bgp.StatusPartial, bgp.ReasonCapacity), skipCapacity},
		{mk(bgp.StatusPartial, bgp.ReasonBusy), skipTransient},
		{mk(bgp.StatusPartial, bgp.ReasonCanceled), skipTransient},
		{mk(bgp.StatusFailed, bgp.ReasonTimeout), skipFailed},
		{mk(bgp.StatusFailed, bgp.ReasonCapacity), skipFailed},
		{mk(bgp.StatusFailed, bgp.ReasonBusy), skipFailed},
	} {
		if got := skipReason(tc.res); got != tc.skip || cacheable(tc.res) != (tc.skip < 0) {
			t.Errorf("%s/%s: skipReason=%d, want %d", tc.res.Status, tc.res.Reason, got, tc.skip)
		}
	}
}

// Capacity exhaustion yields partial/capacity once, is not replayed after the
// capacity recovers, and never disturbs an existing complete entry.
func TestCapacityPartialIsNotCachedAndRecovery(t *testing.T) {
	d := datasetBytes(t)
	e := newChurnEnv(t, d, 3, 8) // room for 1.5 datasets; the cache could hold 4

	a := e.fetch(context.Background(), 1)
	if a.Status != bgp.StatusComplete || a.Cached {
		t.Fatalf("first: %+v", a.Completion)
	}
	if n, _ := e.h.cache.Stats(); n != 1 {
		t.Fatalf("complete result not cached: %d entries", n)
	}

	y := e.fetch(context.Background(), 2)
	if y.Status != bgp.StatusPartial || y.Reason != bgp.ReasonCapacity || y.Cached {
		t.Fatalf("expected partial/capacity, got %+v", y.Completion)
	}
	if strings.Contains(y.Detail, "limit of") {
		t.Fatalf("aggregate exhaustion described as a per-query limit: %q", y.Detail)
	}
	if n, _ := e.h.cache.Stats(); n != 1 {
		t.Fatalf("transient partial entered the cache: %d entries", n)
	}
	if got := e.h.cache.skipped(skipCapacity); got != 1 {
		t.Fatalf("capacity skip counter = %d", got)
	}
	if e.h.budget.denied.Load() == 0 {
		t.Fatal("aggregate denial not counted")
	}

	// The complete entry is untouched: still a hit, nothing evicted.
	again := e.fetch(context.Background(), 1)
	if !again.Cached || again.Status != bgp.StatusComplete || e.h.cache.evictions.Load() != 0 {
		t.Fatalf("complete entry disturbed: cached=%v %+v evictions=%d", again.Cached, again.Completion, e.h.cache.evictions.Load())
	}

	// Capacity recovers: every owner of dataset 1 lets go and its entry expires.
	a.lease.release()
	again.lease.release()
	y.lease.release()
	e.expireAll()
	if used := e.used(); used != 0 {
		t.Fatalf("budget not fully returned: %d", used)
	}

	// The same query now succeeds. It was NOT served from a degraded entry.
	y2 := e.fetch(context.Background(), 2)
	if y2.Status != bgp.StatusComplete || y2.Cached {
		t.Fatalf("query after recovery: cached=%v %+v", y2.Cached, y2.Completion)
	}
	y3 := e.fetch(context.Background(), 2)
	if y3.Status != bgp.StatusComplete || !y3.Cached {
		t.Fatalf("complete result not reused: cached=%v %+v", y3.Cached, y3.Completion)
	}
	y2.lease.release()
	y3.lease.release()
}

// Deterministic partials (the per-query limit) keep being cached on purpose:
// paging through one reuses it instead of re-running an expensive query.
func TestDeterministicPartialStillCached(t *testing.T) {
	lg := defaults()
	lg.CacheTTL = time.Minute
	lg.MaxRetainedBytes = 20000
	cfg, closeBackend := testConfig(lg, backendSpec{id: "a", name: "A", h: serveDoc(ribJSON(300, false))})
	defer closeBackend()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	q := asQuery(t, 1)
	r1 := h.fetch(context.Background(), h.backends[0], q)
	if r1.Status != bgp.StatusPartial || r1.Reason != bgp.ReasonMemoryLimit || r1.Cached {
		t.Fatalf("expected partial/memory_limit: %+v", r1.Completion)
	}
	r2 := h.fetch(context.Background(), h.backends[0], q)
	if !r2.Cached || r2.Status != bgp.StatusPartial || r2.Reason != bgp.ReasonMemoryLimit || len(r2.Routes) != len(r1.Routes) {
		t.Fatalf("deterministic partial not reused: cached=%v %+v", r2.Cached, r2.Completion)
	}
	r1.lease.release()
	r2.lease.release()
}

// Concurrent consumers of one in-flight query all receive the same truthful
// outcome, the result is not cached, and every lease is returned.
func TestSharedConsumersGetConsistentCapacityStatus(t *testing.T) {
	d := datasetBytes(t)
	e := newChurnEnv(t, d, 1, 8) // budget: half a dataset, so the query must be cut off
	gate := e.gate(7)
	const consumers = 4
	out := make([]*BackendResult, consumers)
	var wg sync.WaitGroup
	for i := 0; i < consumers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = e.fetch(context.Background(), 7) }()
		waitForFlightWaitersAny(t, e.h, i+1)
	}
	close(gate)
	wg.Wait()
	shared := 0
	for i, r := range out {
		if r.Status != bgp.StatusPartial || r.Reason != bgp.ReasonCapacity || len(r.Routes) != len(out[0].Routes) {
			t.Fatalf("consumer %d: %+v routes=%d", i, r.Completion, len(r.Routes))
		}
		if r.Shared {
			shared++
		}
	}
	if shared != consumers-1 {
		t.Fatalf("shared consumers = %d, want %d", shared, consumers-1)
	}
	if n, _ := e.h.cache.Stats(); n != 0 {
		t.Fatalf("transient result cached: %d", n)
	}
	for _, r := range out {
		r.lease.release()
	}
	waitSnapshot(t, e.h, "idle after shared capacity partial", 5*time.Second, idleSnapshot.idle)
	if got := e.h.cache.shared.Load(); got != consumers-1 {
		t.Fatalf("shared-flight reuse counter = %d", got)
	}
}

// A canceled query never poisons the cache: the next caller runs a fresh query.
func TestCanceledQueryDoesNotPoisonCache(t *testing.T) {
	d := datasetBytes(t)
	e := newChurnEnv(t, d, 8, 8)
	gate := e.gate(9)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *BackendResult, 1)
	go func() { done <- e.fetch(ctx, 9) }()
	waitForFlightWaitersAny(t, e.h, 1)
	cancel()
	if r := <-done; r.Reason != bgp.ReasonCanceled {
		t.Fatalf("%+v", r.Completion)
	}
	waitSnapshot(t, e.h, "abandoned query to unwind", 5*time.Second, idleSnapshot.idle)
	if n, _ := e.h.cache.Stats(); n != 0 {
		t.Fatalf("canceled query cached: %d", n)
	}
	close(gate)
	r := e.fetch(context.Background(), 9)
	if r.Status != bgp.StatusComplete || r.Cached {
		t.Fatalf("after cancellation: cached=%v %+v", r.Cached, r.Completion)
	}
	r.lease.release()
}

// Cache counters: hits, misses, expirations and LRU evictions are all counted.
func TestCacheCounters(t *testing.T) {
	d := datasetBytes(t)
	e := newChurnEnv(t, d, 20, 3) // cache holds one dataset
	c := e.h.cache
	r1 := e.fetch(context.Background(), 1)
	r2 := e.fetch(context.Background(), 1)
	if c.hits.Load() != 1 || c.misses.Load() != 1 {
		t.Fatalf("hits=%d misses=%d", c.hits.Load(), c.misses.Load())
	}
	r3 := e.fetch(context.Background(), 2) // does not fit beside entry 1: evicts it
	if c.evictions.Load() != 1 {
		t.Fatalf("evictions=%d", c.evictions.Load())
	}
	e.expireAll()
	if c.expirations.Load() < 1 {
		t.Fatalf("expirations=%d", c.expirations.Load())
	}
	for _, r := range []*BackendResult{r1, r2, r3} {
		r.lease.release()
	}
}
