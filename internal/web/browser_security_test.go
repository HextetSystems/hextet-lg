package web

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

// m52HostileBackend is a mock bgplgd whose every text field carries markup,
// plus one marker ("RAWONLY") that exists only in raw backend objects and RTR
// validator addresses that exist nowhere else.
func m52HostileBackend(w http.ResponseWriter, r *http.Request) {
	x := strings.ReplaceAll(m52XSS, `"`, `\"`)
	switch r.URL.Path {
	case "/rib":
		var b strings.Builder
		b.WriteString(`{"rib":[`)
		for i := 0; i < 5; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"prefix":"10.0.%d.0/24","aspath":"64500 6450%d","true_nexthop":"192.0.2.%d","exit_nexthop":"192.0.2.%d","neighbor":{"description":"peer %s","remote_addr":"192.0.2.250"},"valid":true,"best":true,"ovs":"valid","avs":"unknown","origin":"igp %s","source":"%s","communities":["64500:1","%s"],"large_communities":["64500:0:1","%s"],"m52_raw":"RAWONLY-%d %s"}`,
				i, i, i+1, i+1, x, x, x, x, x, i, x)
		}
		b.WriteString("]}\n")
		_, _ = io.WriteString(w, b.String())
	case "/neighbors", "/summary":
		fmt.Fprintf(w, `{"neighbors":[{"remote_as":"64501","remote_addr":"192.0.2.250","description":"peer %s","group":"g %s","bgpid":"192.0.2.251","state":"Established","last_updown":"01:00:00","last_updown_sec":3600,"last_error_received":"%s","last_shutdown_reason":"%s","m52_raw":"RAWONLY-N %s"}]}`, x, x, x, x, x)
	case "/rtr":
		fmt.Fprintf(w, `{"rtrs":[{"descr":"validator %s","remote_addr":"10.77.77.77","remote_port":8282,"local_addr":"10.88.88.88","state":"established","version":1,"session_id":7,"serial":42,"refresh":3600,"retry":600,"expire":7200,"last_recv_error":"%s","last_recv_msg":"%s"},{"remote_addr":"2001:db8:7777::7","remote_port":3323,"state":"idle"}]}`, x, x, x)
	default:
		http.NotFound(w, r)
	}
}

// TestBrowserSecurity runs the M5.2 security scenarios in
// testdata/browser/security.js with headless Chromium against isolated,
// mock-backed instances: a default one, one with every opt-in disclosure and
// hostile branding, and the default one again behind a reverse proxy.
//
// It is opt-in exactly like TestBrowserRegression (HEXTET_LG_BROWSER=1 and
// NODE_PATH pointing at a Playwright install), and a skip is never a pass.
// HEXTET_LG_BROWSER_SCENARIOS limits the run by scenario name.
func TestBrowserSecurity(t *testing.T) {
	if os.Getenv("HEXTET_LG_BROWSER") != "1" {
		t.Skip("set HEXTET_LG_BROWSER=1 (and NODE_PATH to a Playwright install) to run the browser security checks")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	if out, err := exec.Command(node, "-e", "require.resolve('playwright')").CombinedOutput(); err != nil {
		t.Skipf("playwright is not resolvable (set NODE_PATH): %s", strings.TrimSpace(string(out)))
	}

	brand := t.TempDir()
	logo := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 32" width="120" height="32"><rect width="120" height="32" rx="6" fill="#1d4ed8"/><text x="10" y="21" font-size="14" fill="#ffffff">TEST</text></svg>`
	if err := os.WriteFile(filepath.Join(brand, "logo.svg"), []byte(logo), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brand, "favicon.svg"), []byte(logo), 0o644); err != nil {
		t.Fatal(err)
	}

	lg := defaults()
	lg.PageSize = 3
	lg.CacheTTL = 20 * time.Second
	start := func(edit func(*config.Config)) *httptest.Server {
		cfg, closeAll := testConfig(lg, backendSpec{"a", "Alpha " + m52XSS, m52HostileBackend})
		t.Cleanup(closeAll)
		cfg.Server.Title = ""
		cfg.Site.Branding.Directory = brand
		cfg.Site.Branding.Logo = "logo.svg"
		cfg.Site.Branding.Favicon = "favicon.svg"
		cfg.PTREnrichment = config.PTRDefaults()
		cfg.PTREnrichment.Enabled = true
		edit(&cfg)
		h, err := NewHandler(cfg, WithPTRResolver(slowPTR{delay: 100 * time.Millisecond}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.Close)
		srv := httptest.NewServer(h.Routes())
		t.Cleanup(srv.Close)
		return srv
	}
	// Default privacy settings; only the site name is set.
	def := start(func(c *config.Config) { c.Site.Name = "Sec Site" })
	// Every opt-in disclosure, and operator text that looks like markup.
	opt := start(func(c *config.Config) {
		c.Site.Name = `<img src=x onerror=alert(3)> Site`
		c.Site.Operator = `<script>alert(4)</script> "Op" & Co`
		subtitle := `<b onmouseover=alert(5)>sub</b>`
		c.Site.Subtitle = &subtitle
		c.Display.RawJSON = true
		c.Display.RouteColumns.Neighbor = true
		c.Display.RouteColumns.NeighborFormat = "both"
		c.Features.RTRSessionAddresses = b3(true)
	})
	// The default instance behind a reverse proxy on another origin, which
	// rewrites Host and adds the usual forwarded headers.
	appURL, _ := url.Parse(def.URL)
	proxy := httputil.NewSingleHostReverseProxy(appURL)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		public := r.Host
		director(r)
		r.Host = appURL.Host
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", public)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	script, err := filepath.Abs(filepath.Join("testdata", "browser", "security.js"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, append([]string{script}, strings.Fields(os.Getenv("HEXTET_LG_BROWSER_SCENARIOS"))...)...)
	cmd.Env = append(os.Environ(), "HEXTET_LG_SEC_BASE="+def.URL, "HEXTET_LG_SEC_OPTIN="+opt.URL, "HEXTET_LG_SEC_PROXY="+front.URL)
	out, err := cmd.CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("browser security checks failed: %v", err)
	}
	if strings.Contains(string(out), "FAIL ") {
		t.Fatal("browser security checks reported failures")
	}
}
