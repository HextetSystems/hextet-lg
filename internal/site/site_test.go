package site

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func str(s string) *string { return &s }

func TestDefaultsAreOperatorNeutral(t *testing.T) {
	s, err := Compile(Config{}, "", "#2563EB")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != DefaultName || s.Subtitle != DefaultSubtitle || s.Operator != "" || s.ASN != 0 || s.Website != "" || s.Contact != "" || s.Logo != nil {
		t.Fatalf("defaults: %+v", s)
	}
	if len(s.Footer) != 1 || s.Footer[0].Text != DefaultFooter || s.Footer[0].URL != "" {
		t.Fatalf("footer: %+v", s.Footer)
	}
	if s.Favicon == nil || !s.Favicon.Builtin || s.Favicon.ContentType != "image/svg+xml" || !strings.Contains(string(s.Favicon.Data), "#2563EB") {
		t.Fatalf("built-in favicon: %+v", s.Favicon)
	}
	if s.ASNLabel() != "" {
		t.Fatal("unset ASN must have no label")
	}
	if strings.Contains(strings.ToLower(s.Name+s.Subtitle+s.Operator), "hextet") {
		t.Fatal("default identity names an operator")
	}
	// The built-in icon is itself valid under the SVG rules.
	if _, _, err := validateSVG(s.Favicon.Data); err != nil {
		t.Fatalf("built-in favicon fails SVG validation: %v", err)
	}
}

func TestNamePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, legacy, want string
		warn               bool
	}{
		{"", "", DefaultName, false},
		{"", "Legacy LG", "Legacy LG", false},
		{"New LG", "", "New LG", false},
		{"New LG", "Legacy LG", "New LG", true},
		{"Same", "Same", "Same", false},
		{"", "  \x07\x00  ", DefaultName, false},
		{"", strings.Repeat("é", 200), strings.Repeat("é", MaxNameRunes), false},
	} {
		s, err := Compile(Config{Name: tc.name}, tc.legacy, "#2563EB")
		if err != nil {
			t.Fatal(err)
		}
		if s.Name != tc.want || (len(s.Warnings) > 0) != tc.warn {
			t.Errorf("name=%q legacy=%q: got %q warnings=%v", tc.name, tc.legacy, s.Name, s.Warnings)
		}
	}
}

func TestSubtitleAndFooterExplicitEmpty(t *testing.T) {
	s, err := Compile(Config{Subtitle: str(""), Branding: Branding{Footer: str("")}}, "", "#2563EB")
	if err != nil {
		t.Fatal(err)
	}
	if s.Subtitle != "" || len(s.Footer) != 0 {
		t.Fatalf("explicit empty must remove them: %+v", s)
	}
}

func TestFooterParts(t *testing.T) {
	type p = Part
	for _, tc := range []struct {
		name string
		c    Config
		want []p
	}{
		{"operator only", Config{Operator: "Example Networks", Branding: Branding{Footer: str("")}}, []p{{Text: "Example Networks"}}},
		{"operator, asn, footer", Config{Operator: "Example Networks", ASN: 64500}, []p{{Text: "Example Networks"}, {Text: "AS64500"}, {Text: DefaultFooter}}},
		{"operator linked", Config{Operator: "Example", Website: "https://Example.net/", Branding: Branding{Footer: str("")}}, []p{{Text: "Example", URL: "https://example.net/", External: true}}},
		{"website without operator shows host", Config{Website: "https://example.net", Branding: Branding{Footer: str("")}}, []p{{Text: "example.net", URL: "https://example.net", External: true}}},
		{"asn only", Config{ASN: 64500, Branding: Branding{Footer: str("")}}, []p{{Text: "AS64500"}}},
		{"contact", Config{Contact: "noc@example.net", Branding: Branding{Footer: str("")}}, []p{{Text: "noc@example.net", URL: "mailto:noc@example.net"}}},
		{"custom footer", Config{Branding: Branding{Footer: str("Run by volunteers")}}, []p{{Text: "Run by volunteers"}}},
	} {
		s, err := Compile(tc.c, "", "#2563EB")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(s.Footer) != len(tc.want) {
			t.Fatalf("%s: got %+v", tc.name, s.Footer)
		}
		for i := range tc.want {
			if s.Footer[i] != tc.want[i] {
				t.Errorf("%s[%d]: got %+v want %+v", tc.name, i, s.Footer[i], tc.want[i])
			}
		}
	}
}

