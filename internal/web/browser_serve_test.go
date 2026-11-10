package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

// slowPTR answers from a fixed table after a delay, so browser validation can
// observe a page rendering before reverse DNS names arrive.
type slowPTR struct{ delay time.Duration }

func (s slowPTR) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	switch {
	case addr == "198.51.100.1":
		return []string{"peer1.example.net."}, nil
	case addr == "198.51.100.2":
		return []string{"a-very-long-router-hostname-for-truncation.pop1.transit.example.net."}, nil
	case addr == "2001:db8:ffff::1":
		return []string{"v6-peer1.example.net."}, nil
	case strings.HasPrefix(addr, "192.0.2."):
		return []string{"nh" + addr[len("192.0.2."):] + ".example.net."}, nil
	}
	return nil, errors.New("no such host")
}

// TestServeForBrowser is a development aid, not a test: with
// HEXTET_LG_BROWSER_CONFIG set to a configuration path (backends must be mock
// servers) it serves the application with a deterministic slow PTR resolver on
// the configured listen address until killed, for headless browser checks.
//
//	HEXTET_LG_BROWSER_CONFIG=/path/cfg.yaml go test ./internal/web -run TestServeForBrowser -timeout 0
func TestServeForBrowser(t *testing.T) {
	path := os.Getenv("HEXTET_LG_BROWSER_CONFIG")
	if path == "" {
		t.Skip("set HEXTET_LG_BROWSER_CONFIG to serve the application for browser validation")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(cfg, WithPTRResolver(slowPTR{delay: 1500 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	t.Fatal(http.ListenAndServe(cfg.Server.Listen, h.Routes()))
}
