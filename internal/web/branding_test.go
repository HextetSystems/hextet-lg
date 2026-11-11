package web

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/HextetSystems/hextet-lg/internal/config"
	"github.com/HextetSystems/hextet-lg/internal/site"
)

// M4.9: operator-neutral branding and deployment identity.

func m49Handler(t *testing.T, edit func(*config.Config)) http.Handler {
	t.Helper()
	return m48Handler(t, edit,
		backendSpec{"a", "Alpha", serveDoc(ribJSON(3, false))},
		backendSpec{"b", "Beta", serveDoc(ribJSON(2, true))},
	)
}

func ref[T any](v T) *T { return &v }

var footerRE = regexp.MustCompile(`(?s)<footer>(.*?)</footer>`)
var siteIDRE = regexp.MustCompile(`(?s)<span class="site-id">(.*?)</span></footer>`)
var tagRE = regexp.MustCompile(`<[^>]*>`)

// footerText is the visible text of the site-id span, with timing removed.
func footerText(t *testing.T, body string) string {
	t.Helper()
	m := footerRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no footer")
	}
	i := strings.Index(m[1], `<span class="site-id">`)
	if i < 0 {
		t.Fatal("no site-id span")
	}
	right := m[1][i:]
	right = regexp.MustCompile(`(?s)<span id="page-timing".*?</span>`).ReplaceAllString(right, "")
	return strings.TrimSpace(strings.Join(strings.Fields(tagRE.ReplaceAllString(right, "")), " "))
}

var htmlPages = []string{"/", "/?type=prefix&q=10.0.0.0/24", "/?type=prefix&q=bad", "/neighbors", "/summary", "/rtr", "/communities"}

func TestGenericDefaultsNameNoOperator(t *testing.T) {
	h := m49Handler(t, func(c *config.Config) { c.Server.Title = "" })
	for _, target := range htmlPages {
		body := get(h, target).Body.String()
		for _, bad := range []string{"AS395089", "Hextet Systems", "HEXTET", "NETWORK OPERATIONS", "Hextet Looking Glass", "Hextet's"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: operator-specific text %q in the default page", target, bad)
			}
		}
		mustContain(t, body, `<span class="brand-name">BGP Looking Glass</span>`, `<small>Network Routing Information</small>`, "Powered by Hextet LG", "Read-only queries", `<link rel="icon" href="/branding/favicon?v=`)
		if got := footerText(t, body); got != "Powered by Hextet LG" {
			t.Errorf("%s: footer %q", target, got)
		}
		if strings.Contains(body, `class="logo"`) || strings.Contains(body, "mailto:") || strings.Contains(body, `rel="noopener`) {
			t.Errorf("%s: unconfigured logo or links rendered", target)
		}
		if !strings.Contains(body, `<span class="mark" aria-hidden="true">B</span>`) {
			t.Errorf("%s: letter mark missing", target)
		}
	}
	if got := titleOf(t, get(h, "/").Body.String()); got != "Route Explorer · BGP Looking Glass" {
		t.Errorf("default title %q", got)
	}
}

func TestLegacyServerTitleStillNamesTheSite(t *testing.T) {
	h := m49Handler(t, nil) // testConfig sets server.title "Test LG"
	body := get(h, "/").Body.String()
	mustContain(t, body, `<span class="brand-name">Test LG</span>`)
	if got := titleOf(t, body); got != "Route Explorer · Test LG" {
		t.Errorf("title %q", got)
	}
}

func TestSiteNameWinsOverServerTitle(t *testing.T) {
	h := m49Handler(t, func(c *config.Config) { c.Site.Name = "Example LG"; c.Server.Title = "Legacy" })
	body := get(h, "/").Body.String()
	mustContain(t, body, `<span class="brand-name">Example LG</span>`)
	if strings.Contains(body, "Legacy") || titleOf(t, body) != "Route Explorer · Example LG" {
		t.Errorf("legacy title leaked: %q", titleOf(t, body))
	}
}