func TestValidationRejects(t *testing.T) {
	long := strings.Repeat("a", 300)
	for name, c := range map[string]Config{
		"name too long":        {Name: strings.Repeat("n", MaxNameRunes+1)},
		"name control char":    {Name: "a\x1bb"},
		"name newline":         {Name: "a\nb"},
		"subtitle too long":    {Subtitle: str(strings.Repeat("s", MaxSubtitleRunes+1))},
		"operator control":     {Operator: "a\tb"},
		"footer too long":      {Branding: Branding{Footer: str(strings.Repeat("f", MaxFooterRunes+1))}},
		"reserved asn":         {ASN: 23456},
		"website javascript":   {Website: "javascript:alert(1)"},
		"website data":         {Website: "data:text/html,x"},
		"website relative":     {Website: "/about"},
		"website no scheme":    {Website: "example.net"},
		"website ftp":          {Website: "ftp://example.net"},
		"website credentials":  {Website: "https://user:pw@example.net"},
		"website space":        {Website: "https://exa mple.net"},
		"website no host":      {Website: "https:///path"},
		"website long":         {Website: "https://example.net/" + long + long},
		"website non-ascii":    {Website: "https://exämple.net"},
		"contact two at":       {Contact: "a@b@example.net"},
		"contact name form":    {Contact: "NOC <noc@example.net>"},
		"contact list":         {Contact: "a@example.net,b@example.net"},
		"contact no domain":    {Contact: "noc@localhost"},
		"contact quote":        {Contact: `"a b"@example.net`},
		"contact empty local":  {Contact: "@example.net"},
		"contact dot local":    {Contact: ".a@example.net"},
		"contact bad label":    {Contact: "a@-example.net"},
		"contact scheme":       {Contact: "mailto:noc@example.net"},
		"contact newline":      {Contact: "a@example.net\nBcc: x@y.z"},
		"logo without dir":     {Branding: Branding{Logo: "logo.png"}},
		"favicon without dir":  {Branding: Branding{Favicon: "f.ico"}},
		"logo path":            {Branding: Branding{Directory: "/x", Logo: "sub/logo.png"}},
		"logo traversal":       {Branding: Branding{Directory: "/x", Logo: "../logo.png"}},
		"logo dotdot":          {Branding: Branding{Directory: "/x", Logo: ".."}},
		"logo absolute":        {Branding: Branding{Directory: "/x", Logo: "/etc/passwd"}},
		"logo backslash":       {Branding: Branding{Directory: "/x", Logo: `..\logo.png`}},
		"logo type":            {Branding: Branding{Directory: "/x", Logo: "logo.bmp"}},
		"logo ico not allowed": {Branding: Branding{Directory: "/x", Logo: "logo.ico"}},
		"logo html":            {Branding: Branding{Directory: "/x", Logo: "logo.html"}},
		"logo no extension":    {Branding: Branding{Directory: "/x", Logo: "logo"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(c); err == nil {
				t.Fatalf("accepted %+v", c)
			}
			if _, err := Compile(c, "", "#2563EB"); err == nil {
				t.Fatal("Compile accepted it")
			}
		})
	}
}

func TestValidationAccepts(t *testing.T) {
	for name, c := range map[string]Config{
		"https":        {Website: "https://example.net"},
		"http":         {Website: "http://example.net/about?x=1"},
		"upper scheme": {Website: "HTTPS://example.net"},
		"ipv6 host":    {Website: "https://[2001:db8::1]/"},
		"plus contact": {Contact: "noc+lg@sub.example.net"},
		"unicode name": {Name: "Réseau Ünïcode 路由"},
		"asn":          {ASN: 4200000000},
		"markup text":  {Operator: "<b>x</b> & y"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(c); err != nil {
				t.Fatal(err)
			}
		})
	}
	if s, _ := Compile(Config{Website: "HTTPS://Example.NET"}, "", "#2563EB"); s.Website != "https://example.net" {
		t.Fatalf("scheme not canonicalized: %q", s.Website)
	}
}

