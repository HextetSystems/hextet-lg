package web

// Acceptance C: concurrent cache churn against the aggregate memory budget.
// These tests drive the real Handler (budget + work gates + single-flight cache
// + upstream client) through h.fetch, not an isolated counter.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

type churnEnv struct {
	t      *testing.T
	h      *Handler
	limit  int64
	offset atomic.Int64 // fake clock offset, to expire cache entries on demand
	base   time.Time
	gatesM sync.Mutex
	gates  map[string]chan struct{}
}

const churnRoutes = 300

// datasetBytes measures the accounted size of one complete dataset by running
// it once through a throwaway handler.
func datasetBytes(t *testing.T) int64 {
	t.Helper()
	cfg, closeBackend := testConfig(defaults(), backendSpec{id: "a", name: "A", h: serveDoc(ribJSON(churnRoutes, false))})
	defer closeBackend()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	r := h.fetch(context.Background(), h.backends[0], asQuery(t, 1))
	defer r.lease.release()
	if r.Status != bgp.StatusComplete {
		t.Fatalf("calibration: %+v", r.Completion)
	}
	return r.lease.bytes
}

func asQuery(t *testing.T, as int) bgp.Query {
	t.Helper()
	q, err := bgp.ParseQuery(url.Values{"as": {strconv.Itoa(as)}})
	if err != nil {
		t.Fatal(err)
	}
	if q, err = q.Normalize(); err != nil {
		t.Fatal(err)
	}
	return q
}

// newChurnEnv builds a handler whose aggregate budget holds budgetDatasets
// datasets (plus half of one) and whose cache holds cacheDatasets entries.
func newChurnEnv(t *testing.T, d int64, budgetUnits2, cacheUnits2 int64) *churnEnv {
	t.Helper()
	e := &churnEnv{t: t, limit: d * budgetUnits2 / 2, base: time.Unix(1_700_000_000, 0), gates: map[string]chan struct{}{}}
	lg := defaults()
	lg.CacheTTL = time.Minute
	lg.CacheMaxBytes = d * cacheUnits2 / 2
	lg.MaxTotalRetainedBytes = e.limit
	lg.MaxRetainedBytes = 16 << 20
	lg.QueueWait = 2 * time.Second
	lg.MaxConcurrentQueries = 2
	lg.MaxQueuedQueries = 64
	doc := ribJSON(churnRoutes, false)
	cfg, closeBackend := testConfig(lg, backendSpec{id: "a", name: "A", h: func(w http.ResponseWriter, r *http.Request) {
		e.gatesM.Lock()
		g := e.gates[r.URL.Query().Get("as")]
		e.gatesM.Unlock()
		if g != nil {
			select {
			case <-g:
			case <-r.Context().Done():
				return
			}
		}
		io.WriteString(w, doc)
	}})
	t.Cleanup(closeBackend)
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	h.cache.now = func() time.Time { return e.base.Add(time.Duration(e.offset.Load())) }
	e.h = h
	return e
}

func (e *churnEnv) gate(as int) chan struct{} {
	e.gatesM.Lock()
	defer e.gatesM.Unlock()
	g := make(chan struct{})
	e.gates[strconv.Itoa(as)] = g
	return g
}

func (e *churnEnv) fetch(ctx context.Context, as int) *BackendResult {
	r := e.h.fetch(ctx, e.h.backends[0], asQuery(e.t, as))
	return &r
}

func (e *churnEnv) expireAll() {
	e.offset.Add(int64(2 * time.Minute))
	e.h.cache.purgeExpired()
}

func (e *churnEnv) used() int64 {
	e.h.budget.mu.Lock()
	defer e.h.budget.mu.Unlock()
	return e.h.budget.used
}

func leaseRefs(h *Handler, l *resultLease) int {
	h.budget.mu.Lock()
	defer h.budget.mu.Unlock()
	return l.refs
}

// ownedBytes is the memory every distinct dataset still referenced by the cache
// or by the given consumers accounts for. Accounted memory must equal it
// exactly: more means a leaked reservation, less means a premature release.
func (e *churnEnv) ownedBytes(consumers ...*BackendResult) int64 {
	h := e.h
	seen := map[*resultLease]bool{}
	var total int64
	add := func(l *resultLease) {
		if l == nil || seen[l] {
			return
		}
		seen[l] = true
		h.budget.mu.Lock()
		total += l.bytes
		h.budget.mu.Unlock()
	}
	h.cache.mu.Lock()
	for _, it := range h.cache.items {
		add(it.res.lease)
	}
	h.cache.mu.Unlock()
	for _, c := range consumers {
		add(c.lease)
	}
	return total
}

func (e *churnEnv) assertAccounted(what string, consumers ...*BackendResult) {
	e.t.Helper()
	// Cache entries can only change under the cache lock; hold it so the cache
	// contents and the budget are read as one state.
	if got, want := e.used(), e.ownedBytes(consumers...); got != want {
		e.t.Fatalf("%s: accounted %d bytes but owners hold %d (leak if accounted>owned, premature release if less)", what, got, want)
	}
	if u := e.used(); u < 0 || u > e.limit {
		e.t.Fatalf("%s: accounted %d outside [0, %d]", what, u, e.limit)
	}
}

