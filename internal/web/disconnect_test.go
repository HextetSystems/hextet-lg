package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Real HTTP clients ensure that disconnecting propagates through net/http to
// the shared backend request rather than just cancelling a synthetic request.
func TestHTTPSharedDecodeDisconnect(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	cfg, closeBackend := testConfig(defaults(), backendSpec{id: "a", name: "A", h: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"rib":[`)
		flushW(w)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}})
	defer closeBackend()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done := make(chan struct{}, 2)
	request := func(ctx context.Context) {
		defer func() { done <- struct{}{} }()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/?as=64500&view=text&output=plain", nil)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}
	go request(ctx1)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not start")
	}
	go request(ctx2)
	deadline := time.After(5 * time.Second)
	for {
		h.cache.mu.Lock()
		found := false
		for _, f := range h.cache.flights {
			if f.waiters == 2 {
				found = true
			}
		}
		h.cache.mu.Unlock()
		if found {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second consumer did not join")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel1()
	<-done
	select {
	case <-canceled:
		t.Fatal("backend canceled with a consumer remaining")
	default:
	}
	cancel2()
	<-done
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned backend did not cancel")
	}
	deadline = time.After(5 * time.Second)
	for {
		h.budget.mu.Lock()
		used := h.budget.used
		h.budget.mu.Unlock()
		if used == 0 && len(h.work.active) == 0 && len(h.expensive) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("resources retained: bytes=%d work=%d decodes=%d", used, len(h.work.active), len(h.expensive))
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// The client reads nothing from a TCP connection whose receive buffer is tiny.
// A large cached result forces net/http to encounter actual socket backpressure.
func TestCachedRenderDisconnectReleasesResources(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"html", "/?as=64500&view=detail"},
		{"plain", "/?as=64500&view=detail&output=plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg := defaults()
			lg.CacheTTL = time.Minute
			lg.PageSize = 1000
			doc := strings.ReplaceAll(ribJSON(1000, false), `"aspath":"64500 `, `"aspath":"`+strings.Repeat("64500 ", 2000))
			cfg, closeBackend := testConfig(lg, backendSpec{id: "a", name: "A", h: serveDoc(doc)})
			defer closeBackend()
			h, err := NewHandler(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			srv := httptest.NewServer(h.Routes())
			defer srv.Close()
			res, err := http.Get(srv.URL + tc.target)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if n, _ := h.cache.Stats(); n != 1 {
				t.Fatal("result not cached")
			}
			addr := strings.TrimPrefix(srv.URL, "http://")
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			tcp := conn.(*net.TCPConn)
			if err := tcp.SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", tc.target, addr)
			deadline := time.After(5 * time.Second)
			for len(h.work.active) == 0 {
				select {
				case <-deadline:
					conn.Close()
					t.Fatal("renderer did not start")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			// Once the connection has stayed open without reading, closing it must wake
			// a writer even if the peer's socket buffer has filled.
			time.Sleep(100 * time.Millisecond)
			if len(h.work.active) == 0 {
				conn.Close()
				t.Fatal("response did not reach TCP backpressure")
			}
			conn.Close()
			deadline = time.After(5 * time.Second)
			for len(h.work.active) != 0 {
				select {
				case <-deadline:
					t.Fatal("writer did not exit after disconnect")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			h.Close()
			h.budget.mu.Lock()
			used := h.budget.used
			h.budget.mu.Unlock()
			if used != 0 {
				t.Fatalf("retained memory after close: %d", used)
			}
		})
	}
}

func TestHTTPDisconnectWhileWaitingForAdmission(t *testing.T) {
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
	defer h.Close()
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	h.work.active <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/summary", nil)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
	}()
	deadline := time.After(5 * time.Second)
	for len(h.work.queue) == 0 {
		select {
		case <-deadline:
			t.Fatal("request did not queue")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not exit")
	}
	deadline = time.After(5 * time.Second)
	for len(h.work.queue) != 0 {
		select {
		case <-deadline:
			t.Fatal("queue slot leaked")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	<-h.work.active
	if len(h.work.active) != 0 {
		t.Fatal("active slot leaked")
	}
}