func TestASNYAML(t *testing.T) {
	for in, want := range map[string]ASN{"64500": 64500, `"AS64500"`: 64500, `"as64500"`: 64500, `"64500"`: 64500, `""`: 0} {
		var c Config
		if err := yaml.Unmarshal([]byte("asn: "+in), &c); err != nil || c.ASN != want {
			t.Errorf("%s: %v %v", in, c.ASN, err)
		}
	}
	for _, in := range []string{"-1", "4294967296", `"AS"`, `"ASX"`, "1.5", "[1]", "true"} {
		var c Config
		if err := yaml.Unmarshal([]byte("asn: "+in), &c); err == nil {
			t.Errorf("%s accepted as %d", in, c.ASN)
		}
	}
}

// --- assets ---

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.NRGBA{1, 2, 3, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

const goodSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 40"><defs><linearGradient id="g"/></defs><rect width="120" height="40" fill="url(#g)"/><text x="4" y="20">Logo</text></svg>`

func write(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func compileWith(dir string, logo, favicon string) (*Site, error) {
	return Compile(Config{Branding: Branding{Directory: dir, Logo: logo, Favicon: favicon}}, "", "#2563EB")
}

func TestLoadsSupportedAssets(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "logo.png", pngBytes(t, 200, 50))
	var jb, gb bytes.Buffer
	_ = jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 40, 20)), nil)
	_ = gif.Encode(&gb, image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.Black, color.White}), nil)
	write(t, dir, "a.JPG", jb.Bytes())
	write(t, dir, "a.gif", gb.Bytes())
	write(t, dir, "logo.svg", []byte(goodSVG))
	write(t, dir, "fav.ico", []byte{0, 0, 1, 0, 1, 0, 16, 16, 0, 0, 1, 0, 32, 0, 0, 0, 0, 0, 22, 0, 0, 0})
	for _, tc := range []struct {
		file, ct string
		w, h     int
	}{
		{"logo.png", "image/png", 200, 50}, {"a.JPG", "image/jpeg", 40, 20}, {"a.gif", "image/gif", 8, 8}, {"logo.svg", "image/svg+xml", 120, 40},
	} {
		s, err := compileWith(dir, tc.file, "")
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		a := s.Logo
		if a.ContentType != tc.ct || a.Width != tc.w || a.Height != tc.h || a.Version == "" || a.ETag != `"`+a.Version+`"` {
			t.Errorf("%s: %+v", tc.file, a)
		}
		if s.Favicon == nil || !s.Favicon.Builtin {
			t.Errorf("%s: favicon should stay built in", tc.file)
		}
	}
	s, err := compileWith(dir, "", "fav.ico")
	if err != nil || s.Favicon.ContentType != "image/x-icon" || s.Favicon.Builtin {
		t.Fatalf("ico favicon: %+v %v", s.Favicon, err)
	}
}

