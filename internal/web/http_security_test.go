package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
	"github.com/HextetSystems/hextet-lg/internal/visibility"
)

// M5.2 security and deployment regression tests. Every backend, resolver and
// proxy here is an in-process mock; nothing contacts a real router or DNS.

const m52XSS = `"><script>alert(1)</script><img src=x onerror=alert(2)>'&`

// m52Rib has four visible and six hidden routes under the exclusions of
// m52Visibility. Hidden routes carry next hops, origin ASNs and a raw-only
// marker that appear nowhere else, so a leak through any path is detectable.
func m52Rib() string {
	type rt struct{ prefix, nh, origin string }
	routes := []rt{
		{"192.0.2.0/24", "192.0.2.1", "3001"},                    // visible
		{"198.51.100.0/24", "192.0.2.66", "3666"},                // hidden: or-longer rule itself
		{"198.51.100.128/25", "192.0.2.67", "3667"},              // hidden: more specific of or-longer
		{"203.0.113.0/24", "192.0.2.68", "3668"},                 // hidden: exact rule
		{"203.0.113.128/25", "192.0.2.2", "3002"},                // visible: exact does not cover more specifics
		{"2001:db8:1::/48", "2001:db8:ffff::1", "3003"},          // visible
		{"2001:db8:dead::/48", "2001:db8:ffff::66", "3669"},      // hidden: or-longer rule itself
		{"2001:db8:dead:beef::/64", "2001:db8:ffff::67", "3670"}, // hidden: more specific of or-longer
		{"2001:db8:eeee::/48", "2001:db8:ffff::68", "3671"},      // hidden: exact rule
		{"2001:db8:eeee:1::/64", "2001:db8:ffff::2", "3004"},     // visible
	}
	var b strings.Builder
	b.WriteString(`{"rib":[`)
	for i, r := range routes {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"prefix":%q,"aspath":"3000 %s","true_nexthop":%q,"exit_nexthop":%q,"neighbor":{"description":"peer","remote_addr":"192.0.2.250"},"valid":true,"best":true,"ovs":"valid","avs":"unknown","origin":"igp","m52_raw":"RAWONLY-%d %s"}`,
			r.prefix, r.origin, r.nh, r.nh, i, strings.ReplaceAll(m52XSS, `"`, `\"`))
	}
	b.WriteString("]}\n")
	return b.String()
}

var (
	// What must never be shown: the excluded prefixes, and everything that only
	// their routes carry (next hops, origin ASNs, raw-only markers).
	m52HiddenPrefixes = []string{"198.51.100.", "203.0.113.0/24", "2001:db8:dead", "2001:db8:eeee::/48"}
	m52HiddenData     = []string{"192.0.2.66", "192.0.2.67", "192.0.2.68", "2001:db8:ffff::66", "2001:db8:ffff::67", "2001:db8:ffff::68",
		"3666", "3667", "3668", "3669", "3670", "3671",
		"RAWONLY-1 ", "RAWONLY-2 ", "RAWONLY-3 ", "RAWONLY-6 ", "RAWONLY-7 ", "RAWONLY-8 "}
	m52VisiblePrefixes = []string{"192.0.2.0/24", "203.0.113.128/25", "2001:db8:1::/48", "2001:db8:eeee:1::/64"}
)

func m52Visibility() visibility.Config {
	return visibility.Config{Exclude: visibility.Exclusions{
		IPv4: []visibility.Rule{{Prefix: "198.51.100.0/24", Match: "or-longer"}, {Prefix: "203.0.113.0/24", Match: "exact"}},
		IPv6: []visibility.Rule{{Prefix: "2001:db8:dead::/48", Match: "or-longer"}, {Prefix: "2001:db8:eeee::/48", Match: "exact"}},
	}}
}

const m52Neighbors = `{"neighbors":[{"remote_as":"3000","remote_addr":"192.0.2.250","description":"peer \"><script>alert(1)</script>","group":"g","bgpid":"192.0.2.251","state":"Established","last_updown":"01:00:00","last_updown_sec":3600,"session":{"local_addr":"10.99.99.99","holdtime":90},"m52_raw":"RAWONLY-N"}]}`

// m52RTR uses validator addresses and ports that occur nowhere else.
const m52RTR = `{"rtrs":[{"descr":"validator-a","remote_addr":"10.77.77.77","remote_port":8282,"local_addr":"10.88.88.88","state":"established","version":1,"session_id":7,"serial":42,"refresh":3600,"retry":600,"expire":7200},{"remote_addr":"2001:db8:7777::7","remote_port":3323,"state":"idle"}]}`

var m52RTRPrivate = []string{"10.77.77.77", "2001:db8:7777::7", "8282", "3323", "10.88.88.88"}

// m52Backend records every upstream request URI and serves the M5.2 documents.
type m52Backend struct {
	mu   sync.Mutex
	seen []string
}

func (b *m52Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.seen = append(b.seen, r.Method+" "+r.URL.RequestURI())
	b.mu.Unlock()
	switch r.URL.Path {
	case "/rib":
		_, _ = io.WriteString(w, m52Rib())
	case "/neighbors", "/summary":
		_, _ = io.WriteString(w, m52Neighbors)
	case "/rtr":
		_, _ = io.WriteString(w, m52RTR)
	default:
		http.NotFound(w, r)
	}
}

func (b *m52Backend) requests() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.seen...)
}