func TestCustomIdentityRenders(t *testing.T) {
	h := m49Handler(t, func(c *config.Config) {
		c.Site = site.Config{
			Name: "Example Networks LG", Subtitle: ref("Looking glass for AS64500"), Operator: "Example Networks", ASN: 64500,
			Website: "https://example.net", Contact: "noc@example.net", Branding: site.Branding{Footer: ref("Run with care")},
		}
	})
	for _, target := range htmlPages {
		body := get(h, target).Body.String()
		mustContain(t, body,
			`<span class="brand-name">Example Networks LG</span>`, `<small>Looking glass for AS64500</small>`,
			`<a href="https://example.net" rel="noopener noreferrer">Example Networks</a>`,
			`<a href="mailto:noc@example.net">noc@example.net</a>`)
		if got := footerText(t, body); got != "Example Networks · AS64500 · noc@example.net · Run with care" {
			t.Errorf("%s: footer %q", target, got)
		}
		if strings.Count(body, "Example Networks</a>") != 1 || strings.Count(body, "mailto:") != 1 {
			t.Errorf("%s: operator identity repeated", target)
		}
		if titleOf(t, body) == "" || !strings.HasSuffix(titleOf(t, body), " · Example Networks LG") {
			t.Errorf("%s: title %q", target, titleOf(t, body))
		}
	}
}

func TestFooterHasNoStraySeparators(t *testing.T) {
	cases := map[string]struct {
		c    site.Config
		want string
	}{
		"nothing at all":   {site.Config{Branding: site.Branding{Footer: ref("")}}, ""},
		"asn only":         {site.Config{ASN: 64500, Branding: site.Branding{Footer: ref("")}}, "AS64500"},
		"operator no asn":  {site.Config{Operator: "Acme", Branding: site.Branding{Footer: ref("")}}, "Acme"},
		"asn no operator":  {site.Config{ASN: 64500}, "AS64500 · Powered by Hextet LG"},
		"website only":     {site.Config{Website: "https://example.net"}, "example.net · Powered by Hextet LG"},
		"contact only":     {site.Config{Contact: "a@example.net", Branding: site.Branding{Footer: ref("x")}}, "a@example.net · x"},
		"operator, footer": {site.Config{Operator: "Acme"}, "Acme · Powered by Hextet LG"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := m49Handler(t, func(c *config.Config) { c.Site = tc.c })
			body := get(h, "/").Body.String()
			if got := footerText(t, body); got != tc.want {
				t.Errorf("footer %q, want %q", got, tc.want)
			}
			if timingCount(body) != 1 || !strings.Contains(body, "Generated in") {
				t.Fatal("timing missing")
			}
			// The timing separator appears exactly when something precedes it.
			sep := strings.Contains(body, `class="timing"`) && strings.Contains(body, `> · Generated in`)
			if sep != (tc.want != "") {
				t.Errorf("timing separator=%v with footer %q", sep, tc.want)
			}
			if strings.Contains(footerRE.FindString(body), "· ·") || strings.Contains(footerRE.FindString(body), `<span class="dot">·</span> <span class="dot">`) {
				t.Errorf("duplicate separators: %s", footerRE.FindString(body))
			}
		})
	}
}

