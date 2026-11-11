package web

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

func BenchmarkASNRoutePage(b *testing.B) {
	for _, tc := range []struct {
		name                     string
		enabled, warm, saturated bool
		count                    int
	}{{"disabled", false, false, false, 100}, {"empty", true, false, false, 100}, {"warm", true, true, false, 100}, {"large_repeated", true, true, false, 1000}, {"saturated", true, false, true, 1000}} {
		b.Run(tc.name, func(b *testing.B) {
			var doc strings.Builder
			doc.WriteString(`{"rib":[`)
			for i := 0; i < tc.count; i++ {
				if i > 0 {
					doc.WriteByte(',')
				}
				fmt.Fprintf(&doc, `{"prefix":"10.%d.%d.0/24","aspath":"174 15169 13335 13335"}`, i/256, i%256)
			}
			doc.WriteString(`]}`)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, doc.String()) }))
			defer server.Close()
			lg := config.Defaults()
			lg.PageSize = tc.count
			lg.CacheTTL = time.Minute
			cfg := config.Config{Server: config.Server{Title: "Benchmark", RatePerSecond: 1000000, RateBurst: 1000000, LiveRatePerSecond: 1000000, LiveRateBurst: 1000000}, LookingGlass: lg, Backends: []config.Backend{{ID: "a", Name: "Alpha", URL: server.URL, Enabled: true}}, ASNEnrichment: config.ASNDefaults()}
			cfg.ASNEnrichment.Enabled = tc.enabled
			cfg.ASNEnrichment.MaxConcurrent = 1
			release := make(chan struct{})
			if tc.warm {
				close(release)
			}
			h, err := NewHandler(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), WithASNResolver(mockASNResolver{release, make(chan string, 2048)}))
			if err != nil {
				b.Fatal(err)
			}
			defer h.Close()
			if tc.warm {
				for _, n := range []uint32{174, 15169, 13335} {
					h.asn.Schedule(n)
				}
				until := time.Now().Add(time.Second)
				for h.asnTitle("13335") == "" {
					if time.Now().After(until) {
						b.Fatal("warm cache timeout")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if tc.saturated {
				for i := uint32(1); i <= 100; i++ {
					h.asn.Schedule(i)
				}
			}
			handler := h.Routes()
			url := "/?type=prefix&q=8.8.8.0%2F24&backend=a&view=table"
			get(handler, url)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rr := get(handler, url)
				if rr.Code != 200 {
					b.Fatal(rr.Code)
				}
			}
		})
	}
}
