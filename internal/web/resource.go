package web

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// resultBudget counts a dataset once, until its final explicit owner releases it.
type resultBudget struct {
	mu          sync.Mutex
	used, limit int64
	// denied counts reservations refused for lack of aggregate capacity.
	denied atomic.Int64
}

// usage returns the accounted bytes and the configured aggregate limit.
func (b *resultBudget) usage() (used, limit int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.limit
}

type resultLease struct {
	budget *resultBudget
	refs   int
	bytes  int64
}

func (b *resultBudget) newLease() *resultLease { return &resultLease{budget: b, refs: 1} }
func (l *resultLease) reserve(n int64) bool {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.limit-b.used {
		b.denied.Add(1)
		return false
	}
	b.used += n
	l.bytes += n
	return true
}
func (l *resultLease) retain() {
	if l == nil {
		return
	}
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.refs <= 0 {
		panic("retain released result")
	}
	l.refs++
}
func (l *resultLease) release() {
	if l == nil {
		return
	}
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.refs <= 0 {
		panic("release released result")
	}
	l.refs--
	if l.refs == 0 {
		b.used -= l.bytes
		l.bytes = 0
	}
}

// workGate bounds both active consumers and waiters. Slots are held through
// response completion, including slow clients and references to evicted results.
type workGate struct {
	active, queue chan struct{}
	wait          time.Duration
	stop          <-chan struct{}
	rejected      [numGateReasons]atomic.Int64
}

// Why the work gate refused a request. The set is fixed so it can label metrics.
const (
	gateQueueFull = iota
	gateTimeout
	gateCanceled
	gateShutdown
	numGateReasons
)

var gateReasonNames = [numGateReasons]string{"queue_full", "wait_timeout", "canceled", "shutdown"}

func (g *workGate) reject(reason int) bool { g.rejected[reason].Add(1); return false }

func (g *workGate) acquire(ctx context.Context) bool {
	select {
	case g.queue <- struct{}{}:
	default:
		return g.reject(gateQueueFull)
	}
	defer func() { <-g.queue }()
	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	select {
	case g.active <- struct{}{}:
		return true
	case <-ctx.Done():
		return g.reject(gateCanceled)
	case <-g.stop:
		return g.reject(gateShutdown)
	case <-timer.C:
		return g.reject(gateTimeout)
	}
}
func (g *workGate) release() { <-g.active }