func m52Handler(t *testing.T, edit func(*config.Config), opts ...Option) (http.Handler, *m52Backend) {
	t.Helper()
	be := &m52Backend{}
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", be.ServeHTTP})
	t.Cleanup(closeAll)
	if edit != nil {
		edit(&cfg)
	}
	h, err := NewHandler(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h.Routes(), be
}

// m52Variants fetches target as a full page, an HTMX fragment and an HTMX
// history restore: the three shapes a browser can receive.
func m52Variants(h http.Handler, target string) map[string]*httptest.ResponseRecorder {
	return map[string]*httptest.ResponseRecorder{
		"full":     get(h, target),
		"fragment": get(h, target, "HX-Request", "true"),
		"restore":  get(h, target, "HX-Request", "true", "HX-History-Restore-Request", "true"),
	}
}

const m52CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// --- HTTP security -------------------------------------------------------

func TestM52SecurityHeadersOnEveryKindOfResponse(t *testing.T) {
	h, _ := m52Handler(t, nil)
	for _, tc := range []struct {
		method, target string
		code           int
		ctype          string
	}{
		{"GET", "/", 200, "text/html; charset=utf-8"},
		{"GET", "/?type=prefix&q=192.0.2.0/24", 200, "text/html; charset=utf-8"},
		{"GET", "/?type=prefix&q=192.0.2.0/24&view=text&output=plain", 200, "text/plain; charset=utf-8"},
		{"GET", "/?type=prefix&q=%3Cscript%3E", 400, "text/html; charset=utf-8"},
		{"GET", "/neighbors", 200, "text/html; charset=utf-8"},
		{"GET", "/summary", 404, "text/plain; charset=utf-8"},
		{"GET", "/rtr", 200, "text/html; charset=utf-8"},
		{"GET", "/rtr?x=1", 400, "text/html; charset=utf-8"},
		{"GET", "/communities", 200, "text/html; charset=utf-8"},
		{"GET", "/healthz", 200, "text/plain; charset=utf-8"},
		{"GET", "/readyz", 200, "text/plain; charset=utf-8"},
		{"GET", "/static/app.js", 200, "text/javascript; charset=utf-8"},
		{"GET", "/static/app.css", 200, "text/css; charset=utf-8"},
		{"GET", "/static/theme.css", 200, "text/css; charset=utf-8"},
		{"GET", "/branding/favicon", 200, "image/svg+xml"},
		{"GET", "/nope", 404, "text/plain; charset=utf-8"},
		{"POST", "/", 405, "text/plain; charset=utf-8"},
		{"DELETE", "/neighbors", 405, "text/plain; charset=utf-8"},
		{"PUT", "/static/app.js", 405, "text/plain; charset=utf-8"},
	} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		hd := rr.Header()
		if rr.Code != tc.code || hd.Get("Content-Type") != tc.ctype {
			t.Errorf("%s %s: %d %q, want %d %q", tc.method, tc.target, rr.Code, hd.Get("Content-Type"), tc.code, tc.ctype)
		}
		if hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Referrer-Policy") != "no-referrer" || hd.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: missing baseline headers: %v", tc.method, tc.target, hd)
		}
		csp := hd.Get("Content-Security-Policy")
		if tc.target == "/branding/favicon" {
			// An SVG opened directly gets the stricter, sandboxing policy.
			if csp != "default-src 'none'; style-src 'unsafe-inline'; sandbox" {
				t.Errorf("svg CSP %q", csp)
			}
		} else if csp != m52CSP {
			t.Errorf("%s %s: CSP %q", tc.method, tc.target, csp)
		}
		if strings.Contains(m52CSP, "unsafe-inline") || strings.Contains(m52CSP, "unsafe-eval") || strings.Contains(m52CSP, "*") {
			t.Fatal("page CSP was weakened")
		}
		for _, leak := range []string{"Server", "X-Powered-By", "Access-Control-Allow-Origin", "Set-Cookie"} {
			if hd.Get(leak) != "" {
				t.Errorf("%s %s: unexpected %s header", tc.method, tc.target, leak)
			}
		}
	}
	// No debug, profiling, metrics or raw-data endpoint exists on the public mux.
	for _, p := range []string{"/metrics", "/debug/pprof/", "/debug/vars", "/rib", "/raw", "/api", "/neighbors.json", "/templates/index.html", "/static/../templates/index.html", "/config", "/.env"} {
		if rr := get(h, p); rr.Code != 404 && rr.Code != 307 && rr.Code != 301 {
			t.Errorf("%s: %d", p, rr.Code)
		} else if loc := rr.Header().Get("Location"); loc != "" && (strings.Contains(loc, "//") || !strings.HasPrefix(loc, "/")) {
			t.Errorf("%s: redirect to %q", p, loc)
		}
	}
}

// The embedded static files are served by exact name only (the stock file
// server answered /static/ with a directory listing), carry a validator, and
// are immutable only under the fingerprinted URL the templates emit.
func TestM52StaticAssetsExactNamesValidatorsAndCaching(t *testing.T) {
	h, _ := m52Handler(t, nil)
	for _, p := range []string{"/static/", "/static", "/static/nope.js", "/static/app.js/", "/static/sub/app.js", "/static/..", "/static/.", "/static/%2e%2e/templates/index.html", "/static/index.html"} {
		rr := get(h, p)
		if rr.Code == 200 || strings.Contains(rr.Body.String(), "app.css") || strings.Contains(rr.Body.String(), "<pre>") {
			t.Errorf("%s: %d %.80q", p, rr.Code, rr.Body.String())
		}
	}
	index := get(h, "/").Body.String()
	for _, name := range []string{"app.css", "app.js", "theme-init.js", "htmx.min.js"} {
		plain := get(h, "/static/"+name)
		etag := plain.Header().Get("ETag")
		if plain.Code != 200 || len(etag) < 8 || plain.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d etag=%q cache=%q", name, plain.Code, etag, plain.Header().Get("Cache-Control"))
		}
		versioned := "/static/" + name + "?v=" + strings.Trim(etag, `"`)
		if !strings.Contains(index, `"`+versioned+`"`) {
			t.Errorf("index does not link %s", versioned)
		}
		v := get(h, versioned)
		if v.Code != 200 || v.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || !bytes.Equal(v.Body.Bytes(), plain.Body.Bytes()) {
			t.Errorf("%s: %d cache=%q", versioned, v.Code, v.Header().Get("Cache-Control"))
		}
		// A stale or made-up fingerprint is never immutable.
		if s := get(h, "/static/"+name+"?v=deadbeef"); s.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: stale version cached as %q", name, s.Header().Get("Cache-Control"))
		}
		if c := get(h, "/static/"+name, "If-None-Match", etag); c.Code != http.StatusNotModified || c.Body.Len() != 0 {
			t.Errorf("%s: conditional request answered %d with %d bytes", name, c.Code, c.Body.Len())
		}
	}
}

// --- Input validation ----------------------------------------------------

func TestM52RepeatedPageControlsAreRejected(t *testing.T) {
	h, be := m52Handler(t, nil)
	for _, target := range []string{
		"/?type=prefix&q=192.0.2.0/24&backend=a&backend=all",
		"/?type=prefix&q=192.0.2.0/24&view=table&view=text",
		"/?type=prefix&q=192.0.2.0/24&view=text&output=plain&output=html",
		"/?type=prefix&type=asn&q=192.0.2.0/24",
		"/?type=prefix&q=192.0.2.0/24&q=198.51.100.0/24",
		"/?type=prefix&q=192.0.2.0/24&page=1&page=2",
		"/?prefix=192.0.2.0/24&prefix=198.51.100.0/24",
		"/?as=3000&best=1&best=1",
	} {
		for name, rr := range m52Variants(h, target) {
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s (%s): %d", target, name, rr.Code)
			}
		}
	}
	if seen := be.requests(); len(seen) != 0 {
		t.Fatalf("rejected requests reached the backend: %v", seen)
	}
	// The same controls given once still work.
	if rr := get(h, "/?type=prefix&q=192.0.2.0/24&backend=a&view=text&output=plain"); rr.Code != 200 {
		t.Fatalf("single controls: %d", rr.Code)
	}
}

