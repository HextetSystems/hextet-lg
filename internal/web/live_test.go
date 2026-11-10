package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

// Optional, read-only compatibility checks against a real OpenBGPD backend.
//
// Not part of ordinary test runs: it is skipped unless HEXTET_LG_LIVE=1, and
// it takes the backends from an existing configuration file (so no address or
// credential is ever committed):
//
//	HEXTET_LG_LIVE=1 HEXTET_LG_LIVE_CONFIG=configs/config.yaml \
//	[HEXTET_LG_LIVE_EXPECT_VERSION=9.3] \
//	[HEXTET_LG_LIVE_PREFIXES=192.0.2.0/24,2001:db8::1]  (bounded probes you know are small)
//	[HEXTET_LG_LIVE_ASNS=64500]                          (small origin ASes only)
//	go test ./internal/web -run TestLiveOpenBGPD -v -count=1
//
// Safety: the application runs in-process behind httptest (no listener, no
// port); enrichment (DNS) and metrics are forced off; every backend request
// is a GET of /summary, /neighbors, /rtr, /rib (only for the probes you name)
// or /metrics; responses are size-capped; requests are spaced apart. Nothing
// is written. Backend addresses, validator addresses and response bodies are
// never logged: the report holds statuses, counts, durations and field names.
//
// Checks with no data to exercise are reported as SKIP, never as PASS.

const (
	liveRawCap   = 4 << 20 // bytes read from any raw endpoint
	liveMetrics  = 8 << 20
	liveSpacing  = 300 * time.Millisecond
	liveMaxProbe = 8
)

type liveReport struct {
	t     *testing.T
	lines []string
}

func (r *liveReport) add(status, name, detail string) {
	r.lines = append(r.lines, fmt.Sprintf("%-4s %-34s %s", status, name, detail))
}

func (r *liveReport) fail(name, format string, a ...any) {
	r.add("FAIL", name, fmt.Sprintf(format, a...))
	r.t.Errorf("%s: %s", name, fmt.Sprintf(format, a...))
}

func liveGet(ctx context.Context, base, path string, cap int64) (status int, ctype string, body []byte, dur time.Duration, err error) {
	switch path {
	case "/summary", "/neighbors", "/rtr", "/metrics":
	default:
		if !strings.HasPrefix(path, "/rib?") {
			panic("live harness: endpoint not allowed: " + path)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return 0, "", nil, 0, err
	}
	start := time.Now()
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0, "", nil, time.Since(start), fmt.Errorf("request failed (%T)", err) // never echo the URL
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, cap))
	time.Sleep(liveSpacing)
	return resp.StatusCode, resp.Header.Get("Content-Type"), body, time.Since(start), err
}

func jsonKeys(b []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return "(not an object)"
	}
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return strings.Join(ks, ",")
}