func TestOperatorTextIsEscapedEverywhere(t *testing.T) {
	evil := `<img src=x onerror=alert(1)>"'&`
	h := m49Handler(t, func(c *config.Config) {
		c.Site = site.Config{Name: evil, Subtitle: ref(evil), Operator: evil, Branding: site.Branding{Footer: ref(evil)}}
	})
	for _, target := range htmlPages {
		body := get(h, target).Body.String()
		if strings.Contains(body, "<img src=x") || strings.Contains(body, "onerror=alert(1)>") && !strings.Contains(body, "&lt;img src=x onerror") {
			t.Fatalf("%s: unescaped operator text", target)
		}
		if !strings.Contains(body, "&lt;img src=x onerror=alert(1)&gt;") {
			t.Errorf("%s: escaped text missing", target)
		}
		if !strings.Contains(titleOf(t, body), "<img") {
			t.Errorf("%s: title text lost: %q", target, titleOf(t, body))
		}
	}
	// The footer is plain text, not template syntax or Markdown.
	h = m49Handler(t, func(c *config.Config) {
		c.Site.Branding.Footer = ref("{{.Title}} **bold** [x](http://y) {{template \"nav\"}}")
	})
	body := get(h, "/").Body.String()
	if !strings.Contains(body, "{{.Title}} **bold** [x](http://y) {{template &#34;nav&#34;}}") {
		t.Errorf("footer was interpreted: %s", footerText(t, body))
	}
}

func TestLongOperatorTextStaysBounded(t *testing.T) {
	long := strings.Repeat("W", site.MaxNameRunes)
	h := m49Handler(t, func(c *config.Config) {
		c.Site = site.Config{Name: long, Operator: strings.Repeat("O", site.MaxOperatorRunes), Branding: site.Branding{Footer: ref(strings.Repeat("F", site.MaxFooterRunes))}}
	})
	body := get(h, "/?type=prefix&q=10.0.0.0/24").Body.String()
	if got := titleOf(t, body); got != "10.0.0.0/24 · "+long {
		t.Errorf("title %q", got)
	}
	// An over-long legacy title is truncated, not rejected.
	h = m49Handler(t, func(c *config.Config) { c.Server.Title = strings.Repeat("L", 500) })
	if got := titleOf(t, get(h, "/").Body.String()); len([]rune(got)) > len("Route Explorer · ")+site.MaxNameRunes {
		t.Errorf("legacy title unbounded: %d", len(got))
	}
}

// --- assets ---

func writeLogo(t *testing.T, dir, name string, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestBuiltinFavicon(t *testing.T) {
	h := m49Handler(t, nil)
	rr := get(h, "/branding/favicon")
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("%d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	if !strings.Contains(rr.Body.String(), "#2563EB") {
		t.Error("built-in icon should use the default theme accent")
	}
	if !strings.Contains(rr.Header().Get("Content-Security-Policy"), "sandbox") || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("svg must be served inert: %v", rr.Header())
	}
	if rr.Header().Get("Cache-Control") != "no-cache" || rr.Header().Get("ETag") == "" {
		t.Errorf("unversioned: %v", rr.Header())
	}
	// The page links the versioned URL, which is immutable.
	m := regexp.MustCompile(`<link rel="icon" href="(/branding/favicon\?v=[0-9a-f]+)" type="image/svg&#43;xml">`).FindStringSubmatch(get(h, "/").Body.String())
	if m == nil {
		t.Fatal("favicon link missing")
	}
	v := get(h, m[1])
	if v.Code != 200 || v.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("versioned: %d %v", v.Code, v.Header())
	}
	if nm := get(h, m[1], "If-None-Match", v.Header().Get("ETag")); nm.Code != http.StatusNotModified {
		t.Errorf("conditional request: %d", nm.Code)
	}
	if ico := get(h, "/favicon.ico"); ico.Code != 200 || !bytes.Equal(ico.Body.Bytes(), rr.Body.Bytes()) {
		t.Errorf("/favicon.ico: %d", ico.Code)
	}
	if get(h, "/branding/logo").Code != http.StatusNotFound {
		t.Error("no logo is configured: /branding/logo must be 404")
	}
	// Built-in icon follows the configured accent.
	h2 := m49Handler(t, func(c *config.Config) { c.Theme.Accent = "#C2410C" })
	if !strings.Contains(get(h2, "/branding/favicon").Body.String(), "#C2410C") {
		t.Error("favicon ignores theme accent")
	}
}