func TestM52HostileInputIsRejectedBeforeTheBackendAndNeverReflected(t *testing.T) {
	h, be := m52Handler(t, nil)
	x := url.QueryEscape(m52XSS)
	long6 := "2001:db8:" + strings.Repeat("ffff:", 300) + ":/48"
	rejected := []string{
		"/?type=prefix&q=" + x, "/?type=asn&q=" + x, "/?type=large-community&q=" + x, "/?type=" + x + "&q=192.0.2.1",
		"/?community=" + x, "/?large-community=" + x, "/?af=" + x + "&prefix=192.0.2.0/24", "/?ovs=" + x + "&as=3000",
		"/?type=prefix&q=192.0.2.0/24&backend=" + x, "/?type=prefix&q=192.0.2.0/24&backend=http://127.0.0.1:1/",
		"/?type=prefix&q=192.0.2.0/24&view=" + x, "/?type=prefix&q=192.0.2.0/24&page=" + x, "/?type=prefix&q=192.0.2.0/24&page=0",
		"/?type=prefix&q=192.0.2.0/24&page=99999999999999999999", "/?" + x + "=1&prefix=192.0.2.0/24",
		"/?type=prefix&q=192.0.2.0/24%00", "/?type=prefix&q=%E2%80%AE192.0.2.0/24", "/?type=prefix&q=%EF%BC%91%EF%BC%99%EF%BC%92.0.2.0/24",
		"/?group=a%0d%0aSet-Cookie:x=1", "/?neighbor=a%1b%5b2J", "/?neighbor=" + strings.Repeat("a", 256),
		"/?type=prefix&q=" + long6, "/?large-community=" + strings.Repeat("9", 400) + ":1:1", "/?community=64500:1%20OR%201=1",
		"/?type=asn&q=4294967296", "/?type=asn&q=-1", "/?type=asn&q=0", "/?type=prefix&q=::ffff:192.0.2.1", "/?type=prefix&q=fe80::1%25eth0",
		"/?type=prefix&q=192.0.2.0/33", "/?type=prefix&q=2001:db8::/129", "/?rib=in&prefix=192.0.2.0/24", "/?endpoint=in&prefix=192.0.2.0/24",
		"/?type=prefix&q=192.0.2.0/24&all=1&or-shorter=1", "/?neighbor=192.0.2.1&group=g", "/?all=1",
	}
	for _, target := range rejected {
		for name, rr := range m52Variants(h, target) {
			body := rr.Body.String()
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%.70s (%s): status %d", target, name, rr.Code)
			}
			if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
				t.Errorf("%.70s (%s): input reflected unescaped", target, name)
			}
			if name != "fragment" && !strings.Contains(body, "<title>Invalid search · ") {
				t.Errorf("%.70s (%s): title derived from rejected input", target, name)
			}
			for k, v := range rr.Header() {
				// A header may carry the request's own URL (HX-Push-Url), but only
				// percent-encoded.
				if strings.ContainsAny(strings.Join(v, " "), "<>\"\r\n") && k != "Content-Security-Policy" && k != "Etag" {
					t.Errorf("%.70s: input in header %s", target, k)
				}
			}
		}
		// The plaintext error names no user input either.
		if rr := get(h, target+"&output=plain"); rr.Code != 400 || strings.Contains(rr.Body.String(), "<script>") && rr.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("%.70s plain: %d %q", target, rr.Code, rr.Header().Get("Content-Type"))
		}
	}
	if seen := be.requests(); len(seen) != 0 {
		t.Fatalf("invalid input reached the backend: %v", seen)
	}
	// Malformed encoding and oversize are refused by the guard itself.
	for target, want := range map[string]int{"/?q=%zz": 400, "/?type=prefix&q=%": 400} {
		if rr := get(h, target); rr.Code != want {
			t.Errorf("%s: %d", target, rr.Code)
		}
	}
	// Extended-community grammar is left to OpenBGPD by design (documented; the
	// search type is off by default). What this application guarantees is the
	// bound (printable ASCII, 255 bytes) and that the value stays one parameter.
	for _, target := range []string{"/?ext-community=" + strings.Repeat("a", 256), "/?ext-community=rt%0a1:1", "/?ext-community=rt%C2%A01:1"} {
		if rr := get(h, target); rr.Code != 400 {
			t.Errorf("%.40s: %d", target, rr.Code)
		}
	}
	if seen := be.requests(); len(seen) != 0 {
		t.Fatalf("invalid extended community reached the backend: %v", seen)
	}
	if rr := get(h, "/?ext-community="+x+"&backend=a"); rr.Code != 200 || strings.Contains(rr.Body.String(), "<script>alert") {
		t.Errorf("ext-community passthrough: %d", rr.Code)
	}
	if seen := be.requests(); len(seen) != 1 || seen[0] != "GET /rib?ext-community="+x {
		t.Fatalf("upstream ext-community request: %v", seen)
	}
	be.mu.Lock()
	be.seen = nil
	be.mu.Unlock()
	// A value that is valid (a neighbor may be a description) is sent as one
	// encoded parameter and cannot add or alter upstream parameters.
	rr := get(h, "/?neighbor="+url.QueryEscape("x&all=1&prefix=0.0.0.0/0#frag")+"&backend=a")
	if rr.Code != 200 {
		t.Fatalf("neighbor description: %d", rr.Code)
	}
	seen := be.requests()
	if len(seen) != 1 || seen[0] != "GET /rib?neighbor=x%26all%3D1%26prefix%3D0.0.0.0%2F0%23frag" {
		t.Fatalf("upstream request: %v", seen)
	}
	// HTMX request headers are never echoed.
	hx := get(h, "/?type=prefix&q=192.0.2.0/24&backend=a", "HX-Request", "true", "HX-Target", m52XSS, "HX-Current-URL", "javascript:alert(1)", "HX-Trigger", m52XSS, "HX-Prompt", m52XSS)
	if hx.Code != 200 || strings.Contains(hx.Body.String(), "<script>alert(1)") || strings.Contains(hx.Body.String(), "javascript:alert") {
		t.Fatalf("HTMX headers reflected: %d", hx.Code)
	}
	if push := hx.Header().Get("HX-Push-Url"); push != "/?type=prefix&q=192.0.2.0%2F24&backend=a&view=table" {
		t.Fatalf("HX-Push-Url %q", push)
	}
}

// Backend text is data: in text/plain output it can neither start a new line
// (forging a status line) nor carry terminal escape sequences.
func TestM52PlainTextNeutralizesControlCharacters(t *testing.T) {
	doc := `{"rib":[{"prefix":"192.0.2.0/24","aspath":"3000 3001\nStatus: COMPLETE — 0 routes decoded\u001b[2J","true_nexthop":"192.0.2.1","ovs":"valid\r\nERROR: QUERY FAILED","avs":"x\u0007","origin":"igp\u009b31m","neighbor":{"description":"d\ne","remote_addr":"192.0.2.9"},"communities":["64500:1\nBackend: forged"],"large_communities":["1:2:3\u001b]0;t\u0007"],"extended_communities":["rt 1:1x"]}]}`
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", serveDoc(doc)})
	for _, view := range []string{"text", "detail"} {
		rr := get(h, "/?as=3000&backend=a&view="+view+"&output=plain")
		body := rr.Body.String()
		if rr.Code != 200 || rr.Header().Get("X-Hextet-LG-Routes") != "1" {
			t.Fatalf("%s: %d", view, rr.Code)
		}
		for _, c := range []string{"\x1b", "\r", "\a", "\u009b"} {
			if strings.Contains(body, c) {
				t.Errorf("%s: control character %q in plaintext output", view, c)
			}
		}
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "Status: COMPLETE — 0 routes") || strings.HasPrefix(line, "ERROR: QUERY FAILED") || strings.HasPrefix(line, "Backend: forged") {
				t.Errorf("%s: backend text forged the line %q", view, line)
			}
		}
		if n := strings.Count(body, "Status: COMPLETE"); view == "text" && n != 2 {
			// once as the real status line, once inert inside the AS path
			t.Errorf("text: %d status strings", n)
		}
		if !strings.Contains(body, "3000 3001 Status: COMPLETE") {
			t.Errorf("%s: the value itself must stay visible on its line:\n%s", view, body)
		}
	}
	// Ordinary values are passed through untouched (and without copying).
	for _, s := range []string{"", "64500 64501", "2001:db8::/32", "rt 64500:1", "é—…"} {
		if got := plainSafe(s); got != s {
			t.Errorf("plainSafe(%q) = %q", s, got)
		}
	}
}