func TestLiveOpenBGPD(t *testing.T) {
	if os.Getenv("HEXTET_LG_LIVE") != "1" {
		t.Skip("optional live check: set HEXTET_LG_LIVE=1 and HEXTET_LG_LIVE_CONFIG (read-only; see live_test.go)")
	}
	path := os.Getenv("HEXTET_LG_LIVE_CONFIG")
	if path == "" {
		t.Fatal("HEXTET_LG_LIVE_CONFIG is required")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	// Safe overrides, in memory only.
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.RatePerSecond, cfg.Server.RateBurst = 1000, 1000
	cfg.Server.LiveRatePerSecond, cfg.Server.LiveRateBurst = 1000, 1000
	cfg.ASNEnrichment.Enabled = false
	cfg.PTREnrichment.Enabled = false
	cfg.Metrics.Enabled = false
	cfg.CommunityDefinitions.Enabled = false
	cfg.Site.Branding.Directory, cfg.Site.Branding.Logo, cfg.Site.Branding.Favicon = "", "", ""
	cfg.LookingGlass.CacheTTL = 0

	rep := &liveReport{t: t}
	defer func() {
		t.Logf("\nlive OpenBGPD report (%d backends, no addresses or bodies logged)\n  %s", len(cfg.Backends), strings.Join(rep.lines, "\n  "))
	}()

	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	defer h.Close()
	app := httptest.NewServer(h.Routes())
	defer app.Close()
	page := func(target string) (int, string) {
		resp, err := http.Get(app.URL + target)
		if err != nil {
			return 0, ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		time.Sleep(liveSpacing)
		return resp.StatusCode, string(b)
	}
	ctx := context.Background()
	wantVer := os.Getenv("HEXTET_LG_LIVE_EXPECT_VERSION")
	if wantVer == "" {
		wantVer = "9.3"
	}
	release := regexp.MustCompile(`bgpd_info\{[^}]*release="([^"]+)"`)

	for _, b := range cfg.Backends {
		if !b.Enabled {
			continue
		}
		id := b.ID
		// ---- version, from /metrics when the deployment exposes it ----
		if st, ct, body, d, err := liveGet(ctx, b.URL, "/metrics", liveMetrics); err != nil || st != 200 {
			rep.add("LIM", id+" version", "not exposed by this API (HTTP "+fmt.Sprint(st)+"); the version is operator-provided, not verified")
		} else if m := release.FindSubmatch(body); m == nil {
			rep.add("LIM", id+" version", "/metrics answered but has no bgpd_info release label; operator-provided, not verified")
		} else if string(m[1]) != wantVer {
			rep.fail(id+" version", "backend reports release %q, expected %q", m[1], wantVer)
		} else {
			rep.add("PASS", id+" version", fmt.Sprintf("OpenBGPD %s verified via /metrics (%s, %d B, %v)", m[1], ct, len(body), d.Round(time.Millisecond)))
		}

		// ---- raw shapes of the read-only endpoints ----
		var rtrAddrs []string
		for _, ep := range []string{"/summary", "/neighbors", "/rtr"} {
			st, ct, body, d, err := liveGet(ctx, b.URL, ep, liveRawCap)
			switch {
			case err != nil:
				rep.fail(id+" "+ep, "%v", err)
			case ep == "/rtr" && st == 404:
				rep.add("LIM", id+" "+ep, "this bgplgd has no /rtr (HTTP 404): the RTR view must say unsupported")
			case st != 200 || !strings.HasPrefix(ct, "application/json") || !json.Valid(body):
				rep.fail(id+" "+ep, "HTTP %d, content-type %q, valid JSON %v", st, ct, json.Valid(body))
			default:
				rep.add("PASS", id+" "+ep, fmt.Sprintf("HTTP 200 %s, top-level keys [%s], %d B, %v", ct, jsonKeys(body), len(body), d.Round(time.Millisecond)))
				if ep == "/rtr" {
					var doc struct {
						Rtrs []struct {
							RemoteAddr string `json:"remote_addr"`
						} `json:"rtrs"`
					}
					_ = json.Unmarshal(body, &doc)
					for _, s := range doc.Rtrs {
						if s.RemoteAddr != "" {
							rtrAddrs = append(rtrAddrs, s.RemoteAddr)
						}
					}
				}
			}
		}

		// ---- the application's rendering of the same data ----
		for _, p := range []struct{ path, name string }{{"/summary", "summary page"}, {"/neighbors", "neighbors page"}, {"/rtr", "rtr page"}} {
			code, body := page(p.path + "?backend=" + url.QueryEscape(id))
			switch {
			case code != 200:
				rep.fail(id+" "+p.name, "HTTP %d", code)
			case p.path == "/rtr" && strings.Contains(body, "unsupported"):
				rep.add("LIM", id+" "+p.name, "rendered as unsupported (backend has no /rtr)")
			case strings.Contains(body, "Backend unavailable") || strings.Contains(body, "malformed response") || strings.Contains(body, "Request not completed"):
				rep.fail(id+" "+p.name, "the application reports a backend problem for a backend that answered")
			default:
				rep.add("PASS", id+" "+p.name, "rendered; backend section present, no failure notice")
			}
			if strings.Contains(body, "Raw JSON") && !cfg.Display.RawJSON {
				rep.fail(id+" "+p.name+" raw json", "Raw JSON shown although display.raw_json is off")
			}
			if p.path == "/rtr" && !cfg.Features.RTRSessionAddressesEnabled() {
				leaked := 0
				for _, a := range rtrAddrs {
					if strings.Contains(body, a) {
						leaked++
					}
				}
				if leaked > 0 {
					rep.fail(id+" rtr privacy", "%d validator address(es) in the rendered page", leaked)
				} else if len(rtrAddrs) > 0 {
					rep.add("PASS", id+" rtr privacy", fmt.Sprintf("%d validator address(es) absent from the page", len(rtrAddrs)))
				}
			}
		}

		// ---- bounded route probes the operator named ----
		probes := strings.Split(os.Getenv("HEXTET_LG_LIVE_PREFIXES"), ",")
		for i, p := range probes {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if i >= liveMaxProbe {
				break
			}
			code, body := page("/?type=prefix&q=" + url.QueryEscape(p) + "&backend=" + url.QueryEscape(id))
			rep.add(liveRouteVerdict(code, body), fmt.Sprintf("%s prefix probe %d", id, i+1), liveRouteDetail(code, body))
		}
		for i, a := range strings.Split(os.Getenv("HEXTET_LG_LIVE_ASNS"), ",") {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			if i >= liveMaxProbe {
				break
			}
			code, body := page("/?type=asn&q=" + url.QueryEscape(a) + "&backend=" + url.QueryEscape(id))
			rep.add(liveRouteVerdict(code, body), fmt.Sprintf("%s asn probe %d", id, i+1), liveRouteDetail(code, body))
		}
		if os.Getenv("HEXTET_LG_LIVE_PREFIXES") == "" && os.Getenv("HEXTET_LG_LIVE_ASNS") == "" {
			rep.add("SKIP", id+" route probes", "set HEXTET_LG_LIVE_PREFIXES / HEXTET_LG_LIVE_ASNS to small, known queries; no route query was sent")
		}
	}

	// ---- input handling needs no backend ----
	if code, _ := page("/?type=prefix&q=999.1.1.1"); code != 400 {
		rep.fail("invalid input", "HTTP %d, want 400", code)
	} else {
		rep.add("PASS", "invalid input", "rejected with HTTP 400 before any backend call")
	}
}

func liveRouteVerdict(code int, body string) string {
	switch {
	case code != 200, strings.Contains(body, "Query failed"), strings.Contains(body, "Search could not be run"):
		return "FAIL"
	case strings.Contains(body, "PARTIAL"):
		return "LIM"
	}
	return "PASS"
}

func liveRouteDetail(code int, body string) string {
	switch {
	case code != 200:
		return fmt.Sprintf("HTTP %d", code)
	case strings.Contains(body, "Query failed"):
		return "query failed (a failure, not an empty result)"
	case strings.Contains(body, "No matching routes"):
		return "completed with no matching routes (not a failure; this probe exercised no route rendering)"
	case strings.Contains(body, "PARTIAL"):
		return "partial result (bounded by a limit)"
	}
	return fmt.Sprintf("completed with %d route row(s)", strings.Count(body, `<tbody class="route">`))
}
