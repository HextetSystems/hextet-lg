package web

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

func result(status bgp.Status, reason bgp.Reason, mem int64) *BackendResult {
	return &BackendResult{FetchedAt: time.Now(), Result: bgp.Result{Routes: []bgp.Route{{Prefix: "x"}}, Completion: bgp.Completion{Status: status, Reason: reason, MemBytes: mem}}}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClockCache(ttl time.Duration, max int64) (*resultCache, *clock) {
	ck := &clock{t: time.Unix(1_700_000_000, 0)}
	c := newResultCache(ttl, max)
	c.now = ck.now
	return c, ck
}

func TestCacheHitExpiryAndMetadata(t *testing.T) {
	c, ck := newClockCache(20*time.Second, 1<<20)
	var calls int
	fn := func(context.Context) *BackendResult {
		calls++
		return result(bgp.StatusPartial, bgp.ReasonTimeout, 100)
	}
	first := c.Do(context.Background(), "k", fn)
	if first.Cached || calls != 1 {
		t.Fatal("first call is a live query")
	}
	ck.advance(19 * time.Second)
	hit := c.Do(context.Background(), "k", fn)
	if !hit.Cached || calls != 1 || hit.Status != bgp.StatusPartial || hit.Reason != bgp.ReasonTimeout {
		t.Fatalf("expected cached partial with its completion metadata: %+v calls=%d", hit.Completion, calls)
	}
	// Hard TTL from fetch time: hits do not extend the lifetime.
	ck.advance(2 * time.Second)
	if c.Do(context.Background(), "k", fn); calls != 2 {
		t.Fatalf("entry must expire %v after fetch, calls=%d", 20*time.Second, calls)
	}
	if n, b := c.Stats(); n != 1 || b == 0 {
		t.Fatalf("stats %d %d", n, b)
	}
}

func TestCacheNeverStoresFailuresOrCancellations(t *testing.T) {
	c, _ := newClockCache(time.Minute, 1<<20)
	for _, r := range []*BackendResult{
		result(bgp.StatusFailed, bgp.ReasonBackendError, 0), result(bgp.StatusFailed, bgp.ReasonTimeout, 0),
		result(bgp.StatusFailed, bgp.ReasonBusy, 0), result(bgp.StatusPartial, bgp.ReasonCanceled, 10),
	} {
		var calls int
		fn := func(context.Context) *BackendResult { calls++; return r }
		c.Do(context.Background(), "k", fn)
		c.Do(context.Background(), "k", fn)
		if calls != 2 {
			t.Errorf("%s/%s was cached", r.Status, r.Reason)
		}
	}
	if n, _ := c.Stats(); n != 0 {
		t.Fatalf("%d entries", n)
	}
}

func TestCacheMemoryBudgetAndPredictableEviction(t *testing.T) {
	c, _ := newClockCache(time.Minute, 3000)
	put := func(key string, mem int64) {
		c.Do(context.Background(), key, func(context.Context) *BackendResult { return result(bgp.StatusComplete, "", mem) })
	}
	has := func(key string) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.items[key] != nil
	}
	put("a", 900) // each entry costs mem+1024
	put("b", 900)
	if n, b := c.Stats(); n != 1 || b != 1924 {
		// 2 entries of 1924 exceed the 3000 budget, so "a" (LRU) was evicted.
		t.Fatalf("stats %d %d", n, b)
	}
	if has("a") || !has("b") {
		t.Fatal("least recently used entry must be evicted first")
	}
	put("c", 100) // 1124: fits beside b? 1924+1124 > 3000 -> evict b
	if has("b") || !has("c") {
		t.Fatal("eviction order")
	}
	if _, used := c.Stats(); used > 3000 {
		t.Fatalf("budget exceeded: %d", used)
	}
	put("huge", 5000) // larger than the whole budget: not stored, nothing evicted
	if has("huge") || !has("c") {
		t.Fatal("oversized entries must not be cached or flush the cache")
	}
}

// An expired entry is reclaimed before a still-valid, less recently used one.
func TestCacheExpiredEntriesGoBeforeLiveOnes(t *testing.T) {
	c, ck := newClockCache(60*time.Second, 4000)
	put := func(key string, mem int64) {
		c.Do(context.Background(), key, func(context.Context) *BackendResult { return result(bgp.StatusComplete, "", mem) })
	}
	has := func(key string) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.items[key] != nil
	}
	put("x", 900) // t=0, expires t=60
	ck.advance(10 * time.Second)
	put("y", 900) // t=10, expires t=70
	ck.advance(10 * time.Second)
	put("x", 900)                // hit: x becomes most recently used, but keeps its t=60 expiry
	ck.advance(45 * time.Second) // t=65: x expired (and MRU), y alive (and LRU)
	put("z", 100)
	if has("x") || !has("y") || !has("z") {
		t.Fatalf("expired x must be reclaimed, live y kept: x=%v y=%v z=%v", has("x"), has("y"), has("z"))
	}
}