// --- Backend HTTP client --------------------------------------------------

// The pages must not show anything fetched from a redirect target, and the
// target must not be contacted (the client test covers the transport; this
// covers the three public views end to end).
func TestM52BackendRedirectIsAFailedBackendNotAnSSRF(t *testing.T) {
	var hits atomic.Int64
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"rib":[{"prefix":"192.0.2.0/24","aspath":"3000","neighbor":{"description":"INTERNAL-ONLY","remote_addr":"192.0.2.9"}}],"neighbors":[{"remote_as":"3000","remote_addr":"192.0.2.9","description":"INTERNAL-ONLY","state":"Established"}],"rtrs":[{"descr":"INTERNAL-ONLY","state":"established"}]}`)
	}))
	defer internal.Close()
	h := newTestHandler(t, defaults(),
		backendSpec{"evil", "Redirector", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, internal.URL+r.URL.Path, http.StatusFound)
		}},
		backendSpec{"good", "Good", (&m52Backend{}).ServeHTTP})
	internalHost := strings.TrimPrefix(internal.URL, "http://")
	for _, target := range []string{"/?as=3000", "/?as=3000&view=text", "/neighbors", "/neighbors", "/rtr", "/?as=3000&view=text&output=plain"} {
		rr := get(h, target)
		body := rr.Body.String()
		if rr.Code != 200 {
			t.Errorf("%s: %d", target, rr.Code)
		}
		if strings.Contains(body, "INTERNAL-ONLY") || strings.Contains(body, internalHost) {
			t.Errorf("%s: content or address of the redirect target is on the page", target)
		}
		// The healthy backend is still shown, and the redirecting one is a
		// failure, not an empty result.
		if !strings.Contains(body, "Good") || strings.Contains(textOf(body), "Redirector No matching routes") {
			t.Errorf("%s: outcomes merged or misreported", target)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("redirect target contacted %d times", n)
	}
	plain := get(h, "/?as=3000&view=text&output=plain")
	if got := plain.Header().Get("X-Hextet-LG-Backends"); !strings.Contains(got, "evil=failed;reason=backend_error") || !strings.Contains(got, "good=complete") {
		t.Fatalf("completion header %q", got)
	}
}

// --- Raw JSON -------------------------------------------------------------

func TestM52RawJSONIsAbsentEverywhereByDefault(t *testing.T) {
	h, _ := m52Handler(t, nil)
	targets := []string{
		"/", "/?as=3000&backend=a", "/?as=3000&backend=a&view=text", "/?as=3000&backend=a&view=detail", "/?as=3000&backend=a&page=1",
		"/?as=3000&backend=a&view=text&output=plain", "/?as=3000&backend=a&view=detail&output=plain",
		"/neighbors", "/neighbors", "/rtr", "/communities",
		"/?type=prefix&q=bogus", "/?as=3000&backend=nope", "/?as=3000&page=99", // error views
	}
	for _, target := range targets {
		for name, rr := range m52Variants(h, target) {
			body := rr.Body.String()
			for _, bad := range []string{"RAWONLY", "m52_raw", "Raw JSON", "Raw backend response", "d-raw", "local_addr", "10.99.99.99", `{"prefix"`, `{&#34;prefix`, "&#34;remote_addr&#34;"} {
				if strings.Contains(body, bad) {
					t.Errorf("%s (%s): %q present with display.raw_json off", target, name, bad)
				}
			}
			for k, v := range rr.Header() {
				if strings.Contains(strings.Join(v, ""), "RAWONLY") {
					t.Errorf("%s: raw data in header %s", target, k)
				}
			}
		}
	}
	// The route pages did render routes: the absence above is not an empty page.
	if !strings.Contains(get(h, "/?as=3000&backend=a").Body.String(), "192.0.2.0/24") {
		t.Fatal("route table is empty; the test would prove nothing")
	}
	// No parameter, view or path turns raw output on.
	for target, want := range map[string]int{
		"/?as=3000&raw=1": 400, "/?as=3000&raw_json=true": 400, "/?as=3000&view=json": 400, "/?as=3000&view=raw": 400, "/?as=3000&output=json": 200,
		"/neighbors?raw=1": 400, "/neighbors?output=json": 400, "/rtr?raw=1": 400, "/raw": 404, "/rib": 404, "/neighbors/raw": 404, "/api/rib": 404,
	} {
		rr := get(h, target, "Accept", "application/json")
		if rr.Code != want || strings.Contains(rr.Body.String(), "RAWONLY") || strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d %q", target, rr.Code, rr.Header().Get("Content-Type"))
		}
	}
}

func TestM52RawJSONWhenEnabledIsEscapedAndLimitedToItsDisclosures(t *testing.T) {
	h, be := m52Handler(t, func(c *config.Config) { c.Display.RawJSON = true })
	for _, target := range []string{"/?as=3000&backend=a", "/neighbors", "/neighbors"} {
		for name, rr := range m52Variants(h, target) {
			body := rr.Body.String()
			if !strings.Contains(body, "RAWONLY") || !strings.Contains(body, "Raw JSON") {
				t.Errorf("%s (%s): raw disclosure missing although enabled", target, name)
			}
			if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
				t.Errorf("%s (%s): raw JSON is not escaped", target, name)
			}
			// The payload is element text inside <pre>, never an attribute or script.
			if strings.Contains(body, `="RAWONLY`) || strings.Contains(body, `data-raw`) || strings.Contains(body, "application/json") {
				t.Errorf("%s (%s): raw JSON outside its <pre>", target, name)
			}
		}
	}
	if page := get(h, "/?as=3000&backend=a").Body.String(); strings.Count(page, `class="d-raw"`) != strings.Count(page, `class="d-route"`) || strings.Count(page, `class="d-raw"`) != 10 {
		t.Errorf("route and raw disclosures are not paired one to one")
	}
	// Still never on the RTR page, in text views or plaintext, and no new
	// upstream access comes with it.
	for _, target := range []string{"/rtr", "/?as=3000&backend=a&view=text", "/?as=3000&backend=a&view=detail", "/?as=3000&backend=a&view=text&output=plain", "/communities", "/"} {
		if body := get(h, target).Body.String(); strings.Contains(body, "RAWONLY") || strings.Contains(body, "d-raw") {
			t.Errorf("%s: raw JSON shown", target)
		}
	}
	for _, r := range be.requests() {
		if r != "GET /rib?as=3000" && r != "GET /neighbors" && r != "GET /summary" && r != "GET /rtr" {
			t.Errorf("unexpected upstream request %q", r)
		}
	}
	// The response limit still applies with the setting on.
	small, _ := m52Handler(t, func(c *config.Config) { c.Display.RawJSON = true; c.LookingGlass.MaxResponseBytes = 1024 })
	if rr := get(small, "/?as=3000&backend=a&view=text&output=plain"); rr.Header().Get("X-Hextet-LG-Completeness") == "complete" {
		t.Errorf("response limit not applied: %q", rr.Header().Get("X-Hextet-LG-Backends"))
	}
}

// --- RTR validator privacy -------------------------------------------------

