package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

// BackendResult is the presentation-side outcome of one backend. Completion
// (status, reason, counts, timings) is tracked independently per backend.
type BackendResult struct {
	ID, Name, Location string
	bgp.Result
	// FetchedAt is when the upstream query finished. Cached is true when the
	// result was served from the short-lived cache and Shared when it came from
	// another request's identical in-flight query.
	FetchedAt time.Time
	Cached    bool
	Shared    bool
	lease     *resultLease

	// Overview pages (neighbors and rtr) use these.
	Error          string
	SummaryError   string
	Duration       string
	Overview       bgp.Overview
	SessionSummary bgp.Overview
	// Neighbors is Overview.Neighbors with presentation; ShowCounters says
	// whether any neighbor carries stats (otherwise those columns are omitted).
	Neighbors    []NeighborRow
	ShowCounters bool
	// RTR is the /rtr answer; RTRRows its presentation (empty unless the
	// state is bgp.RTRSessions).
	RTR         bgp.RTRResult
	RTRRows     []RTRRow
	RTRAnyError bool
	// RTRBadge and RTRBadgeClass label the backend's RTR capability state.
	RTRBadge, RTRBadgeClass string
}

// Age is how old a cached result is.
func (r BackendResult) Age() time.Duration {
	if r.FetchedAt.IsZero() {
		return 0
	}
	return time.Since(r.FetchedAt)
}

// Summary aggregates per-backend completion without merging it.
type Summary struct {
	Status                    bgp.Status
	Backends                  int
	Complete, Partial, Failed int
	Routes                    int
}

func summarize(results []BackendResult) Summary {
	s := Summary{Backends: len(results)}
	for _, r := range results {
		switch r.Status {
		case bgp.StatusComplete:
			s.Complete++
		case bgp.StatusPartial:
			s.Partial++
		default:
			s.Failed++
		}
		s.Routes += len(r.Routes)
	}
	switch {
	case s.Backends > 0 && s.Complete == s.Backends:
		s.Status = bgp.StatusComplete
	case s.Complete+s.Partial == 0:
		s.Status = bgp.StatusFailed
	default:
		s.Status = bgp.StatusPartial
	}
	return s
}

// Incomplete reports whether any selected backend did not complete.
func (s Summary) Incomplete() bool { return s.Status != bgp.StatusComplete }

func (h *Handler) selectBackends(selected string) []backendClient {
	if selected == "all" {
		return h.backends
	}
	for _, b := range h.backends {
		if b.ID == selected {
			return []backendClient{b}
		}
	}
	return nil
}

// search runs q on the selected backends concurrently (bounded per request by
// max_concurrent_backends). Every backend yields a result; one failing backend
// never discards the others.
func (h *Handler) search(ctx context.Context, selected string, q bgp.Query) ([]BackendResult, error) {
	targets := h.selectBackends(selected)
	if len(targets) == 0 {
		return nil, fmt.Errorf("select an enabled backend")
	}
	out := make([]BackendResult, len(targets))
	sem := make(chan struct{}, h.cfg.LookingGlass.MaxConcurrentBackends)
	var wg sync.WaitGroup
	for i, b := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out[i] = canceledResult(b)
				return
			}
			out[i] = h.fetch(ctx, b, q)
		}()
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func canceledResult(b backendClient) BackendResult {
	return BackendResult{ID: b.ID, Name: b.Name, Location: b.Location, Result: bgp.Result{Completion: bgp.Completion{Status: bgp.StatusFailed, Reason: bgp.ReasonCanceled, Detail: "Request cancelled"}}}
}

// fetch returns the backend's result for q from the cache, a shared in-flight
// query, or a new upstream query.
func (h *Handler) fetch(ctx context.Context, b backendClient, q bgp.Query) BackendResult {
	key := b.ID + "|" + q.Key()
	res := h.cache.Do(ctx, key, func(ctx context.Context) *BackendResult { return h.query(ctx, b, q) })
	r := *res
	r.ID, r.Name, r.Location = b.ID, b.Name, b.Location
	return r
}

