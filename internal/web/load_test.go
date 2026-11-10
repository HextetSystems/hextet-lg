package web

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Run explicitly with HEXTET_LOAD_TEST=1 go test ./internal/web -run TestMockLoad -v.
// This in-process smoke load is not a substitute for container RSS testing.
func TestMockLoad(t *testing.T) {
	if os.Getenv("HEXTET_LOAD_TEST") != "1" {
		t.Skip("optional load smoke test")
	}
	doc := ribJSON(10000, false)
	lg := defaults()
	lg.CacheTTL = time.Minute
	lg.MaxRetainedBytes = 32 << 20
	lg.MaxTotalRetainedBytes = 512 << 20
	lg.CacheMaxBytes = 64 << 20
	lg.QueueWait = 100 * time.Millisecond
	cfg, closeBackend := testConfig(lg, backendSpec{id: "a", name: "A", h: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rib" {
			io.WriteString(w, doc)
		} else {
			io.WriteString(w, `{"neighbors":[]}`)
		}
	}})
	defer closeBackend()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var accepted, rejected, partial atomic.Int64
	var peakHeap, peakRSS, peakRetained atomic.Uint64
	var peakActive, peakWork, peakWaiting atomic.Int64
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				maxUint(&peakHeap, m.HeapAlloc)
				if data, err := os.ReadFile("/proc/self/statm"); err == nil {
					f := strings.Fields(string(data))
					if len(f) > 1 {
						p, _ := strconv.ParseUint(f[1], 10, 64)
						maxUint(&peakRSS, p*uint64(os.Getpagesize()))
					}
				}
				h.budget.mu.Lock()
				n := h.budget.used
				h.budget.mu.Unlock()
				maxUint(&peakRetained, uint64(n))
				maxInt(&peakActive, int64(len(h.expensive)))
				maxInt(&peakWork, int64(len(h.work.active)))
				maxInt(&peakWaiting, int64(len(h.work.queue)))
			}
		}
	}()
	handler := h.Routes()
	for phase := 0; phase < 2; phase++ {
		var wg sync.WaitGroup
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				target := "/?as=64500&view=text&output=plain"
				if phase == 1 {
					if i%5 == 0 {
						target = "/summary"
					} else if i%5 == 1 {
						target = "/neighbors"
					} else if i%2 == 0 {
						target = fmt.Sprintf("/?as=%d&view=table", 64501+i)
					}
				}
				req := httptest.NewRequest("GET", target, nil)
				req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
				w := &loadWriter{header: make(http.Header)}
				handler.ServeHTTP(w, req)
				if w.status == 503 || w.status == 429 {
					rejected.Add(1)
				} else {
					accepted.Add(1)
				}
				if w.header.Get("X-Hextet-LG-Completeness") == "partial" {
					partial.Add(1)
				}
			}(i)
		}
		wg.Wait()
	}
	close(done)
	_, cache := h.cache.Stats()
	t.Logf("requests=%d accepted=%d rejected=%d partial=%d peak_decodes=%d peak_renderers=%d peak_waiters=%d peak_accounted=%d cache_bytes=%d peak_heap=%d peak_rss=%d", accepted.Load()+rejected.Load(), accepted.Load(), rejected.Load(), partial.Load(), peakActive.Load(), peakWork.Load(), peakWaiting.Load(), peakRetained.Load(), cache, peakHeap.Load(), peakRSS.Load())
}

type loadWriter struct {
	header http.Header
	status int
}

func (w *loadWriter) Header() http.Header { return w.header }
func (w *loadWriter) WriteHeader(n int) {
	if w.status == 0 {
		w.status = n
	}
}
func (w *loadWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return len(p), nil
}
func maxUint(p *atomic.Uint64, n uint64) {
	for old := p.Load(); n > old; old = p.Load() {
		if p.CompareAndSwap(old, n) {
			return
		}
	}
}
func maxInt(p *atomic.Int64, n int64) {
	for old := p.Load(); n > old; old = p.Load() {
		if p.CompareAndSwap(old, n) {
			return
		}
	}
}