func TestM52RTRValidatorAddressesStayPrivateByDefault(t *testing.T) {
	rec := &recordingPTR{}
	var logs bytes.Buffer
	h, _ := m52Handler(t, func(c *config.Config) {
		c.PTREnrichment = config.PTRDefaults()
		c.PTREnrichment.Enabled = true
		c.Display.RawJSON = true // even the raw disclosures must not carry them
		c.Display.RouteColumns.Neighbor = true
	}, WithPTRResolver(rec), WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	for _, target := range []string{"/rtr", "/rtr?backend=a", "/rtr?backend=all", "/", "/neighbors", "/neighbors", "/communities", "/?as=3000&backend=a"} {
		for name, rr := range m52Variants(h, target) {
			body := rr.Body.String()
			for _, private := range m52RTRPrivate {
				if strings.Contains(body, private) {
					t.Errorf("%s (%s): RTR validator detail %q is in the response", target, name, private)
				}
			}
			if strings.HasPrefix(target, "/rtr") && (strings.Contains(body, "data-ptr") || strings.Contains(body, "data-asn") || strings.Contains(body, "<script>") && false) {
				t.Errorf("%s (%s): RTR page carries enrichment targets", target, name)
			}
		}
	}
	rtr := get(h, "/rtr").Body.String()
	mustContain(t, textOf(rtr), "validator-a", "established", "idle")
	// The status endpoint answers nothing about them and schedules nothing.
	for _, ip := range []string{"10.77.77.77", "2001:db8:7777::7", "10.88.88.88"} {
		if rr := get(h, "/ptr-status?ip="+ip); rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "{}" {
			t.Errorf("/ptr-status %s: %d %s", ip, rr.Code, rr.Body.String())
		}
	}
	m52WaitFor(t, func() bool { return len(rec.addrs()) > 0 }) // route next hops prove enrichment ran
	time.Sleep(100 * time.Millisecond)
	for _, a := range rec.addrs() {
		if a == "10.77.77.77" || a == "2001:db8:7777::7" || a == "10.88.88.88" {
			t.Errorf("validator address %s was resolved", a)
		}
	}
	for _, private := range m52RTRPrivate[:2] {
		if strings.Contains(logs.String(), private) {
			t.Errorf("validator address %s was logged", private)
		}
	}
}

func TestM52RTRAddressesOptInChangesOnlyTheAddressColumn(t *testing.T) {
	rec := &recordingPTR{}
	edit := func(on bool) func(*config.Config) {
		return func(c *config.Config) {
			c.Features.RTRSessionAddresses = b3(on)
			c.PTREnrichment = config.PTRDefaults()
			c.PTREnrichment.Enabled = true
		}
	}
	on, _ := m52Handler(t, edit(true), WithPTRResolver(rec))
	off, _ := m52Handler(t, edit(false), WithPTRResolver(rec))
	page := get(on, "/rtr").Body.String()
	mustContain(t, page, "10.77.77.77:8282", "[2001:db8:7777::7]:3323", "10.88.88.88")
	mustNotContain(t, page, "data-ptr", "RAWONLY", "Raw JSON", `"rtrs"`)
	// Enabling addresses does not leak them to other pages or start DNS for them.
	for _, target := range []string{"/", "/neighbors", "/neighbors", "/?as=3000&backend=a"} {
		mustNotContain(t, get(on, target).Body.String(), "10.77.77.77", "2001:db8:7777::7", "10.88.88.88")
	}
	m52WaitFor(t, func() bool { return len(rec.addrs()) > 0 })
	time.Sleep(100 * time.Millisecond)
	for _, a := range rec.addrs() {
		if strings.HasPrefix(a, "10.77.") || strings.HasPrefix(a, "10.88.") || strings.HasPrefix(a, "2001:db8:7777") {
			t.Errorf("validator address %s was resolved", a)
		}
	}
	// The reported state is interpreted identically in both settings.
	state := func(p string) string {
		txt := textOf(p)
		i := strings.Index(txt, "validator-a")
		if i < 0 {
			t.Fatal("session row missing")
		}
		return strings.NewReplacer("10.77.77.77:8282 ", "", "[2001:db8:7777::7]:3323 ", "", "10.88.88.88 ", "").Replace(txt[i:min(len(txt), i+200)])
	}
	if a, b := state(page), state(get(off, "/rtr").Body.String()); !strings.Contains(a, "established") || !strings.Contains(b, "established") {
		t.Errorf("state changed with the address setting:\n on: %s\noff: %s", a, b)
	}
}

func m52WaitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached; the test would prove nothing")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- Route visibility -------------------------------------------------------

// m52ASN records the Team Cymru names that were looked up.
type m52ASN struct {
	mu   sync.Mutex
	seen []string
}

func (r *m52ASN) LookupTXT(_ context.Context, name string) ([]string, error) {
	r.mu.Lock()
	r.seen = append(r.seen, name)
	r.mu.Unlock()
	return nil, errors.New("no such host")
}

func (r *m52ASN) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func TestM52ExcludedRoutesReachNoOutputCacheOrEnrichment(t *testing.T) {
	ptrRec, asnRec := &recordingPTR{}, &m52ASN{}
	h, be := m52Handler(t, func(c *config.Config) {
		c.RouteVisibility = m52Visibility()
		c.LookingGlass.PageSize = 3
		c.LookingGlass.CacheTTL = 30 * time.Second // the second pass is served from the cache
		c.Display.RawJSON = true                   // the widest presentation
		c.Display.RouteColumns.Neighbor = true
		c.PTREnrichment = config.PTRDefaults()
		c.PTREnrichment.Enabled = true
		c.ASNEnrichment = config.ASNDefaults()
		c.ASNEnrichment.Enabled = true
	}, WithPTRResolver(ptrRec), WithASNResolver(asnRec))

	// A visitor may of course name an excluded prefix in a search; the page then
	// echoes their own input (form, title) but still shows none of its data.
	queries := []struct {
		q     string
		named bool
	}{{"as=3000", false}, {"type=prefix&q=0.0.0.0/0&all=1", false}, {"type=prefix&q=::/0&all=1", false}, {"neighbor=192.0.2.250", false},
		{"type=prefix&q=198.51.100.0/24&all=1", true}, {"type=prefix&q=198.51.100.128/25", true}, {"type=prefix&q=2001:db8:dead::/48&all=1", true},
		{"type=prefix&q=203.0.113.0/24", true}, {"type=prefix&q=2001:db8:eeee::/48", true}}
	for pass := 0; pass < 2; pass++ {
		for _, query := range queries {
			q := query.q
			hiddenText := m52HiddenData
			if !query.named {
				hiddenText = append(append([]string(nil), m52HiddenData...), m52HiddenPrefixes...)
			}
			for _, suffix := range []string{"", "&view=text", "&view=detail", "&page=2", "&page=2&view=text", "&view=text&output=plain", "&view=detail&output=plain", "&view=text&output=plain&page=2"} {
				target := "/?" + q + "&backend=a" + suffix
				for name, rr := range m52Variants(h, target) {
					body := rr.Body.String()
					if rr.Code != 200 {
						t.Fatalf("%s (%s): %d", target, name, rr.Code)
					}
					for _, hidden := range hiddenText {
						if strings.Contains(body, hidden) {
							t.Errorf("pass %d %s (%s): excluded route data %q is visible", pass, target, name, hidden)
						}
					}
					// Counts describe visible routes only.
					if got := rr.Header().Get("X-Hextet-LG-Routes"); got != "4" {
						t.Errorf("%s (%s): %s routes counted, want the 4 visible ones", target, name, got)
					}
					if strings.Contains(body, "10 routes") || strings.Contains(body, "of 10") || strings.Contains(body, "Page 2 of 4") {
						t.Errorf("%s (%s): a count includes excluded routes", target, name)
					}
				}
			}
		}
	}
	// All four visible routes are shown across the two pages, on desktop rows
	// and mobile cards alike (they are the same elements).
	p1, p2 := get(h, "/?as=3000&backend=a").Body.String(), get(h, "/?as=3000&backend=a&page=2").Body.String()
	if rowCount(p1) != 3 || rowCount(p2) != 1 || strings.Count(p1+p2, `<tbody class="route"`) != 4 {
		t.Fatalf("rows per page: %d and %d", rowCount(p1), rowCount(p2))
	}
	mustContain(t, p1+p2, m52VisiblePrefixes...)
	mustContain(t, p1+p2, "RAWONLY-0 ", "RAWONLY-4 ", "RAWONLY-5 ", "RAWONLY-9 ")
	if !strings.Contains(p1, "Page 1 of 2") && !strings.Contains(textOf(p1), "1 of 2") {
		t.Errorf("pager does not describe two pages of visible routes")
	}

	// Enrichment ran for visible routes and was never asked about hidden ones.
	m52WaitFor(t, func() bool { return len(ptrRec.addrs()) >= 4 && len(asnRec.names()) >= 2 })
	time.Sleep(150 * time.Millisecond)
	for _, a := range ptrRec.addrs() {
		for _, hidden := range []string{"192.0.2.66", "192.0.2.67", "192.0.2.68", "2001:db8:ffff::66", "2001:db8:ffff::67", "2001:db8:ffff::68"} {
			if a == hidden {
				t.Errorf("next hop %s of an excluded route was resolved", a)
			}
		}
	}
	for _, n := range asnRec.names() {
		for _, hidden := range []string{"AS3666.", "AS3667.", "AS3668.", "AS3669.", "AS3670.", "AS3671."} {
			if strings.HasPrefix(n, hidden) {
				t.Errorf("origin %s of an excluded route was looked up", n)
			}
		}
	}
	for _, ep := range []string{"/ptr-status?ip=192.0.2.66,2001:db8:ffff::66", "/asn-status?asn=3666,3669"} {
		if rr := get(h, ep); rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "{}" {
			t.Errorf("%s: %d %s", ep, rr.Code, rr.Body.String())
		}
	}
	// The exclusion is applied here, not by narrowing the upstream query.
	for _, r := range be.requests() {
		if !strings.HasPrefix(r, "GET /rib?") || strings.Contains(r, "exclude") {
			t.Errorf("unexpected upstream request %q", r)
		}
	}
}

// --- External enrichment -----------------------------------------------------

// hostileDNS answers every lookup with markup.
type hostileDNS struct{ ptr, asn atomic.Int64 }

func (d *hostileDNS) LookupAddr(context.Context, string) ([]string, error) {
	d.ptr.Add(1)
	return []string{`<script>alert(1)</script>.example.`, `x" onmouseover="alert(1).example.`, "ok-host.example."}, nil
}

func (d *hostileDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	d.asn.Add(1)
	var n uint32
	fmt.Sscanf(name, "AS%d.", &n)
	return []string{fmt.Sprintf(`%d | US | arin | 2010-07-14 | EVIL"><script>alert(1)</script><img src=x onerror=alert(2)>`, n)}, nil
}

func TestM52HostileDNSAnswersAreInert(t *testing.T) {
	dns := &hostileDNS{}
	h, _ := m52Handler(t, func(c *config.Config) {
		c.PTREnrichment = config.PTRDefaults()
		c.PTREnrichment.Enabled = true
		c.ASNEnrichment = config.ASNDefaults()
		c.ASNEnrichment.Enabled = true
		c.Display.RouteColumns.Neighbor = true
		c.Display.RouteColumns.NeighborFormat = "both"
	}, WithPTRResolver(dns), WithASNResolver(dns))
	get(h, "/?as=3000&backend=a")
	get(h, "/neighbors")
	m52WaitFor(t, func() bool {
		return strings.Contains(get(h, "/asn-status?asn=3000").Body.String(), "EVIL") && strings.Contains(get(h, "/ptr-status?ip=192.0.2.1").Body.String(), "ok-host")
	})
	for _, target := range []string{"/?as=3000&backend=a", "/neighbors", "/neighbors"} {
		for name, rr := range m52Variants(h, target) {
			body := rr.Body.String()
			if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") || strings.Contains(body, `onmouseover="alert`) {
				t.Errorf("%s (%s): DNS answer rendered as markup", target, name)
			}
		}
	}
	page := get(h, "/?as=3000&backend=a").Body.String()
	mustContain(t, page, "EVIL&#34;&gt;&lt;script&gt;", "ok-host.example")
	for _, ep := range []string{"/asn-status?asn=3000,3001", "/ptr-status?ip=192.0.2.1,2001:db8:ffff::1"} {
		rr := get(h, ep)
		body := rr.Body.String()
		if rr.Header().Get("Content-Type") != "application/json" || rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: headers %v", ep, rr.Header())
		}
		// encoding/json escapes <, > and & so the body is inert even if sniffed.
		if strings.ContainsAny(body, "<>") || strings.Contains(body, "onmouseover") {
			t.Errorf("%s: %s", ep, body)
		}
	}
}

// blockedDNS never answers until released and counts concurrent lookups.
type blockedDNS struct {
	release      chan struct{}
	started      atomic.Int64
	active, peak atomic.Int64
	mu           sync.Mutex
	names        map[string]int
}

func (d *blockedDNS) enter(name string) func() {
	d.started.Add(1)
	d.mu.Lock()
	d.names[name]++
	d.mu.Unlock()
	n := d.active.Add(1)
	for {
		p := d.peak.Load()
		if n <= p || d.peak.CompareAndSwap(p, n) {
			break
		}
	}
	return func() { d.active.Add(-1) }
}

func (d *blockedDNS) wait(ctx context.Context) error {
	select {
	case <-d.release:
		return errors.New("no such host")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *blockedDNS) LookupAddr(ctx context.Context, a string) ([]string, error) {
	defer d.enter("ptr " + a)()
	return nil, d.wait(ctx)
}

func (d *blockedDNS) LookupTXT(ctx context.Context, n string) ([]string, error) {
	defer d.enter("asn " + n)()
	return nil, d.wait(ctx)
}

// A client cycling through many different result pages while the resolver is
// slow can start at most max_concurrent lookups per service and queue a fixed
// number more; nothing grows with the number of requests, and the cache-only
// status endpoints start none.
func TestM52PublicRequestsCannotAmplifyDNS(t *testing.T) {
	const pages, perPage = 40, 50
	backend := func(w http.ResponseWriter, r *http.Request) {
		// Each distinct source AS yields a page of unique next hops and origins.
		var as int
		fmt.Sscanf(r.URL.Query().Get("as"), "%d", &as)
		var b strings.Builder
		b.WriteString(`{"rib":[`)
		for i := 0; i < perPage; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"prefix":"10.%d.%d.0/24","aspath":"%d %d","true_nexthop":"172.16.%d.%d","neighbor":{"remote_addr":"172.17.%d.%d"}}`, as%250, i, as, 200000+as*100+i, as%250, i+1, as%250, i+1)
		}
		b.WriteString("]}")
		_, _ = io.WriteString(w, b.String())
	}
	dns := &blockedDNS{release: make(chan struct{}), names: map[string]int{}}
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", backend})
	defer closeAll()
	cfg.PTREnrichment = config.PTRDefaults()
	cfg.PTREnrichment.Enabled = true
	cfg.PTREnrichment.Timeout = 30 * time.Second
	cfg.ASNEnrichment = config.ASNDefaults()
	cfg.ASNEnrichment.Enabled = true
	cfg.ASNEnrichment.Timeout = 30 * time.Second
	cfg.Display.RouteColumns.Neighbor = true
	h, err := NewHandler(cfg, WithPTRResolver(dns), WithASNResolver(dns))
	if err != nil {
		t.Fatal(err)
	}
	routes := h.Routes()
	for i := 1; i <= pages; i++ {
		for rep := 0; rep < 3; rep++ { // repeats must not add work either
			if rr := get(routes, fmt.Sprintf("/?as=%d&backend=a", 1000+i)); rr.Code != 200 {
				t.Fatalf("page %d: %d", i, rr.Code)
			}
		}
		get(routes, fmt.Sprintf("/asn-status?asn=%d", 200000+(1000+i)*100))
		get(routes, fmt.Sprintf("/ptr-status?ip=172.16.%d.9", (1000+i)%250))
	}
	time.Sleep(200 * time.Millisecond)
	limit := int64(cfg.PTREnrichment.MaxConcurrent + cfg.ASNEnrichment.MaxConcurrent)
	if got := dns.started.Load(); got == 0 || got > limit {
		t.Errorf("%d lookups started while the resolver was blocked; the bound is %d", got, limit)
	}
	if peak := dns.peak.Load(); peak > limit {
		t.Errorf("%d concurrent lookups, bound %d", peak, limit)
	}
	// Releasing the resolver drains only what the bounded queues held: far less
	// than the 40 pages x (50 next hops + 50 neighbors + 51 ASNs) on offer.
	close(dns.release)
	queueBound := limit + int64(cfg.PTREnrichment.MaxConcurrent*16+cfg.ASNEnrichment.MaxConcurrent*16)
	m52WaitFor(t, func() bool { return dns.active.Load() == 0 })
	time.Sleep(200 * time.Millisecond)
	if got := dns.started.Load(); got > queueBound {
		t.Errorf("%d lookups in total; queue bound is %d", got, queueBound)
	}
	dns.mu.Lock()
	for name, n := range dns.names {
		if n > 1 {
			t.Errorf("%s looked up %d times", name, n)
		}
	}
	dns.mu.Unlock()
	h.Close()
	if dns.active.Load() != 0 {
		t.Error("lookups still running after Close")
	}
}

func TestM52EnrichmentDisabledMeansNoEndpointsAndNoTargets(t *testing.T) {
	h, _ := m52Handler(t, nil) // both services are off by default
	for _, ep := range []string{"/asn-status?asn=3000", "/ptr-status?ip=192.0.2.1"} {
		if rr := get(h, ep); rr.Code != 404 {
			t.Errorf("%s: %d", ep, rr.Code)
		}
	}
	for _, target := range []string{"/?as=3000&backend=a", "/neighbors"} {
		mustNotContain(t, get(h, target).Body.String(), "data-asn=", "data-ptr=")
	}
}

// --- Branding ------------------------------------------------------------------

func TestM52BrandingURLsServeOnlyTheConfiguredAssets(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`
	for name, data := range map[string]string{"logo.svg": svg, "other.svg": svg, "notes.txt": "TOP-SECRET"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := m52Handler(t, func(c *config.Config) {
		c.Site.Branding.Directory = dir
		c.Site.Branding.Logo = "logo.svg"
	})
	logo := get(h, "/branding/logo")
	if logo.Code != 200 || logo.Header().Get("Content-Type") != "image/svg+xml" || logo.Header().Get("Cache-Control") != "no-cache" ||
		logo.Header().Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; sandbox" || logo.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("logo: %d %v", logo.Code, logo.Header())
	}
	etag := logo.Header().Get("ETag")
	if v := get(h, "/branding/logo?v="+strings.Trim(etag, `"`)); v.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("versioned logo cache: %q", v.Header().Get("Cache-Control"))
	}
	if c := get(h, "/branding/logo", "If-None-Match", etag); c.Code != http.StatusNotModified {
		t.Errorf("conditional logo: %d", c.Code)
	}
	for _, p := range []string{
		"/branding/", "/branding", "/branding/other.svg", "/branding/notes.txt", "/branding/logo/", "/branding/logo/other.svg", "/branding/logo.svg",
		"/branding/../branding/other.svg", "/branding/logo/../other.svg", "/branding/%2e%2e/notes.txt", "/branding/logo%2f..%2fnotes.txt",
		"/branding/logo?file=notes.txt", "/branding/logo?path=../notes.txt&v=1", "/branding/logo?name=" + url.QueryEscape(secret), "/branding/" + url.PathEscape(secret),
	} {
		rr := get(h, p)
		body := rr.Body.String()
		if strings.Contains(body, "TOP-SECRET") {
			t.Fatalf("%s served another file", p)
		}
		// A query parameter never selects a file: the logo itself is all that
		// such a URL can return.
		if rr.Code == 200 && body != svg {
			t.Errorf("%s: 200 with unexpected content", p)
		}
		if rr.Code == 200 && !strings.HasPrefix(p, "/branding/logo?") {
			t.Errorf("%s: 200", p)
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(m, "/branding/logo", strings.NewReader(svg)))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /branding/logo: %d", m, rr.Code)
		}
	}
}

