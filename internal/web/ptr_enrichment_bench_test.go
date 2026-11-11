package web

import (
	"context"
	"fmt"
	"github.com/HextetSystems/hextet-lg/internal/config"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type benchPTR struct{ release <-chan struct{} }

func (m benchPTR) LookupAddr(ctx context.Context, _ string) ([]string, error) {
	select {
	case <-m.release:
		return []string{"router.example."}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func BenchmarkPTRRoutePage(b *testing.B) {
	for _, tc := range []struct {
		name                    string
		enabled, warm, combined bool
		count                   int
	}{{"disabled", false, false, false, 100}, {"empty", true, false, false, 100}, {"warm", true, true, false, 100}, {"large_repeated", true, true, false, 1000}, {"saturated", true, false, false, 1000}, {"combined", true, true, true, 100}} {
		b.Run(tc.name, func(b *testing.B) {
			var doc strings.Builder
			doc.WriteString(`{"rib":[`)
			for i := 0; i < tc.count; i++ {
				if i > 0 {
					doc.WriteByte(',')
				}
				fmt.Fprintf(&doc, `{"prefix":"10.%d.%d.0/24","aspath":"174 13335","true_nexthop":"192.0.2.%d"}`, i/256, i%256, 1+i%200)
			}
			doc.WriteString(`]}`)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, doc.String()) }))
			defer srv.Close()
			lg := config.Defaults()
			lg.PageSize = tc.count
			lg.CacheTTL = time.Minute
			cfg := config.Config{Server: config.Server{Title: "Bench", RatePerSecond: 1e6, RateBurst: 1e6, LiveRatePerSecond: 1e6, LiveRateBurst: 1e6}, LookingGlass: lg, Backends: []config.Backend{{ID: "a", Name: "A", URL: srv.URL, Enabled: true}}, PTREnrichment: config.PTRDefaults(), ASNEnrichment: config.ASNDefaults()}
			cfg.PTREnrichment.Enabled = tc.enabled
			cfg.PTREnrichment.MaxConcurrent = 1
			cfg.ASNEnrichment.Enabled = tc.combined
			release := make(chan struct{})
			if tc.warm {
				close(release)
			}
			h, err := NewHandler(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), WithPTRResolver(benchPTR{release}), WithASNResolver(mockASNResolver{release, make(chan string, 2048)}))
			if err != nil {
				b.Fatal(err)
			}
			defer h.Close()
			handler := h.Routes()
			url := "/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table"
			get(handler, url)
			if tc.warm {
				until := time.Now().Add(time.Second)
				for h.ptrTitle("192.0.2.1") == "" {
					if time.Now().After(until) {
						b.Fatal("cache timeout")
					}
					time.Sleep(time.Millisecond)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if get(handler, url).Code != 200 {
					b.Fatal("response")
				}
			}
		})
	}
}