// query performs one upstream query. Expensive queries (those without a
// prefix) take one of max_concurrent_queries slots shared by the whole
// instance, which also bounds decoded-route memory. A waiting query gives up
// after half of its timeout. Queries are never retried.
func (h *Handler) query(ctx context.Context, b backendClient, q bgp.Query) *BackendResult {
	res := &BackendResult{ID: b.ID, Name: b.Name, Location: b.Location}
	op := operationName("/rib")
	reject := func(metric string, reason bgp.Reason, detail string) *BackendResult {
		res.Result.Completion = bgp.Completion{Status: bgp.StatusFailed, Reason: reason, Detail: detail}
		h.metrics.queryRejected.WithLabelValues(metric).Inc()
		h.log.LogAttrs(ctx, slog.LevelDebug, "query not started", slog.String("request_id", requestID(ctx)), slog.String("backend", b.ID), slog.String("operation", op), slog.String("reason", metric))
		return res
	}
	if h.stopping.Load() {
		return reject("shutdown", bgp.ReasonBusy, "Server shutting down")
	}
	select {
	case h.queue <- struct{}{}:
		defer func() { <-h.queue }()
	default:
		return reject("queue_full", bgp.ReasonBusy, "Server busy: query queue is full; try again shortly")
	}
	// A query that cannot start within half its time budget is rejected
	// quickly (HTTP 503 for plain text) instead of queueing indefinitely.
	wait := time.NewTimer(min(h.cfg.LookingGlass.QueueWait, h.cfg.TimeoutFor(b.Backend)/2))
	defer wait.Stop()
	select {
	case h.expensive <- struct{}{}:
		defer func() { <-h.expensive }()
	case <-h.stopCh:
		return reject("shutdown", bgp.ReasonBusy, "Server shutting down")
	case <-wait.C:
		return reject("slot_timeout", bgp.ReasonBusy, "Server busy: too many large queries are already running; try again shortly")
	case <-ctx.Done():
		return reject("canceled", bgp.ReasonCanceled, "Request cancelled")
	}
	lease := h.budget.newLease()
	res.lease = lease
	// Include per-result metadata and cache bookkeeping even for empty results.
	if !lease.reserve(1024) {
		return reject("budget", bgp.ReasonBusy, "Server memory capacity is busy; please retry shortly")
	}
	h.metrics.backendActive.WithLabelValues(b.ID).Inc()
	r, err := b.client.SearchWithReservation(ctx, q, lease.reserve)
	h.metrics.backendActive.WithLabelValues(b.ID).Dec()
	if err != nil {
		r.Completion = bgp.Completion{Status: bgp.StatusFailed, Reason: bgp.ReasonBackendError, Detail: "Invalid query"}
	} else {
		h.recordBackend(b, op, r.Status, r.Reason, r.Elapsed)
	}
	res.Result = r
	res.FetchedAt = time.Now()
	h.logBackend(ctx, b.ID, op, r)
	return res
}

// logBackend writes one summary line per upstream query. Routes, queries and
// backend addresses are never logged. Complete results and client
// cancellations are informational; partial or failed results are warnings.
func (h *Handler) logBackend(ctx context.Context, id, op string, r bgp.Result) {
	level := slog.LevelInfo
	if r.Status != bgp.StatusComplete && r.Reason != bgp.ReasonCanceled {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("request_id", requestID(ctx)), // the request that started the (possibly shared) query
		slog.String("backend", id),
		slog.String("operation", op),
		slog.String("status", string(r.Status)),
		slog.Float64("backend_duration_ms", float64(r.Elapsed.Microseconds())/1000),
		slog.Int("routes", r.RouteCount),
		slog.Int64("bytes", r.Bytes),
		slog.Int64("retained_bytes", r.MemBytes),
	}
	if r.Reason != bgp.ReasonNone {
		attrs = append(attrs, slog.String("reason", string(r.Reason)))
	}
	if r.FirstRoute > 0 {
		attrs = append(attrs, slog.Float64("first_route_ms", float64(r.FirstRoute.Microseconds())/1000))
	}
	h.log.LogAttrs(ctx, level, "backend query", attrs...)
}

// overviewReason maps an overview transport error to a bounded reason.
func overviewReason(err error) bgp.Reason {
	switch {
	case err == nil:
		return bgp.ReasonNone
	case errors.Is(err, context.DeadlineExceeded):
		return bgp.ReasonTimeout
	case errors.Is(err, context.Canceled):
		return bgp.ReasonCanceled
	}
	return bgp.ReasonBackendError
}