func waitForFlightWaiters(t *testing.T, c *resultCache, key string, want int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		f := c.flights[key]
		ready := f != nil && f.waiters == want
		c.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("flight %q did not reach %d waiters", key, want)
		default:
			runtime.Gosched()
		}
	}
}

func TestCacheDisabledStillDeduplicates(t *testing.T) {
	c := newResultCache(0, 0)
	var calls atomic.Int32
	gate := make(chan struct{})
	fn := func(context.Context) *BackendResult { calls.Add(1); <-gate; return result(bgp.StatusComplete, "", 10) }
	var wg sync.WaitGroup
	var shared atomic.Int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := c.Do(context.Background(), "k", fn); r.Shared {
				shared.Add(1)
			}
		}()
	}
	waitForFlightWaiters(t, c, "k", 5)
	close(gate)
	wg.Wait()
	if calls.Load() != 1 || shared.Load() != 4 {
		t.Fatalf("calls=%d shared=%d", calls.Load(), shared.Load())
	}
	c.Do(context.Background(), "k", func(context.Context) *BackendResult { calls.Add(1); return result(bgp.StatusComplete, "", 10) })
	if calls.Load() != 2 {
		t.Fatal("a disabled cache must not retain results")
	}
}

func TestSingleFlightSurvivesOneCallerLeaving(t *testing.T) {
	c := newResultCache(0, 0)
	started := make(chan struct{})
	gate := make(chan struct{})
	var cancelled atomic.Bool
	fn := func(ctx context.Context) *BackendResult {
		close(started)
		select {
		case <-gate:
		case <-ctx.Done():
			cancelled.Store(true)
		}
		return result(bgp.StatusComplete, "", 10)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	resB := make(chan *BackendResult, 1)
	resA := make(chan *BackendResult, 1)
	go func() { resA <- c.Do(ctxA, "k", fn) }()
	<-started
	go func() { resB <- c.Do(context.Background(), "k", fn) }()
	waitForFlightWaiters(t, c, "k", 2)
	cancelA() // the first caller disconnects; the second still wants the answer
	if a := <-resA; a.Reason != bgp.ReasonCanceled || a.Status != bgp.StatusFailed {
		t.Fatalf("leaving caller: %+v", a.Completion)
	}
	if cancelled.Load() {
		t.Fatal("shared query was cancelled while a caller still waited")
	}
	close(gate)
	if b := <-resB; !b.IsComplete() {
		t.Fatalf("remaining caller: %+v", b.Completion)
	}
}

func TestSingleFlightCancelledWhenAllCallersLeave(t *testing.T) {
	c := newResultCache(time.Minute, 1<<20)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var calls atomic.Int32
	fn := func(ctx context.Context) *BackendResult {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return result(bgp.StatusFailed, bgp.ReasonCanceled, 0)
		}
		return result(bgp.StatusComplete, "", 10)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Do(ctx, "k", fn); close(done) }()
	<-started
	cancel()
	<-done
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream query must be cancelled when nobody is waiting")
	}
	// A later identical request starts a fresh query rather than joining the dead one.
	if r := c.Do(context.Background(), "k", fn); !r.IsComplete() || r.Shared {
		t.Fatalf("%+v", r.Completion)
	}
}

func TestCacheReleasesExpiredEntriesWithoutFurtherRequests(t *testing.T) {
	c := newResultCache(80*time.Millisecond, 1<<20)
	c.Do(context.Background(), "k", func(context.Context) *BackendResult { return result(bgp.StatusComplete, "", 5000) })
	if n, _ := c.Stats(); n != 1 {
		t.Fatal("entry not stored")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, b := c.Stats(); n == 0 && b == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expired entry stayed in memory on an idle cache")
}

func TestPurgeExpired(t *testing.T) {
	c, ck := newClockCache(time.Minute, 1<<20)
	c.Do(context.Background(), "old", func(context.Context) *BackendResult { return result(bgp.StatusComplete, "", 10) })
	ck.advance(40 * time.Second)
	c.Do(context.Background(), "new", func(context.Context) *BackendResult { return result(bgp.StatusComplete, "", 10) })
	ck.advance(30 * time.Second)
	c.purgeExpired()
	if n, _ := c.Stats(); n != 1 || c.items["new"] == nil {
		t.Fatalf("entries=%d", n)
	}
}