func TestCustomLogoAndFavicon(t *testing.T) {
	dir := t.TempDir()
	logo := writeLogo(t, dir, "logo.png", 240, 60)
	fav := writeLogo(t, dir, "fav.png", 32, 32)
	h := m49Handler(t, func(c *config.Config) {
		c.Site = site.Config{Name: "Example LG", Branding: site.Branding{Directory: dir, Logo: "logo.png", Favicon: "fav.png"}}
	})
	body := get(h, "/").Body.String()
	m := regexp.MustCompile(`<img class="logo" src="(/branding/logo\?v=[0-9a-f]+)" alt="" width="240" height="60">`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("logo markup missing: %s", regexp.MustCompile(`<header.*?</header>`).FindString(body))
	}
	if strings.Contains(body, `class="mark"`) {
		t.Error("letter mark should be replaced by the logo")
	}
	if !strings.Contains(body, `<span class="brand-name">Example LG</span>`) {
		t.Error("site name must stay as text next to the logo")
	}
	rr := get(h, m[1])
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rr.Body.Bytes(), logo) || rr.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("logo: %d %v", rr.Code, rr.Header())
	}
	if strings.Contains(rr.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Error("raster images need no sandbox policy")
	}
	fr := get(h, "/branding/favicon")
	if fr.Code != 200 || fr.Header().Get("Content-Type") != "image/png" || !bytes.Equal(fr.Body.Bytes(), fav) {
		t.Errorf("favicon: %d %v", fr.Code, fr.Header())
	}
	mustContain(t, body, `type="image/png"`)
	// A page of every kind carries the same icon and logo.
	for _, target := range htmlPages {
		b := get(h, target).Body.String()
		if !strings.Contains(b, m[1]) || !strings.Contains(b, `/branding/favicon?v=`) {
			t.Errorf("%s: branding assets not linked", target)
		}
	}
	if get(h, "/branding/logo?v=stale").Header().Get("Cache-Control") != "no-cache" {
		t.Error("a stale version must not be cached immutably")
	}
	if rr := get(h, "/branding/logo", "If-None-Match", rr.Header().Get("ETag")); rr.Code != http.StatusNotModified {
		t.Errorf("etag: %d", rr.Code)
	}
}

func TestSVGLogoIsServedInert(t *testing.T) {
	dir := t.TempDir()
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 90 30"><rect width="90" height="30"/></svg>`
	if err := os.WriteFile(filepath.Join(dir, "logo.svg"), []byte(svg), 0o644); err != nil {
		t.Fatal(err)
	}
	h := m49Handler(t, func(c *config.Config) { c.Site.Branding = site.Branding{Directory: dir, Logo: "logo.svg"} })
	body := get(h, "/").Body.String()
	if strings.Contains(body, "<svg") || strings.Contains(body, "<rect") {
		t.Fatal("SVG must never be inlined into a page")
	}
	rr := get(h, "/branding/logo")
	if rr.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(rr.Header().Get("Content-Security-Policy"), "default-src 'none'") || !strings.Contains(rr.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("headers: %v", rr.Header())
	}
	mustContain(t, body, `width="90" height="30"`)
}

func TestBrandingStartupFailures(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "evil.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]site.Branding{
		"missing file":      {Directory: dir, Logo: "missing.png"},
		"missing directory": {Directory: filepath.Join(dir, "nope"), Favicon: "x.png"},
		"script svg":        {Directory: dir, Logo: "evil.svg"},
		"traversal":         {Directory: dir, Logo: "../x.png"},
	} {
		cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", serveDoc(ribJSON(1, false))})
		cfg.Site.Branding = b
		if _, err := NewHandler(cfg); err == nil {
			t.Errorf("%s: handler started", name)
		}
		closeAll()
	}
}

