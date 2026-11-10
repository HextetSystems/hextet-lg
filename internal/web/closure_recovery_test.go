package web

// Acceptance B: after a mixed workload ends, every resource counter returns to
// its idle baseline. Nothing is force-reset: if a counter does not recover, the
// test reports who still owns the reservation.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type peaks struct {
	decodes, work, waiters, queryQueue, flights, cacheEntries atomic.Int64
	retained, cacheBytes                                      atomic.Int64
	violations                                                atomic.Int64
	firstViolation                                            atomic.Value
}

func (p *peaks) observe(s idleSnapshot, limit int64) {
	maxInt(&p.decodes, int64(s.Decodes))
	maxInt(&p.work, int64(s.Work))
	maxInt(&p.waiters, int64(s.WorkWaiters))
	maxInt(&p.queryQueue, int64(s.QueryQueue))
	maxInt(&p.flights, int64(s.Flights))
	maxInt(&p.cacheEntries, int64(s.CacheEntries))
	maxInt(&p.retained, s.Retained)
	maxInt(&p.cacheBytes, s.CacheBytes)
	if s.Retained < 0 || s.Retained > limit || s.Decodes < 0 || s.Work < 0 {
		if p.violations.Add(1) == 1 {
			p.firstViolation.Store(s.String())
		}
	}
}

func TestResourceRecoveryAfterMixedLoad(t *testing.T) {
	const (
		workers    = 40
		iterations = 14
		cacheTTL   = 600 * time.Millisecond
		budget     = 160 << 20 // one overview (128 MiB) plus a few datasets: forces contention
	)
	doc := ribJSON(1500, false)
	lg := defaults()
	lg.QueryTimeout = 20 * time.Second
	lg.CacheTTL = cacheTTL
	lg.CacheMaxBytes = 24 << 20
	lg.MaxRetainedBytes = 16 << 20
	lg.MaxTotalRetainedBytes = budget
	lg.QueueWait = 300 * time.Millisecond
	lg.PageSize = 200
	cfg, closeBackend := testConfig(lg, backendSpec{id: "a", name: "A", h: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rib" {
			io.WriteString(w, doc)
			return
		}
		io.WriteString(w, `{"neighbors":[],"summary":{}}`)
	}})
	defer closeBackend()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTrackedServer(h)
	defer srv.Close()

	before := h.snapshot()
	if !before.idle() {
		t.Fatalf("baseline not idle: %s", before)
	}

	var pk peaks
	stopSampler := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			select {
			case <-stopSampler:
				return
			default:
				pk.observe(h.snapshot(), budget)
				time.Sleep(500 * time.Microsecond)
			}
		}
	}()

	var (
		statusMu sync.Mutex
		statuses = map[int]int{}
		canceled atomic.Int64
	)
	record := func(code int) { statusMu.Lock(); statuses[code]++; statusMu.Unlock() }
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	drain := func(res *http.Response, slow bool) {
		defer res.Body.Close()
		buf := make([]byte, 32<<10)
		for {
			_, err := res.Body.Read(buf)
			if err != nil {
				return
			}
			if slow {
				time.Sleep(time.Millisecond)
			}
		}
	}
	var distinct atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				var target string
				cancelAfterBytes, cancelAfter := 0, time.Duration(0)
				slow := false
				switch (w + i) % 9 {
				case 0, 1: // shared plaintext query
					target = "/?as=64500&view=text&output=plain"
				case 2: // cached HTML (the shared query again, table view)
					target = "/?as=64500&view=table"
				case 3: // distinct query, plaintext
					target = fmt.Sprintf("/?as=%d&view=text&output=plain", 65000+int(distinct.Add(1)))
				case 4: // distinct query, HTML, slow reader
					target = fmt.Sprintf("/?as=%d&view=table", 65000+int(distinct.Add(1)))
					slow = true
				case 5:
					target = "/summary"
				case 6:
					target = "/neighbors"
				case 7: // client that disconnects after the first bytes
					target = "/?as=64500&view=text&output=plain"
					cancelAfterBytes = 1
				case 8: // client that gives up almost immediately on a distinct query
					target = fmt.Sprintf("/?as=%d&view=table", 65000+int(distinct.Add(1)))
					cancelAfter = 3 * time.Millisecond
				}
				if cancelAfter > 0 {
					time.AfterFunc(cancelAfter, cancel)
				}
				req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+target, nil)
				res, err := client.Do(req)
				switch {
				case err != nil:
					canceled.Add(1)
				case cancelAfterBytes > 0:
					buf := make([]byte, cancelAfterBytes)
					io.ReadFull(res.Body, buf)
					cancel()
					res.Body.Close()
					canceled.Add(1)
					record(res.StatusCode)
				default:
					record(res.StatusCode)
					drain(res, slow)
				}
				cancel()
			}
		}(w)
	}
	wg.Wait()
	// Stop generating requests. Everything below is observation only.
	atWorkloadEnd := h.snapshot()

	// 1. Active requests finish or cancel: no decode, renderer, waiter or flight remains.
	quiet := waitSnapshot(t, h, "all active work to drain", 20*time.Second, idleSnapshot.quiescent)

	// 2. With nothing running, the cache must be the only owner of retained memory.
	waitFor(t, "ownership to settle on the cache", 5*time.Second, func() bool {
		// Retained must equal what the cache's datasets account for; an
		// unexplained remainder would be a leaked reservation.
		return h.snapshot().Retained == h.cacheOwnedBytes()
	}, func() string {
		return fmt.Sprintf("%s owned_by_cache=%d", h.snapshot(), h.cacheOwnedBytes())
	})

	// 3. After the TTL and the expiry timer, the cache has released everything.
	expiryStart := time.Now()
	final := waitSnapshot(t, h, "cache expiry to release all memory", cacheTTL+10*time.Second, idleSnapshot.idle)
	expiry := time.Since(expiryStart)
	close(stopSampler)
	<-samplerDone

	if n := pk.violations.Load(); n != 0 {
		t.Fatalf("%d invariant violations (retained outside [0, limit]); first: %v", n, pk.firstViolation.Load())
	}
	if !final.idle() {
		t.Fatalf("did not return to baseline (owner investigation): %s", final)
	}
	// The workload must have actually exercised the machinery, or "returned to
	// baseline" proves nothing.
	if pk.decodes.Load() < 1 || pk.work.Load() < 2 || pk.retained.Load() == 0 || pk.flights.Load() < 1 || pk.cacheEntries.Load() < 1 {
		t.Fatalf("workload too weak to prove recovery: %+v", &pk)
	}
	statusMu.Lock()
	defer statusMu.Unlock()
	for code := range statuses {
		if code != 200 && code != 503 {
			t.Fatalf("unexpected status %d in %v", code, statuses)
		}
	}
	t.Logf("requests: statuses=%v client_cancellations=%d distinct_queries=%d", statuses, canceled.Load(), distinct.Load())
	t.Logf("peaks:    decodes=%d work=%d work_waiters=%d query_queue=%d flights=%d cache_entries=%d cache_bytes=%d retained=%d (limit %d)",
		pk.decodes.Load(), pk.work.Load(), pk.waiters.Load(), pk.queryQueue.Load(), pk.flights.Load(), pk.cacheEntries.Load(), pk.cacheBytes.Load(), pk.retained.Load(), int64(budget))
	t.Logf("before:   %s", before)
	t.Logf("workload end:    %s", atWorkloadEnd)
	t.Logf("quiescent (cache not yet expired; retained == cache-owned): %s", quiet)
	t.Logf("after TTL+cleanup (%s later): %s", expiry.Round(time.Millisecond), final)
}