func TestRejectsBadAssets(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "fake.png", []byte("<html>not a png</html>"))
	write(t, dir, "wrongtype.png", func() []byte {
		var b bytes.Buffer
		_ = jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil)
		return b.Bytes()
	}())
	write(t, dir, "huge.png", append(pngBytes(t, 4, 4), make([]byte, maxLogoBytes+1)...))
	write(t, dir, "wide.png", pngBytes(t, maxPixels+1, 1))
	write(t, dir, "bad.ico", []byte("not an icon at all"))
	write(t, dir, "empty.svg", nil)
	if err := os.Mkdir(filepath.Join(dir, "dir.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	write(t, outside, "secret.png", pngBytes(t, 4, 4))
	if err := os.Symlink(filepath.Join(outside, "secret.png"), filepath.Join(dir, "link.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../etc/passwd", filepath.Join(dir, "passwd.png")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing.png", "fake.png", "wrongtype.png", "huge.png", "wide.png", "empty.svg", "dir.png", "link.png", "passwd.png"} {
		if _, err := compileWith(dir, name, ""); err == nil {
			t.Errorf("logo %s accepted", name)
		}
	}
	if _, err := compileWith(dir, "", "bad.ico"); err == nil {
		t.Error("bad ico accepted")
	}
	if _, err := compileWith(filepath.Join(dir, "nope"), "x.png", ""); err == nil {
		t.Error("missing directory accepted")
	}
	// A favicon over its (smaller) limit.
	write(t, dir, "bigfav.png", append(pngBytes(t, 4, 4), make([]byte, maxFaviconBytes+1)...))
	if _, err := compileWith(dir, "", "bigfav.png"); err == nil {
		t.Error("oversized favicon accepted")
	}
}

func TestSVGValidation(t *testing.T) {
	ns := ` xmlns="http://www.w3.org/2000/svg"`
	for name, doc := range map[string]string{
		"script element":    `<svg` + ns + `><script>alert(1)</script></svg>`,
		"onload":            `<svg` + ns + ` onload="alert(1)"/>`,
		"onclick child":     `<svg` + ns + `><rect onclick="x()"/></svg>`,
		"foreignObject":     `<svg` + ns + `><foreignObject><div/></foreignObject></svg>`,
		"external href":     `<svg` + ns + ` xmlns:x="http://www.w3.org/1999/xlink"><image href="https://evil.example/x.png"/></svg>`,
		"data href":         `<svg` + ns + `><image href="data:image/png;base64,AAAA"/></svg>`,
		"javascript href":   `<svg` + ns + `><a href="javascript:alert(1)"><rect/></a></svg>`,
		"style import":      `<svg` + ns + `><style>@import url(https://evil.example/x.css);</style></svg>`,
		"style url":         `<svg` + ns + `><style>rect{fill:url(https://evil.example/x)}</style></svg>`,
		"attr url":          `<svg` + ns + `><rect style="fill:url('http://evil.example')"/></svg>`,
		"animate":           `<svg` + ns + `><a><animate attributeName="href" to="javascript:alert(1)"/></a></svg>`,
		"entity":            `<!DOCTYPE svg [<!ENTITY x "y">]><svg` + ns + `>&x;</svg>`,
		"wrong root":        `<html` + ns + `/>`,
		"no namespace":      `<svg><rect/></svg>`,
		"wrong namespace":   `<svg xmlns="http://example.com/"/>`,
		"unclosed":          `<svg` + ns + `><rect>`,
		"not xml":           `<svg` + ns + `><rect></svg>`,
		"plain text":        `hello`,
		"trailing garbage":  `<svg` + ns + `/><`,
		"expression":        `<svg` + ns + `><rect style="width:expression(alert(1))"/></svg>`,
		"uppercase handler": `<svg` + ns + `><rect ONCLICK="x()"/></svg>`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := validateSVG([]byte(doc)); err == nil {
				t.Fatalf("accepted: %s", doc)
			}
		})
	}
	if w, h, err := validateSVG([]byte(goodSVG)); err != nil || w != 120 || h != 40 {
		t.Fatalf("good svg: %d %d %v", w, h, err)
	}
	if w, h, err := validateSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64px" height="16"/>`)); err != nil || w != 64 || h != 16 {
		t.Fatalf("width/height: %d %d %v", w, h, err)
	}
	if w, h, err := validateSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="100%" height="100%"/>`)); err != nil || w != 0 || h != 0 {
		t.Fatalf("unknown size: %d %d %v", w, h, err)
	}
}

func TestMark(t *testing.T) {
	for in, want := range map[string]string{"Example LG": "E", "  éclair": "É", "9 Networks": "9", "---": "L", "": "L", "路由": "路"} {
		if got := mark(in); got != want {
			t.Errorf("mark(%q)=%q want %q", in, got, want)
		}
	}
}

// Docker mounts the directory read-only (and often via in-root symlinks);
// standalone deployments use relative paths. All must load.
func TestAssetDirectoryAsMountedAndRelative(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "real.png", pngBytes(t, 64, 16))
	if err := os.Symlink("real.png", filepath.Join(dir, "logo.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	s, err := compileWith(dir, "logo.png", "")
	if err != nil || s.Logo.Width != 64 {
		t.Fatalf("read-only directory with in-root symlink: %v", err)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".", "./", "../" + filepath.Base(dir)} {
		if _, err := compileWith(rel, "real.png", ""); err != nil {
			t.Errorf("relative directory %q: %v", rel, err)
		}
	}
	// Trailing slashes and redundant elements in an absolute path are fine.
	if _, err := compileWith(dir+"/./", "real.png", ""); err != nil {
		t.Errorf("unclean absolute path: %v", err)
	}
}
