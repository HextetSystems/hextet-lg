package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// m51Mock is a deterministic mock bgplgd for browser checks. A search for the
// documentation prefix 203.0.113.0/24 answers with no routes; everything else
// answers with a few routes. It never serves private routing data.
func m51Mock(routes int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rib":
			if strings.HasPrefix(r.URL.Query().Get("prefix"), "203.0.113.") {
				_, _ = io.WriteString(w, "{\n\n}\n")
				return
			}
			_, _ = io.WriteString(w, ribJSON(routes, strings.Contains(r.URL.Query().Get("prefix"), ":")))
		case "/neighbors":
			_, _ = io.WriteString(w, m47Neighbors)
		case "/summary":
			_, _ = io.WriteString(w, m47Summary)
		case "/rtr":
			_, _ = io.WriteString(w, m47RTR)
		default:
			http.NotFound(w, r)
		}
	}
}

// TestBrowserRegression runs the headless-Chromium scenarios in
// testdata/browser/regression.js against an isolated, mock-backed instance.
// It is opt-in because Playwright is not a dependency of this repository:
//
//	HEXTET_LG_BROWSER=1 \
//	NODE_PATH=/path/to/node_modules   (directory containing "playwright")
//	go test ./internal/web -run TestBrowserRegression -v
//
// HEXTET_LG_BROWSER_SCENARIOS optionally limits the run (space separated).
// When it is not enabled, or Playwright or node is missing, the test is
// skipped; a skip is never a pass.
func TestBrowserRegression(t *testing.T) {
	if os.Getenv("HEXTET_LG_BROWSER") != "1" {
		t.Skip("set HEXTET_LG_BROWSER=1 (and NODE_PATH to a Playwright install) to run the browser regression")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	if out, err := exec.Command(node, "-e", "require.resolve('playwright')").CombinedOutput(); err != nil {
		t.Skipf("playwright is not resolvable (set NODE_PATH): %s", strings.TrimSpace(string(out)))
	}

	lg := defaults()
	lg.PageSize = 2 // five routes over two backends: three pages
	lg.CacheTTL = 20 * time.Second
	cfg, closeAll := testConfig(lg,
		backendSpec{"a", "Alpha", m51Mock(3)},
		backendSpec{"b", "Bravo", m51Mock(2)},
	)
	defer closeAll()
	cfg.Site.Name = "Test Site"
	h, err := NewHandler(cfg, WithPTRResolver(slowPTR{delay: 200 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	// Second instance: the opt-in presentation settings and hostile-looking
	// branding text, which must be shown as text and never as markup.
	rawCfg, closeRaw := testConfig(lg, backendSpec{"a", "Alpha", m51Mock(3)})
	defer closeRaw()
	rawCfg.Site.Name = "<i>Raw</i> Site"
	rawCfg.Site.Operator = "<b>Op</b> & \"quotes\" long-operator-name"
	rawCfg.Display.RawJSON = true
	rawCfg.Display.RouteColumns.Neighbor = true
	rawCfg.Display.RouteColumns.NeighborFormat = "both"
	rawH, err := NewHandler(rawCfg, WithPTRResolver(slowPTR{delay: 200 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	defer rawH.Close()
	rawSrv := httptest.NewServer(rawH.Routes())
	defer rawSrv.Close()

	script, err := filepath.Abs(filepath.Join("testdata", "browser", "regression.js"))
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{script, srv.URL}, strings.Fields(os.Getenv("HEXTET_LG_BROWSER_SCENARIOS"))...)
	cmd := exec.Command(node, args...)
	cmd.Env = append(os.Environ(), "HEXTET_LG_RAW_BASE="+rawSrv.URL)
	out, err := cmd.CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("browser regression failed: %v", err)
	}
	if strings.Contains(string(out), "FAIL ") {
		t.Fatal("browser regression reported failures")
	}
}