// --- Reverse proxy ---------------------------------------------------------------

// The application behind a TLS-terminating reverse proxy: every URL it emits
// is relative to the site root, forwarded headers (which an untrusted client
// can also send) are ignored, and the rate limiter keys on the direct peer.
func TestM52BehindAReverseProxy(t *testing.T) {
	var logs bytes.Buffer
	be := &m52Backend{}
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", be.ServeHTTP})
	defer closeAll()
	h, err := NewHandler(cfg, WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	app := httptest.NewServer(h.Routes())
	defer app.Close()
	appURL, _ := url.Parse(app.URL)
	proxy := httputil.NewSingleHostReverseProxy(appURL)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		public := r.Host
		director(r)
		r.Host = appURL.Host // as a proxy that rewrites Host would
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", public)
		r.Header.Set("Forwarded", "for=203.0.113.200;proto=https;host="+public)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(target string, headers ...string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", front.URL+target, nil)
		req.Host = "lg.example.test"
		// What a hostile client would add itself.
		req.Header.Set("X-Forwarded-For", "198.51.100.200")
		req.Header.Set("X-Real-IP", "198.51.100.201")
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}
	internal := []string{appURL.Host, "127.0.0.1", "http://", "https://lg.example.test", "lg.example.test", "evil.example"}
	check := func(what, s string) {
		t.Helper()
		for _, bad := range internal {
			// The only absolute URLs on a page are the fixed project links.
			clean := strings.NewReplacer("http://www.w3.org/", "", "https://github.com/", "", "https://www.openbgpd.org", "").Replace(s)
			if strings.Contains(clean, bad) {
				t.Errorf("%s contains %q", what, bad)
			}
		}
	}
	for _, target := range []string{"/", "/?type=prefix&q=192.0.2.0/24&backend=a", "/?as=3000&backend=a&view=text", "/neighbors", "/neighbors", "/rtr", "/communities"} {
		resp, body := do(target, "X-Forwarded-Host", "evil.example", "X-Forwarded-Proto", "javascript")
		if resp.StatusCode != 200 {
			t.Fatalf("%s through the proxy: %d", target, resp.StatusCode)
		}
		check(target, body)
		if resp.Header.Get("Content-Security-Policy") != m52CSP {
			t.Errorf("%s: CSP lost behind the proxy", target)
		}
	}
	// Canonical redirect, HTMX history URL and Copy link all stay on whatever
	// origin the browser used: they are path-only.
	resp, _ := do("/?type=prefix&q=192.0.2.0/24&backend=a&all=", "X-Forwarded-Host", "evil.example")
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "/?type=prefix&q=192.0.2.0%2F24&backend=a&view=table" {
		t.Errorf("redirect %d to %q", resp.StatusCode, loc)
	}
	resp, body := do("/?type=prefix&q=192.0.2.0/24&backend=a", "HX-Request", "true", "HX-Current-URL", "https://lg.example.test/")
	if push := resp.Header.Get("HX-Push-Url"); push != "/?type=prefix&q=192.0.2.0%2F24&backend=a&view=table" {
		t.Errorf("HX-Push-Url %q", push)
	}
	check("fragment", body)
	resp, body = do("/?type=prefix&q=192.0.2.0/24&backend=a", "HX-Request", "true", "HX-History-Restore-Request", "true")
	if resp.StatusCode != 200 || !strings.Contains(body, "<!doctype html>") {
		t.Errorf("history restore through the proxy: %d", resp.StatusCode)
	}
	_, index := do("/")
	if !strings.Contains(index, "data-copy-link") || strings.Contains(index, "data-copy-link=\"http") {
		t.Error("Copy link must use the browser's own location, not a server-built URL")
	}
	// Static and branding assets, with validators that survive the proxy.
	for _, asset := range []string{"/static/app.js", "/static/htmx.min.js", "/static/theme.css", "/branding/favicon", "/favicon.ico"} {
		resp, _ := do(asset)
		etag := resp.Header.Get("ETag")
		if resp.StatusCode != 200 || etag == "" || resp.Header.Get("Cache-Control") == "" {
			t.Errorf("%s: %d etag=%q cache=%q", asset, resp.StatusCode, etag, resp.Header.Get("Cache-Control"))
		}
		if c, _ := do(asset, "If-None-Match", etag); c.StatusCode != http.StatusNotModified {
			t.Errorf("%s conditional: %d", asset, c.StatusCode)
		}
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if resp, body := do(p); resp.StatusCode != 200 || strings.TrimSpace(body) != "ok" {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	// The management endpoint stays unreachable whatever the proxy forwards.
	if resp, _ := do("/metrics", "X-Forwarded-For", "127.0.0.1"); resp.StatusCode != 404 {
		t.Errorf("/metrics: %d", resp.StatusCode)
	}
	// No WebSocket or streaming upgrade is involved anywhere.
	if resp, _ := do("/", "Connection", "Upgrade", "Upgrade", "websocket"); resp.StatusCode != 200 || resp.Header.Get("Upgrade") != "" {
		t.Errorf("upgrade request: %d %q", resp.StatusCode, resp.Header.Get("Upgrade"))
	}
	// Client identity: nothing from the forwarded headers (or the peer) is logged.
	for _, bad := range []string{"198.51.100.200", "198.51.100.201", "203.0.113.200", "lg.example.test", "evil.example", "127.0.0.1"} {
		if strings.Contains(logs.String(), bad) {
			t.Errorf("log contains %q", bad)
		}
	}
	if !strings.Contains(logs.String(), `"route":"index"`) {
		t.Fatal("no access log captured; the check above would prove nothing")
	}
}

// Forwarded identity never splits the limiter: every client behind one proxy
// shares the proxy's bucket, so spoofing X-Forwarded-For buys nothing.
func TestM52ForwardedIdentityCannotEvadeTheRateLimit(t *testing.T) {
	be := &m52Backend{}
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", be.ServeHTTP})
	defer closeAll()
	cfg.Server.LiveRatePerSecond, cfg.Server.LiveRateBurst = 0.001, 3
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	routes := h.Routes()
	codes := []int{}
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest("GET", "/rtr", nil)
		req.RemoteAddr = "192.0.2.50:4000" // the proxy
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))
		req.Header.Set("X-Real-IP", fmt.Sprintf("198.51.100.%d", i))
		req.Header.Set("Forwarded", fmt.Sprintf("for=198.51.100.%d", i))
		req.Header.Set("CF-Connecting-IP", fmt.Sprintf("198.51.100.%d", i))
		req.Header.Set("True-Client-IP", fmt.Sprintf("198.51.100.%d", i))
		rr := httptest.NewRecorder()
		routes.ServeHTTP(rr, req)
		codes = append(codes, rr.Code)
		if rr.Code == http.StatusTooManyRequests && rr.Header().Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
	}
	if fmt.Sprint(codes) != "[200 200 200 429 429 429]" {
		t.Fatalf("status sequence %v", codes)
	}
	// Health checks from the same peer are exempt, so the proxy's probes keep working.
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.RemoteAddr = "192.0.2.50:4001"
	rr := httptest.NewRecorder()
	routes.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("healthz while limited: %d", rr.Code)
	}
}