// The deterministic ownership story: one dataset shared by several consumers is
// counted once, survives eviction and expiry while referenced, blocks new
// reservations beyond the aggregate budget, and frees capacity only when its
// last consumer lets go.
func TestChurnEvictedDatasetStaysAccountedUntilLastConsumer(t *testing.T) {
	d := datasetBytes(t)
	e := newChurnEnv(t, d, 5, 4) // budget 2.5 datasets, cache holds 2
	h := e.h

	// 1. Four callers share one in-flight query: one upstream request, one
	// dataset, accounted once.
	gate := e.gate(1)
	results := make([]*BackendResult, 4)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = e.fetch(context.Background(), 1) }()
	}
	waitForFlightWaitersAny(t, h, 4)
	close(gate)
	wg.Wait()
	a := results[0].lease
	for i, r := range results {
		if r.Status != bgp.StatusComplete || r.lease != a {
			t.Fatalf("consumer %d did not share the dataset: %+v", i, r.Completion)
		}
	}
	if got := e.used(); got != d {
		t.Fatalf("one dataset shared by 4 consumers accounted %d, want exactly %d", got, d)
	}
	// 4 consumers + the cache entry; the flight's own reference is gone.
	if refs := leaseRefs(h, a); refs != 5 {
		t.Fatalf("lease refs = %d, want 4 consumers + 1 cache entry", refs)
	}

	// 2. Expire it while the consumers are still using it.
	e.expireAll()
	if n, _ := h.cache.Stats(); n != 0 {
		t.Fatal("expired entry still cached")
	}
	if got := e.used(); got != d {
		t.Fatalf("expiry released a referenced dataset: used=%d want %d", got, d)
	}
	if refs := leaseRefs(h, a); refs != 4 {
		t.Fatalf("after expiry refs = %d, want the 4 consumers", refs)
	}

	// 3. A new query for the same key cannot reuse the evicted dataset (it is
	// gone from the cache), so a second copy is accounted alongside the first.
	b := e.fetch(context.Background(), 1)
	if b.Status != bgp.StatusComplete || b.lease == a || b.Cached {
		t.Fatalf("expected a fresh second dataset: %+v cached=%v", b.Completion, b.Cached)
	}
	if got := e.used(); got != 2*d {
		t.Fatalf("evicted+fresh datasets accounted %d, want %d", got, 2*d)
	}

	// 4. Only half a dataset of budget remains: the next query is cut off and
	// marked, never silently truncated, and the budget is never exceeded.
	y := e.fetch(context.Background(), 2)
	if y.Status == bgp.StatusComplete {
		t.Fatalf("query exceeding the aggregate budget reported complete (%d routes)", len(y.Routes))
	}
	if y.Reason != bgp.ReasonCapacity && y.Reason != bgp.ReasonBusy {
		t.Fatalf("unmarked rejection: %+v", y.Completion)
	}
	if y.Status == bgp.StatusPartial && (y.Detail == "" || len(y.Routes) == 0 || len(y.Routes) >= churnRoutes) {
		t.Fatalf("partial result not clearly marked: %+v routes=%d", y.Completion, len(y.Routes))
	}
	if got := e.used(); got > e.limit {
		t.Fatalf("budget exceeded: %d > %d", got, e.limit)
	}
	e.assertAccounted("after over-budget query", append(results, b, y)...)
	// With the budget now essentially full, another query cannot even start.
	z := e.fetch(context.Background(), 3)
	if z.Status == bgp.StatusComplete || (z.Reason != bgp.ReasonBusy && z.Reason != bgp.ReasonCapacity) {
		t.Fatalf("exhausted budget admitted a query: %+v", z.Completion)
	}
	if got := e.used(); got > e.limit {
		t.Fatalf("budget exceeded: %d > %d", got, e.limit)
	}
	t.Logf("budget=%d dataset=%d used=%d; over-budget query: status=%s reason=%s routes=%d/%d; next: status=%s reason=%s",
		e.limit, d, e.used(), y.Status, y.Reason, len(y.Routes), churnRoutes, z.Status, z.Reason)

	// 5. Consumers of the evicted dataset leave one by one. Its memory stays
	// accounted until the last reference goes.
	for i := 0; i < 3; i++ {
		results[i].lease.release()
		results[i] = &BackendResult{} // never release twice
		if got := e.used(); got != e.ownedBytes(append(results[3:], b, y, z)...) {
			t.Fatalf("after consumer %d left: accounted %d, owners hold %d", i, got, e.ownedBytes(append(results[3:], b, y, z)...))
		}
		if refs := leaseRefs(h, a); refs != 3-i {
			t.Fatalf("after consumer %d left refs=%d want %d", i, refs, 3-i)
		}
	}
	before := e.used()
	if before < d {
		t.Fatalf("dataset released before its last consumer: used=%d", before)
	}
	results[3].lease.release()
	results[3] = &BackendResult{}
	if after := e.used(); after != before-d {
		t.Fatalf("last consumer released %d bytes, want exactly the dataset's %d", before-after, d)
	}

	// 6. Capacity is available again.
	y.lease.release()
	z.lease.release()
	again := e.fetch(context.Background(), 4)
	if again.Status != bgp.StatusComplete {
		t.Fatalf("capacity did not recover: %+v", again.Completion)
	}
	again.lease.release()
	b.lease.release()
	h.cache.Close()
	if got := e.used(); got != 0 {
		t.Fatalf("leaked %d bytes after every owner released", got)
	}
}

