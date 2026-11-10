package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/HextetSystems/hextet-lg/internal/theme"
)

const themeRoute = `[{"prefix":"203.0.113.0/24","aspath":"64500 64501","origin":"igp","true_nexthop":"192.0.2.1","neighbor":{"description":"peer-a","remote_addr":"192.0.2.1"},"best":true,"valid":true,"ovs":"valid","avs":"invalid","localpref":100,"communities":["64500:1"],"large_communities":["395089:0:8"]},
{"prefix":"203.0.113.0/24","aspath":"64502","origin":"igp","true_nexthop":"192.0.2.2","neighbor":"peer-b","ovs":"invalid","avs":"unknown","localpref":90}]`

func themeHandler(t *testing.T, th theme.Config, opts ...Option) http.Handler {
	t.Helper()
	cfg, closeAll := testConfig(defaults(), backendSpec{id: "a", name: "Alpha", h: serveDoc(themeRoute)})
	t.Cleanup(closeAll)
	cfg.Theme = th
	h, err := NewHandler(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h.Routes()
}

func TestThemeStylesheetDelivery(t *testing.T) {
	h := themeHandler(t, theme.Config{Accent: "#0D9488", Default: "dark"})
	rr := get(h, "/static/theme.css")
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	if rr.Header().Get("Cache-Control") != "no-cache" || rr.Header().Get("Last-Modified") != "" {
		t.Fatalf("must revalidate and carry no weak timestamp: %v", rr.Header())
	}
	etag := rr.Header().Get("ETag")
	if !regexp.MustCompile(`^"[0-9a-f]{16}"$`).MatchString(etag) {
		t.Fatalf("ETag %q", etag)
	}
	if !strings.Contains(rr.Body.String(), "--color-accent:#0D9488;") {
		t.Fatal("configured accent missing from delivered CSS")
	}
	// Security headers still apply to the generated stylesheet.
	if rr.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rr.Header().Get("Content-Security-Policy"), "style-src 'self'") {
		t.Fatalf("security headers missing: %v", rr.Header())
	}
	// Generated once: identical bytes and validator on every request.
	again := get(h, "/static/theme.css")
	if again.Body.String() != rr.Body.String() || again.Header().Get("ETag") != etag {
		t.Fatal("stylesheet must be stable between requests")
	}
	// Conditional requests.
	if nm := get(h, "/static/theme.css", "If-None-Match", etag); nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
		t.Fatalf("matching validator: %d", nm.Code)
	}
	if st := get(h, "/static/theme.css", "If-None-Match", `"stale"`); st.Code != 200 {
		t.Fatalf("stale validator: %d", st.Code)
	}
	head := httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequest("HEAD", "/static/theme.css", nil))
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("ETag") != etag {
		t.Fatalf("HEAD: %d %v", head.Code, head.Header())
	}
	if post := httptest.NewRecorder(); func() bool {
		h.ServeHTTP(post, httptest.NewRequest("POST", "/static/theme.css", nil))
		return post.Code == 200
	}() {
		t.Fatal("POST must not be served")
	}
}

func TestRestartWithNewColorsNeverReusesOldValidator(t *testing.T) {
	old := get(themeHandler(t, theme.Config{Accent: "#2563EB"}), "/static/theme.css")
	restarted := themeHandler(t, theme.Config{Accent: "#7C3AED"})
	rr := get(restarted, "/static/theme.css", "If-None-Match", old.Header().Get("ETag"))
	if rr.Code != 200 || rr.Header().Get("ETag") == old.Header().Get("ETag") || !strings.Contains(rr.Body.String(), "#7C3AED") {
		t.Fatalf("stale validator was honoured after a configuration change: %d", rr.Code)
	}
	// The page references the new stylesheet by content hash, so a cached copy
	// of the old URL is never reused either.
	oldLink := regexp.MustCompile(`theme\.css\?v=([0-9a-f]+)`).FindStringSubmatch(get(themeHandler(t, theme.Config{}), "/").Body.String())
	newLink := regexp.MustCompile(`theme\.css\?v=([0-9a-f]+)`).FindStringSubmatch(get(restarted, "/").Body.String())
	if oldLink == nil || newLink == nil || oldLink[1] == newLink[1] {
		t.Fatalf("page links must change with the stylesheet: %v %v", oldLink, newLink)
	}
}

