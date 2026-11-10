package web

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

func TestResultOwnershipAfterEviction(t *testing.T) {
	b := &resultBudget{limit: 100}
	c := newResultCache(time.Minute, 2048)
	l := b.newLease()
	if !l.reserve(80) {
		t.Fatal("reservation")
	}
	makeResult := func(context.Context) *BackendResult {
		return &BackendResult{Result: bgp.Result{Completion: bgp.Completion{Status: bgp.StatusComplete, MemBytes: 80}}, lease: l}
	}
	a := c.Do(context.Background(), "a", makeResult)
	hit := c.Do(context.Background(), "a", makeResult)
	c.Close()
	if b.used != 80 {
		t.Fatalf("eviction lost live reservation: %d", b.used)
	}
	a.lease.release()
	if b.used != 80 {
		t.Fatal("released before final consumer")
	}
	hit.lease.release()
	if b.used != 0 {
		t.Fatalf("leaked %d", b.used)
	}
}
func TestBudgetAndGateRecovery(t *testing.T) {
	b := &resultBudget{limit: 100}
	a := b.newLease()
	other := b.newLease()
	if !a.reserve(80) || other.reserve(21) {
		t.Fatal("budget not enforced")
	}
	a.release()
	if !other.reserve(100) {
		t.Fatal("capacity did not recover")
	}
	other.release()
	g := &workGate{active: make(chan struct{}, 1), queue: make(chan struct{}, 1), wait: time.Millisecond}
	if !g.acquire(context.Background()) {
		t.Fatal("first admission")
	}
	if g.acquire(context.Background()) {
		t.Fatal("over admission")
	}
	g.release()
	if !g.acquire(context.Background()) {
		t.Fatal("gate did not recover")
	}
	g.release()
}
func TestHTMXValidationStatus(t *testing.T) {
	h := newTestHandler(t, defaults(), backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	rr := get(h, "/?prefix=not-an-ip", "HX-Request", "true")
	if rr.Code != 400 || rr.Body.Len() == 0 {
		t.Fatalf("status %d, body %q", rr.Code, rr.Body.String())
	}
}

func TestExpirationKeepsConsumerReservation(t *testing.T) {
	b := &resultBudget{limit: 100}
	c := newResultCache(time.Minute, 2048)
	now := time.Now()
	c.now = func() time.Time { return now }
	lease := b.newLease()
	if !lease.reserve(50) {
		t.Fatal("reserve")
	}
	r := c.Do(context.Background(), "key", func(context.Context) *BackendResult {
		return &BackendResult{Result: bgp.Result{Completion: bgp.Completion{Status: bgp.StatusComplete, MemBytes: 50}}, lease: lease}
	})
	now = now.Add(2 * time.Minute)
	c.purgeExpired()
	if b.used != 50 {
		t.Fatalf("expired while in use: %d", b.used)
	}
	r.lease.release()
	if b.used != 0 {
		t.Fatalf("reservation leak: %d", b.used)
	}
	c.Close()
}

func TestConcurrentCacheReplacementKeepsConsumersAccounted(t *testing.T) {
	budget := &resultBudget{limit: 10000}
	cache := newResultCache(time.Minute, 4096)
	makeLease := func() *resultLease {
		l := budget.newLease()
		if !l.reserve(100) {
			t.Fatal("reserve")
		}
		return l
	}
	original := makeLease()
	first := cache.Do(context.Background(), "key", func(context.Context) *BackendResult {
		return &BackendResult{Result: bgp.Result{Completion: bgp.Completion{Status: bgp.StatusComplete, MemBytes: 100}}, lease: original}
	})
	second := cache.Do(context.Background(), "key", nil)
	const replacements = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < replacements; i++ {
		lease := makeLease()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cache.mu.Lock()
			cache.storeLocked("key", &BackendResult{Result: bgp.Result{Completion: bgp.Completion{Status: bgp.StatusComplete, MemBytes: 100}}, lease: lease})
			cache.mu.Unlock()
			lease.release()
		}()
	}
	close(start)
	wg.Wait()
	cache.Close()
	if budget.used != 100 {
		t.Fatalf("live consumers should retain 100 bytes, got %d", budget.used)
	}
	first.lease.release()
	if budget.used != 100 {
		t.Fatalf("second consumer lost reservation: %d", budget.used)
	}
	second.lease.release()
	if budget.used != 0 {
		t.Fatalf("leaked %d bytes", budget.used)
	}
}

func TestGateSaturationCancellationAndRecovery(t *testing.T) {
	stop := make(chan struct{})
	g := &workGate{active: make(chan struct{}, 1), queue: make(chan struct{}, 1), wait: 10 * time.Millisecond, stop: stop}
	if !g.acquire(context.Background()) {
		t.Fatal("first admission")
	}
	// Occupy the one waiting position explicitly, deterministically exercising
	// immediate rejection without depending on goroutine scheduling.
	g.queue <- struct{}{}
	if g.acquire(context.Background()) {
		t.Fatal("full queue admitted")
	}
	<-g.queue
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.acquire(ctx) {
		t.Fatal("canceled waiter admitted")
	}
	if g.acquire(context.Background()) {
		t.Fatal("waiter failed to time out")
	}
	g.release()
	if !g.acquire(context.Background()) {
		t.Fatal("capacity did not recover")
	}
	g.release()
	if len(g.active) != 0 || len(g.queue) != 0 {
		t.Fatal("gate leaked capacity")
	}
}

func TestRepeatedLeaseCycles(t *testing.T) {
	budget := &resultBudget{limit: 100}
	for i := 0; i < 1000; i++ {
		l := budget.newLease()
		if !l.reserve(100) {
			t.Fatalf("cycle %d reserve", i)
		}
		l.retain()
		l.release()
		l.release()
		if budget.used != 0 {
			t.Fatalf("cycle %d leaked %d", i, budget.used)
		}
	}
}