// Many goroutines churn the cache (insertions, LRU eviction, expiry, canceled
// callers, partial results) while others hold evicted datasets. At any instant
// the accounted memory stays within the budget, and when the dust settles it
// equals exactly what live owners hold.
func TestChurnConcurrentReservationsNeverExceedBudget(t *testing.T) {
	d := datasetBytes(t)
	e := newChurnEnv(t, d, 7, 4) // budget 3.5 datasets, cache holds 2
	h := e.h

	var (
		violations atomic.Int64
		first      atomic.Value
		peak       atomic.Int64
		stopMon    = make(chan struct{})
		monDone    = make(chan struct{})
	)
	go func() {
		defer close(monDone)
		for {
			select {
			case <-stopMon:
				return
			default:
			}
			u := e.used()
			maxInt(&peak, u)
			if u < 0 || u > e.limit {
				if violations.Add(1) == 1 {
					first.Store(u)
				}
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	const (
		fetchers = 24
		rounds   = 6
		keys     = 8
	)
	var (
		heldMu sync.Mutex
		held   []*BackendResult
		wg     sync.WaitGroup
		start  = make(chan struct{})
		stats  sync.Map // status/reason -> count
	)
	count := func(r *BackendResult) {
		k := string(r.Status) + "/" + string(r.Reason)
		v, _ := stats.LoadOrStore(k, new(atomic.Int64))
		v.(*atomic.Int64).Add(1)
	}
	for g := 0; g < fetchers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				if (g+i)%5 == 0 {
					cancel() // a consumer that is canceled before/while joining
				}
				r := e.fetch(ctx, 100+(g+i)%keys)
				cancel()
				count(r)
				if (g+i)%6 == 0 {
					heldMu.Lock()
					held = append(held, r) // keep some alive across evictions
					heldMu.Unlock()
				} else {
					r.lease.release()
				}
			}
		}()
	}
	for p := 0; p < 3; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 20; i++ {
				e.expireAll() // concurrent expiration
				time.Sleep(time.Millisecond)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(stopMon)
	<-monDone
	if n := violations.Load(); n != 0 {
		t.Fatalf("%d budget violations; first observed %v (limit %d)", n, first.Load(), e.limit)
	}

	// Settled: no request is running, so owners are the cache plus held results.
	// Abandoned (all callers canceled) upstream queries unwind asynchronously
	// after their last caller returns; wait for them to release their slots.
	snap := waitSnapshot(t, h, "abandoned queries to unwind", 10*time.Second, idleSnapshot.quiescent)
	e.assertAccounted("after concurrent churn", held...)
	t.Logf("churn: peak accounted=%d of %d (dataset=%d); held=%d; settled=%s", peak.Load(), e.limit, d, len(held), snap)
	outcomes := map[string]int64{}
	stats.Range(func(k, v any) bool {
		outcomes[k.(string)] = v.(*atomic.Int64).Load()
		t.Logf("  outcome %-24v %d", k, v.(*atomic.Int64).Load())
		return true
	})
	// Non-vacuous: the budget was actually contended (some queries completed,
	// some were cut off or refused) and the peak came close to the limit.
	if outcomes["complete/"] == 0 || outcomes["partial/capacity"]+outcomes["failed/busy"] == 0 || peak.Load() < e.limit/2 {
		t.Fatalf("churn did not contend for the budget: outcomes=%v peak=%d limit=%d", outcomes, peak.Load(), e.limit)
	}

	// Release the held consumers in an arbitrary order, concurrently: each
	// release is exactly one reference, none may panic (double release) or drive
	// the counter negative, and the cache expiry races with them.
	var rel sync.WaitGroup
	for _, r := range held {
		rel.Add(1)
		go func() { defer rel.Done(); r.lease.release() }()
	}
	rel.Add(1)
	go func() { defer rel.Done(); e.expireAll() }()
	rel.Wait()
	e.expireAll()
	e.assertAccounted("after releasing every consumer")
	if got := e.used(); got != 0 {
		t.Fatalf("leaked %d bytes (cache entries=%d)", got, len(h.cache.items))
	}
	if r := e.fetch(context.Background(), 200); r.Status != bgp.StatusComplete {
		t.Fatalf("capacity did not return: %+v", r.Completion)
	} else {
		r.lease.release()
	}
	e.expireAll()
	if got := e.used(); got != 0 {
		t.Fatal(fmt.Sprintf("leaked %d bytes at the end", got))
	}
}
