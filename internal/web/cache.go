package web

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

// resultCache is a short-lived, memory-bounded cache of validated backend
// route results with single-flight de-duplication of identical queries.
//
// Properties:
//   - Only complete and partial results are stored (with their completion
//     metadata). Failed, cancelled and busy outcomes are never cached.
//   - Entries expire a fixed TTL after they were fetched (no sliding expiry) and
//     are released by a timer at that moment, not only on the next request.
//   - The sum of estimated entry sizes never exceeds maxBytes; when a new entry
//     does not fit, expired entries go first, then least-recently-used ones.
//     An entry larger than the whole budget is not stored.
//   - Cached values are shared read-only; nothing is copied.
//   - Identical concurrent queries share one upstream request. The shared query
//     is cancelled only when every waiter has gone away.
//   - Nothing is persisted: this is process memory, not a snapshot store.
//
// Admission policy (see cacheable): complete results are stored; partial
// results are stored unless they ended for a transient reason (shared memory
// capacity, queueing, cancellation), because replaying those would preserve an
// overload condition after capacity has recovered.
type resultCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	maxBytes int64
	used     int64
	items    map[string]*cacheEntry
	lru      *list.List // front = most recently used
	flights  map[string]*flight
	now      func() time.Time

	// Authoritative counters, exported by the metrics collector.
	hits, misses, shared   atomic.Int64
	evictions, expirations atomic.Int64
	skips                  [numSkipReasons]atomic.Int64
}

// Why a finished result was not stored. The set is fixed so it can label metrics.
const (
	skipDisabled  = iota // caching is off
	skipFailed           // failed result: never cached
	skipCapacity         // partial because shared memory capacity was exhausted
	skipTransient        // busy or canceled: this instance's momentary state
	skipAbandoned        // every caller left; the shared query was canceled
	skipTooLarge         // larger than the whole cache budget
	numSkipReasons
)

var skipReasonNames = [numSkipReasons]string{"disabled", "failed", "capacity", "transient", "abandoned", "too_large"}

type cacheEntry struct {
	key     string
	res     *BackendResult
	size    int64
	expires time.Time
	elem    *list.Element
	timer   *time.Timer
}

type flight struct {
	done    chan struct{}
	res     *BackendResult
	cancel  context.CancelFunc
	waiters int
}

func newResultCache(ttl time.Duration, maxBytes int64) *resultCache {
	return &resultCache{ttl: ttl, maxBytes: maxBytes, items: map[string]*cacheEntry{}, lru: list.New(), flights: map[string]*flight{}, now: time.Now}
}

func (c *resultCache) enabled() bool { return c.ttl > 0 && c.maxBytes > 0 }

// Stats reports the entry count and estimated bytes in use.
func (c *resultCache) Stats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.used
}

// snapshot is the O(1) reading used by the metrics collector.
func (c *resultCache) snapshot() (entries int, bytes int64, flights int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.used, len(c.flights)
}

// Do returns the cached result for key, or runs fn once for all concurrent
// callers. The returned result is a shallow copy marked Cached when it came
// from the cache or from another caller's in-flight query.
func (c *resultCache) Do(ctx context.Context, key string, fn func(context.Context) *BackendResult) *BackendResult {
	c.mu.Lock()
	if e := c.lookupLocked(key); e != nil {
		c.lru.MoveToFront(e.elem)
		c.hits.Add(1)
		res := *e.res
		res.lease.retain()
		c.mu.Unlock()
		res.Cached = true
		return &res
	}
	c.misses.Add(1)
	f, shared := c.flights[key]
	if shared {
		c.shared.Add(1)
	} else {
		// The query outlives any single caller; it is cancelled when none remain.
		fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &flight{done: make(chan struct{}), cancel: cancel}
		c.flights[key] = f
		go c.run(key, f, fctx, fn)
	}
	f.waiters++
	c.mu.Unlock()

	select {
	case <-f.done:
		c.mu.Lock()
		res := *f.res
		res.lease.retain()
		f.waiters--
		if f.waiters == 0 {
			f.res.lease.release()
		}
		c.mu.Unlock()
		if shared {
			res.Shared = true
		}
		return &res
	case <-ctx.Done():
		c.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			if c.flights[key] == f {
				delete(c.flights, key)
			}
			f.cancel()
			if f.res != nil {
				f.res.lease.release()
			}
		}
		c.mu.Unlock()
		return &BackendResult{Result: bgp.Result{Completion: bgp.Completion{Status: bgp.StatusFailed, Reason: bgp.ReasonCanceled, Detail: "Request cancelled"}}}
	}

}

