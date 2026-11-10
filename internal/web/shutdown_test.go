package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func TestShutdownDeadlineCancelsBackendAndRecoversResources(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	cfg, closeBackend := testConfig(defaults(), backendSpec{id: "a", name: "A", h: func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}})
	defer closeBackend()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(h.Routes())
	server.Start()
	defer server.Close()
	defer h.Close()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r, err := http.Get(server.URL + "/?as=64500&view=text&output=plain")
		if err == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("backend never started")
	}
	h.StopAdmission()
	ready := httptest.NewRecorder()
	h.Routes().ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status %d", ready.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := server.Config.Shutdown(ctx); err != context.DeadlineExceeded {
		t.Fatalf("shutdown error = %v", err)
	}
	_ = server.Config.Close()
	h.Close()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("backend request not canceled")
	}
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not finish")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		h.budget.mu.Lock()
		used := h.budget.used
		h.budget.mu.Unlock()
		if used == 0 && len(h.work.active) == 0 && len(h.expensive) == 0 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("resources retained: bytes=%d work=%d queries=%d", used, len(h.work.active), len(h.expensive))
		case <-time.After(time.Millisecond):
		}
	}
}

func TestStopAdmissionWakesWaiterAndIdleClose(t *testing.T) {
	lg := defaults()
	lg.MaxConcurrentWork = 1
	lg.MaxQueuedWork = 1
	lg.QueueWait = time.Minute
	cfg, closeBackend := testConfig(lg, backendSpec{id: "a", name: "A", h: serveDoc(`{}`)})
	defer closeBackend()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.work.active <- struct{}{}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		if h.admitWork(w, httptest.NewRequest("GET", "/summary", nil)) {
			h.work.release()
		}
		done <- w
	}()
	deadline := time.After(5 * time.Second)
	for len(h.work.queue) == 0 {
		select {
		case <-deadline:
			t.Fatal("request did not enter admission queue")
		default:
			runtime.Gosched()
		}
	}
	h.StopAdmission()
	select {
	case w := <-done:
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Fatalf("status=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting request did not stop")
	}
	<-h.work.active
	h.Close()
	if len(h.work.active) != 0 || len(h.work.queue) != 0 {
		t.Fatal("admission capacity leaked")
	}
	// Closing an idle handler is safe and repeatable.
	h.Close()
}