func TestPagesLinkThemeBeforePaintAndExposeDefault(t *testing.T) {
	for mode, want := range map[string]string{"": "auto", "auto": "auto", "light": "light", "dark": "dark"} {
		h := themeHandler(t, theme.Config{Default: mode})
		for _, path := range []string{"/", "/?type=prefix&q=203.0.113.0/24&backend=all", "/neighbors", "/summary", "/communities"} {
			rr := get(h, path)
			if rr.Code != 200 {
				t.Fatalf("%s: %d", path, rr.Code)
			}
			body := rr.Body.String()
			if !strings.Contains(body, `<html lang="en" data-theme-default="`+want+`">`) {
				t.Errorf("%s (%q): data-theme-default %q missing", path, mode, want)
			}
			headEnd := strings.Index(body, "</head>")
			if headEnd < 0 {
				// Shared pages open <head> and <body> contiguously.
				headEnd = strings.Index(body, "<body>")
			}
			css, themeAt, initJS := strings.Index(body, `href="/static/app.css?v=`), strings.Index(body, `href="/static/theme.css?v=`), strings.Index(body, `src="/static/theme-init.js?v=`)
			if css < 0 || themeAt < 0 || initJS < 0 || !(css < themeAt && themeAt < initJS && initJS < headEnd) {
				t.Errorf("%s: stylesheets then blocking theme-init must be in <head> (%d %d %d %d)", path, css, themeAt, initJS, headEnd)
			}
			if m := regexp.MustCompile(`<script src="/static/theme-init\.js[^>]*>`).FindString(body); strings.Contains(m, "defer") || strings.Contains(m, "async") {
				t.Errorf("%s: theme-init must be synchronous, got %s", path, m)
			}
			if !strings.Contains(body, `id="theme"`) || !strings.Contains(body, `<script src="/static/app.js?v=`) {
				t.Errorf("%s: theme toggle or app.js missing", path)
			}
			if strings.Contains(body, "<style") || strings.Contains(body, " style=") || strings.Contains(body, "onclick=") {
				t.Errorf("%s: inline styles or handlers would violate the CSP", path)
			}
		}
	}
}

func TestVersionedAssetLinksResolve(t *testing.T) {
	h := themeHandler(t, theme.Config{})
	for _, m := range regexp.MustCompile(`(?:href|src)="(/static/[^"]+)"`).FindAllStringSubmatch(get(h, "/").Body.String(), -1) {
		if rr := get(h, strings.ReplaceAll(m[1], "&amp;", "&")); rr.Code != 200 || rr.Body.Len() == 0 {
			t.Errorf("%s: %d", m[1], rr.Code)
		}
	}
	if !bytes.Contains(get(h, "/static/theme-init.js").Body.Bytes(), []byte("localStorage")) {
		t.Fatal("theme-init.js not served")
	}
}

func TestHTMXFragmentsDoNotTouchThemeState(t *testing.T) {
	h := themeHandler(t, theme.Config{Default: "dark"})
	frag := get(h, "/?type=prefix&q=203.0.113.0/24&backend=all", "HX-Request", "true")
	if frag.Code != 200 || strings.Contains(frag.Body.String(), "<html") || strings.Contains(frag.Body.String(), "data-theme") || strings.Contains(frag.Body.String(), "theme.css") {
		t.Fatalf("a results fragment must leave <html data-theme> and the stylesheet untouched: %d", frag.Code)
	}
	// Browser history restore receives a full page that re-runs theme-init.
	restore := get(h, "/?type=prefix&q=203.0.113.0/24&backend=all", "HX-Request", "true", "HX-History-Restore-Request", "true")
	if !strings.Contains(restore.Body.String(), `data-theme-default="dark"`) || !strings.Contains(restore.Body.String(), "theme-init.js") {
		t.Fatal("history restore must return a full themed page")
	}
}

func TestStatusIndicatorsKeepTextLabels(t *testing.T) {
	h := themeHandler(t, theme.Config{})
	body := get(h, "/?type=prefix&q=203.0.113.0/24&backend=all").Body.String()
	for _, want := range []string{
		`<span class="pill good">BEST</span>`,
		`<span class="pill good">valid</span>`,    // OVS valid
		`<span class="pill bad">invalid</span>`,   // AVS invalid / OVS invalid
		`<span class="pill muted">unknown</span>`, // AVS unknown
		`<th>RPKI</th>`, `<th>ASPA</th>`,
		`class="pill good">COMPLETE`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status markup missing %s", want)
		}
	}
	if strings.Count(body, `class="pill bad">invalid`) != 2 {
		t.Errorf("OVS and AVS must stay separate columns with their own pill")
	}
}