func TestBrandingURLsCannotReachOtherFiles(t *testing.T) {
	dir := t.TempDir()
	writeLogo(t, dir, "logo.png", 10, 10)
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := m49Handler(t, func(c *config.Config) { c.Site.Branding = site.Branding{Directory: dir, Logo: "logo.png"} })
	for _, target := range []string{
		"/branding/", "/branding/secret.txt", "/branding/logo.png", "/branding/../secret.txt", "/branding/%2e%2e/secret.txt",
		"/branding/logo/../../etc/passwd", "/branding/..%2fsecret.txt", "/branding/logo/secret.txt", "/branding/favicon/x", "/branding/favicon?file=secret.txt",
		"/branding/logo?path=/etc/passwd", "/branding//etc/passwd",
	} {
		rr := get(h, target)
		if strings.Contains(rr.Body.String(), "TOPSECRET") || strings.Contains(rr.Body.String(), "root:") {
			t.Errorf("%s leaked a file", target)
		}
		if target != "/branding/favicon?file=secret.txt" && target != "/branding/logo?path=/etc/passwd" && rr.Code == 200 && rr.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s served %q", target, rr.Header().Get("Content-Type"))
		}
	}
	// Extra parameters are ignored: the response is still the configured asset.
	if rr := get(h, "/branding/logo?path=/etc/passwd"); rr.Header().Get("Content-Type") != "image/png" {
		t.Errorf("param changed the asset: %v", rr.Header())
	}
	// The stylesheet and embedded static files are unaffected.
	if get(h, "/static/app.css").Code != 200 || get(h, "/static/theme.css").Code != 200 {
		t.Error("static assets broken")
	}
}

func TestBrandingRouteIsAFixedMetricsLabel(t *testing.T) {
	for path, want := range map[string]string{"/branding/logo": "branding", "/branding/favicon": "branding", "/favicon.ico": "branding", "/branding/anything-else": "branding", "/static/app.css": "static", "/x": "other"} {
		if got := routeName(path); got != want {
			t.Errorf("routeName(%q)=%q want %q", path, got, want)
		}
	}
}

// --- titles, HTMX and history with a custom site name ---

func TestCustomSiteNameInEveryTitle(t *testing.T) {
	h := m49Handler(t, func(c *config.Config) { c.Site.Name = "Example Networks LG" })
	long := strings.Repeat("x", 300)
	for target, want := range map[string]string{
		"/":                                  "Route Explorer · Example Networks LG",
		"/?type=asn&q=395611&backend=all":    "AS395611 · Example Networks LG",
		"/?type=prefix&q=192.0.2.0/24":       "192.0.2.0/24 · Example Networks LG",
		"/?community=64500:1&backend=all":    "Community 64500:1 · Example Networks LG",
		"/?neighbor=192.0.2.1&backend=a":     "Neighbor 192.0.2.1 @ Alpha · Example Networks LG",
		"/?type=large-community&q=64500:1:2": "64500:1:2 · Example Networks LG",
		"/summary":                           "BGP Summary · Example Networks LG",
		"/neighbors":                         "BGP Neighbors · Example Networks LG",
		"/rtr":                               "RTR Sessions · Example Networks LG",
		"/communities":                       "Communities · Example Networks LG",
		"/?type=prefix&q=garbage":            "Invalid search · Example Networks LG",
		"/?type=prefix&q=" + long:            "Invalid search · Example Networks LG",
		"/?type=asn&q=64500&backend=nope":    "Invalid search · Example Networks LG",
		"/?prefix=10.0.0.0/24&ovs=valid&best=1&backend=all": "10.0.0.0/24 +2 filters · Example Networks LG",
	} {
		rr := get(h, target)
		if got := titleOf(t, rr.Body.String()); got != want {
			t.Errorf("%s: %q, want %q", target, got, want)
		}
	}
}