func (c *resultCache) run(key string, f *flight, ctx context.Context, fn func(context.Context) *BackendResult) {
	res := fn(ctx)
	c.mu.Lock()
	if c.flights[key] != f || ctx.Err() != nil {
		c.skips[skipAbandoned].Add(1)
	} else if reason := skipReason(res); reason >= 0 {
		c.skips[reason].Add(1)
	} else if !c.enabled() {
		c.skips[skipDisabled].Add(1)
	} else {
		c.storeLocked(key, res)
	}
	if c.flights[key] == f {
		delete(c.flights, key)
	}
	f.cancel()
	f.res = res
	if f.waiters == 0 {
		res.lease.release()
	}
	close(f.done)
	c.mu.Unlock()
}

// skipReason classifies why a result must not enter the cache, or -1 when it
// may. The decision uses the typed completion Reason, never the message text:
//
//	complete                                      store
//	partial, deterministic reason                 store (paging reuses it):
//	    memory_limit (per-query), response_limit, timeout,
//	    connection_closed, invalid_json, backend_error
//	partial, capacity (aggregate budget)          skip: transient overload
//	partial, busy / canceled                      skip: this instance's state
//	failed                                        skip
func skipReason(r *BackendResult) int {
	switch {
	case r.Status == bgp.StatusComplete:
		return -1
	case r.Status != bgp.StatusPartial:
		return skipFailed
	case r.Reason == bgp.ReasonCapacity:
		return skipCapacity
	case r.Reason.Transient():
		return skipTransient
	}
	return -1
}

// cacheable reports whether a finished result may be stored for reuse.
func cacheable(r *BackendResult) bool { return skipReason(r) < 0 }

func (c *resultCache) lookupLocked(key string) *cacheEntry {
	e := c.items[key]
	if e == nil {
		return nil
	}
	if !c.now().Before(e.expires) {
		c.expirations.Add(1)
		c.removeLocked(e)
		return nil
	}
	return e
}

func (c *resultCache) removeLocked(e *cacheEntry) {
	if e.timer != nil {
		e.timer.Stop()
	}
	c.lru.Remove(e.elem)
	delete(c.items, e.key)
	c.used -= e.size
	e.res.lease.release()
}

func (c *resultCache) storeLocked(key string, res *BackendResult) {
	size := res.MemBytes + 1024 // route estimate plus fixed metadata overhead
	if size > c.maxBytes {
		c.skips[skipTooLarge].Add(1)
		return
	}
	if old := c.items[key]; old != nil {
		c.removeLocked(old)
	}
	now := c.now()
	for el := c.lru.Back(); el != nil; {
		prev := el.Prev()
		if e := el.Value.(*cacheEntry); !now.Before(e.expires) {
			c.expirations.Add(1)
			c.removeLocked(e)
		}
		el = prev
	}
	for c.used+size > c.maxBytes {
		el := c.lru.Back()
		if el == nil {
			return
		}
		c.evictions.Add(1)
		c.removeLocked(el.Value.(*cacheEntry))
	}
	res.lease.retain()
	e := &cacheEntry{key: key, res: res, size: size, expires: now.Add(c.ttl)}
	e.elem = c.lru.PushFront(e)
	c.items[key] = e
	c.used += size
	// Release the memory when the entry expires even if no further request
	// arrives; otherwise an idle instance would hold the last large result.
	e.timer = time.AfterFunc(c.ttl+50*time.Millisecond, c.purgeExpired)
}

// purgeExpired drops every expired entry.
func (c *resultCache) purgeExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for el := c.lru.Back(); el != nil; {
		prev := el.Prev()
		if e := el.Value.(*cacheEntry); !now.Before(e.expires) {
			c.expirations.Add(1)
			c.removeLocked(e)
		}
		el = prev
	}
}

// Close releases cache entries and cancels all shared work.
func (c *resultCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.flights {
		f.cancel()
	}
	for _, e := range c.items {
		c.removeLocked(e)
	}
}