func TestErrorPartialAndEmptyStatesUseStatusClasses(t *testing.T) {
	cfg, closeAll := testConfig(defaults(),
		backendSpec{id: "ok", name: "Empty", h: serveDoc(`[]`)},
		backendSpec{id: "bad", name: "Broken", h: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 502) }})
	defer closeAll()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	body := get(h.Routes(), "/?type=prefix&q=203.0.113.0/24&backend=all").Body.String()
	for _, want := range []string{`class="empty"`, "No matching routes", `<div class="notice error"><strong>Query failed`, `<div class="notice warn"><strong>Incomplete results`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if bad := get(h.Routes(), "/?type=prefix&q=nonsense&backend=all").Body.String(); !strings.Contains(bad, `class="notice error"`) {
		t.Error("validation error notice missing")
	}
}

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("static/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every color in the delivered UI is a token. The stylesheet and templates must
// not contain color literals, and every token they use must be defined for
// both palettes.
func TestNoHardcodedColorsAndAllTokensDefined(t *testing.T) {
	literal := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(|\b(?:gradient|box-shadow|text-shadow)\b|\b(?:white|black|red|green|blue|cyan|teal|gray|grey|orange|yellow|purple)\b\s*[;}!]`)
	css := readStatic(t, "app.css")
	// Comments may mention words; strip them before scanning.
	stripped := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	if m := literal.FindAllString(stripped, -1); len(m) > 0 {
		t.Fatalf("app.css contains color literals or decorative effects: %v", m)
	}
	for _, name := range []string{"app.js", "theme-init.js"} {
		if m := regexp.MustCompile(`#[0-9a-fA-F]{6}\b`).FindAllString(readStatic(t, name), -1); len(m) > 0 {
			t.Errorf("%s contains colors: %v", name, m)
		}
	}
	tmpl, _ := os.ReadDir("templates")
	for _, f := range tmpl {
		b, _ := os.ReadFile("templates/" + f.Name())
		if m := regexp.MustCompile(`#[0-9a-fA-F]{6}\b|style=`).FindAllString(string(b), -1); len(m) > 0 {
			t.Errorf("template %s contains colors or inline styles: %v", f.Name(), m)
		}
	}
	def, err := theme.Compile(theme.Config{})
	if err != nil {
		t.Fatal(err)
	}
	generated := string(def.CSS)
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`var\((--[a-z-]+)\)`).FindAllStringSubmatch(stripped, -1) {
		used[m[1]] = true
	}
	if len(used) < 15 {
		t.Fatalf("suspiciously few tokens used: %v", used)
	}
	for tok := range used {
		// Defined in every palette block: the plain rule, the dark media rule
		// and the explicit dark rule each declare it.
		if n := strings.Count(generated, "  "+tok+":"); n != 3 {
			t.Errorf("token %s is declared %d times, want once per palette block", tok, n)
		}
	}
}

func TestPillClassesAreStyledWithStatusTokens(t *testing.T) {
	css := readStatic(t, "app.css")
	for class, tok := range map[string]string{".good{": "--status-good-fg", ".bad{": "--status-bad-fg", ".pill.warn,": "--status-warn-fg", ".muted{": "--color-text-muted"} {
		i := strings.Index(css, class)
		if i < 0 {
			t.Fatalf("no rule for %s", class)
		}
		if rule := css[i : i+strings.Index(css[i:], "}")]; !strings.Contains(rule, tok) {
			t.Errorf("%s must use %s: %s", class, tok, rule)
		}
	}
	// Status colors must never be the accent.
	if m := regexp.MustCompile(`\.(?:good|bad|warn|muted)\b[^{]*\{[^}]*--color-accent`).FindString(css); m != "" {
		t.Errorf("a status class uses the accent: %s", m)
	}
}

func TestFocusAndDisabledStatesAreDefined(t *testing.T) {
	css := readStatic(t, "app.css")
	for _, want := range []string{":focus-visible{outline:2px solid var(--color-focus)", "input::placeholder{color:var(--color-text-muted)", ":disabled{background:var(--color-surface-alt);color:var(--color-text-muted)", "tbody tr:not(.details-row):hover td{background:var(--color-surface-hover)}"} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
	if strings.Contains(css, "outline:none") || strings.Contains(css, "outline:0") {
		t.Error("focus outlines must not be removed")
	}
}

func TestContrastWarningsAreLoggedNotFatal(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, nil))
	themeHandler(t, theme.Config{Light: theme.Palette{TextMuted: "#B0B0B0"}}, WithLogger(lg))
	out := buf.String()
	if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, "text_muted (#B0B0B0)") {
		t.Fatalf("expected a contrast warning, got %q", out)
	}
	buf.Reset()
	themeHandler(t, theme.Config{}, WithLogger(lg))
	if strings.Contains(buf.String(), "contrast") {
		t.Fatalf("built-in themes must not warn: %s", buf.String())
	}
}