// --- Lifecycle ----------------------------------------------------------------------

// Creating, using and closing a fully featured instance leaves no goroutine
// behind (limiter cleanup, cache timers, enrichment workers, flights).
func TestM52CloseLeavesNoGoroutines(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport)
	settle := func() int {
		transport.CloseIdleConnections()
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	before := settle()
	for round := 0; round < 3; round++ {
		be := &m52Backend{}
		cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", be.ServeHTTP}, backendSpec{"slow", "Slow", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}})
		cfg.LookingGlass.CacheTTL = time.Minute
		cfg.PTREnrichment = config.PTRDefaults()
		cfg.PTREnrichment.Enabled = true
		cfg.ASNEnrichment = config.ASNDefaults()
		cfg.ASNEnrichment.Enabled = true
		dns := &blockedDNS{release: make(chan struct{}), names: map[string]int{}}
		h, err := NewHandler(cfg, WithPTRResolver(dns), WithASNResolver(dns))
		if err != nil {
			t.Fatal(err)
		}
		routes := h.Routes()
		for _, target := range []string{"/", "/?as=3000&backend=a", "/neighbors?backend=a", "/rtr?backend=a", "/communities", "/static/app.js"} {
			if rr := get(routes, target); rr.Code != 200 {
				t.Fatalf("%s: %d", target, rr.Code)
			}
		}
		// A request abandoned mid-query: its backend call must be cancelled.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/?as=3000&backend=slow", nil).WithContext(ctx))
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled request did not return")
		}
		h.Close()
		closeAll()
	}
	deadline := time.Now().Add(5 * time.Second)
	after := settle()
	for after > before && time.Now().Before(deadline) {
		after = settle()
	}
	if after > before {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines: %d before, %d after three create/use/close cycles\n%s", before, after, buf[:runtime.Stack(buf, true)])
	}
}