func TestHTMXAndHistoryWithBranding(t *testing.T) {
	dir := t.TempDir()
	writeLogo(t, dir, "logo.png", 80, 20)
	h := m49Handler(t, func(c *config.Config) {
		c.Site = site.Config{Name: "Example LG", Operator: "Example Networks", ASN: 64500, Branding: site.Branding{Directory: dir, Logo: "logo.png"}}
	})
	target := "/?type=asn&q=64500&backend=a&view=table"
	frag := get(h, target, "HX-Request", "true").Body.String()
	restore := get(h, target, "HX-Request", "true", "HX-History-Restore-Request", "true").Body.String()
	if !strings.HasPrefix(frag, "<title>AS64500 @ Alpha · Example LG</title>") || titleOf(t, frag) != "AS64500 @ Alpha · Example LG" {
		t.Errorf("fragment title: %.120s", frag)
	}
	// A fragment carries no header or branding: the header is never swapped,
	// so the logo and favicon cannot flicker.
	for _, bad := range []string{`class="brand"`, `class="logo"`, `rel="icon"`, "<header", "Example Networks"} {
		if strings.Contains(frag, bad) {
			t.Errorf("fragment contains %q", bad)
		}
	}
	if timingCount(frag) != 1 || strings.Count(frag, "hx-swap-oob") != 2 { // context + timing
		t.Errorf("oob updates: timing=%d oob=%d", timingCount(frag), strings.Count(frag, "hx-swap-oob"))
	}
	// History restoration is a complete page with the branding and one title.
	if strings.Contains(restore, "hx-swap-oob") && !strings.Contains(restore, "<html") {
		t.Fatal("history restore must be a full page")
	}
	mustContain(t, restore, "<html", `class="logo"`, `rel="icon"`)
	if got := footerText(t, restore); got != "Example Networks · AS64500 · Powered by Hextet LG" {
		t.Errorf("restore footer %q", got)
	}
	if titleOf(t, restore) != "AS64500 @ Alpha · Example LG" || strings.Contains(restore, `hx-swap-oob="true"`) {
		t.Errorf("restore: title %q", titleOf(t, restore))
	}
	if timingCount(restore) != 1 {
		t.Errorf("restore timing %d", timingCount(restore))
	}
}

func TestOperatorASNIsDisplayOnly(t *testing.T) {
	var gotQuery []string
	spec := backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = append(gotQuery, r.URL.RequestURI())
		_, _ = w.Write([]byte(ribJSON(2, false)))
	}}
	plain := m48Handler(t, nil, spec)
	get(plain, "/?type=prefix&q=10.0.0.0/24&backend=a")
	get(plain, "/?type=asn&q=64500&backend=a")
	base := append([]string(nil), gotQuery...)
	gotQuery = nil

	branded := m48Handler(t, func(c *config.Config) {
		c.Site = site.Config{Name: "X", Operator: "Y", ASN: 395089, Website: "https://example.net", Contact: "a@example.net"}
	}, spec)
	r1 := get(branded, "/?type=prefix&q=10.0.0.0/24&backend=a")
	get(branded, "/?type=asn&q=64500&backend=a")
	if strings.Join(base, "|") != strings.Join(gotQuery, "|") {
		t.Fatalf("branding changed backend queries:\n%v\n%v", base, gotQuery)
	}
	if strings.Contains(strings.Join(gotQuery, ""), "395089") {
		t.Fatal("operator ASN reached a backend request")
	}
	// Search pages never mention the operator ASN outside the footer.
	body := r1.Body.String()
	if i := strings.Index(body, "<footer>"); strings.Contains(body[:i], "AS395089") {
		t.Error("operator ASN rendered outside the footer")
	}
}

func TestBrandingDoesNotChangeFeaturesOrPrivacy(t *testing.T) {
	h := m49Handler(t, func(c *config.Config) {
		c.Site = site.Config{Name: "X", Operator: "Y", ASN: 64500}
	})
	body := get(h, "/?type=prefix&q=10.0.0.0/24&backend=a").Body.String()
	if strings.Contains(body, "Raw JSON") {
		t.Error("raw JSON must stay off by default")
	}
	mustContain(t, body, `class="disclosures"`, "Route details", `<tbody class="route">`)
	if get(h, "/healthz").Body.String() != "ok\n" {
		t.Error("healthz")
	}
}
