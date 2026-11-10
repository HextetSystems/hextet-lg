package theme

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, c Config) *Theme {
	t.Helper()
	th, err := Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	return th
}

// block returns the declarations of the rule whose selector is exactly sel.
func block(t *testing.T, css, sel string) string {
	t.Helper()
	i := strings.Index(css, "\n"+sel+"{")
	if i < 0 {
		i = strings.Index(css, "  "+sel+"{")
	}
	if i < 0 {
		t.Fatalf("no rule %q in:\n%s", sel, css)
	}
	rest := css[i:]
	return rest[:strings.Index(rest, "}")]
}

func token(t *testing.T, blk, name string) rgb {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(name) + `:(#[0-9A-F]{6});`).FindStringSubmatch(blk)
	if m == nil {
		t.Fatalf("token %s missing in %s", name, blk)
	}
	c, _ := parseHex(m[1])
	return c
}

func TestMissingThemeUsesDefaultsWithoutWarnings(t *testing.T) {
	th := mustCompile(t, Config{})
	if th.Default != ModeAuto {
		t.Fatalf("default mode %q", th.Default)
	}
	if len(th.Warnings) != 0 {
		t.Fatalf("built-in palettes must meet the contrast targets: %v", th.Warnings)
	}
	css := string(th.CSS)
	for _, want := range []string{"--color-background:#F7F8FA;", "--color-surface:#FFFFFF;", "--color-background:#111316;", "--color-surface:#1B1E23;", "--color-accent:#2563EB;"} {
		if !strings.Contains(css, want) {
			t.Errorf("default CSS lacks %s", want)
		}
	}
}

func TestEveryPaletteDefinesTheSameTokens(t *testing.T) {
	th := mustCompile(t, Config{})
	css := string(th.CSS)
	names := func(blk string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`(--[a-z-]+):`).FindAllStringSubmatch(blk, -1) {
			out = append(out, m[1])
		}
		return out
	}
	light := names(block(t, css, ":root"))
	dark := names(block(t, css, `:root[data-theme="dark"]`))
	if len(light) < 20 || strings.Join(light, ",") != strings.Join(dark, ",") {
		t.Fatalf("palettes must define the same tokens in the same order:\nlight %v\ndark  %v", light, dark)
	}
}

func TestSingleAccentIsSharedAndOverridable(t *testing.T) {
	th := mustCompile(t, Config{Accent: "#0d9488"})
	css := string(th.CSS)
	if n := strings.Count(css, "--color-accent:#0D9488;"); n != 3 { // light, dark (media), dark (explicit)
		t.Fatalf("accent must be normalized and appear once per generated palette block, got %d\n%s", n, css)
	}
	if strings.Contains(css, "#2563EB") {
		t.Fatal("default accent leaked into a themed stylesheet")
	}
	// Palette values remain the built-in defaults.
	if !strings.Contains(css, "--color-background:#F7F8FA;") || !strings.Contains(css, "--color-background:#111316;") {
		t.Fatal("accent-only configuration must keep both default palettes")
	}
}

func TestCustomPalettesOverrideIndependently(t *testing.T) {
	th := mustCompile(t, Config{
		Light: Palette{Background: "#ffffff", Border: "#c0c0c0"},
		Dark:  Palette{Surface: "#101010", Text: "#FFFFFF"},
	})
	css := string(th.CSS)
	light := block(t, css, ":root")
	dark := block(t, css, `:root[data-theme="dark"]`)
	for _, want := range []string{"--color-background:#FFFFFF;", "--color-border:#C0C0C0;", "--color-surface:#FFFFFF;"} {
		if !strings.Contains(light, want) {
			t.Errorf("light block lacks %s", want)
		}
	}
	for _, want := range []string{"--color-surface:#101010;", "--color-text:#FFFFFF;", "--color-background:#111316;", "--color-border:#30353C;"} {
		if !strings.Contains(dark, want) {
			t.Errorf("dark block lacks %s", want)
		}
	}
	if !strings.Contains(light, "--color-text:#252B32;") {
		t.Error("untouched light values must keep their defaults")
	}
}

func TestDefaultModeSelectsCascade(t *testing.T) {
	cases := map[string][]string{
		"":      {"@media (prefers-color-scheme:dark)", `:root:not([data-theme="light"])`, `:root[data-theme="dark"]{`},
		"auto":  {"@media (prefers-color-scheme:dark)", `:root:not([data-theme="light"])`, `:root[data-theme="dark"]{`},
		"light": {`:root[data-theme="dark"]{`},
		"dark":  {`:root[data-theme="light"]{`},
	}
	for mode, wants := range cases {
		th := mustCompile(t, Config{Default: mode})
		css := string(th.CSS)
		for _, w := range wants {
			if !strings.Contains(css, w) {
				t.Errorf("mode %q lacks %q", mode, w)
			}
		}
		first := block(t, "\n"+css[strings.Index(css, "\n")+1:], ":root")
		wantScheme := map[string]string{"": "light", "auto": "light", "light": "light", "dark": "dark"}[mode]
		if !strings.Contains(first, "color-scheme:"+wantScheme+";") {
			t.Errorf("mode %q: plain :root must be the %s palette", mode, wantScheme)
		}
		if mode == "light" || mode == "dark" {
			if strings.Contains(css, "prefers-color-scheme") {
				t.Errorf("explicit %s default must not follow the system preference", mode)
			}
		}
		if mode == "" && th.Default != "auto" {
			t.Errorf("empty default must normalize to auto, got %q", th.Default)
		}
	}
}

func TestInvalidConfigurationIsRejected(t *testing.T) {
	bad := []string{
		"red", "#fff", "#FFFFF", "#FFFFFFF", "#GGGGGG", "FFFFFF", "0xFFFFFF", "#ffffff80", " #FFFFFF", "#FFFFFF ",
		"rgb(0,0,0)", "hsl(0,0%,0%)", "url(http://x/y)", "var(--color-text)", "expression(alert(1))", "#FFFFFF;",
		"#FFFFFF;}body{display:none}", "#FFF}</style><script>alert(1)</script>", "#000000 !important", "transparent", "currentcolor", "#FFFFFF\n",
	}
	for _, v := range bad {
		for name, c := range map[string]Config{
			"accent":           {Accent: v},
			"light.background": {Light: Palette{Background: v}},
			"light.text_muted": {Light: Palette{TextMuted: v}},
			"dark.border":      {Dark: Palette{Border: v}},
			"dark.surface_alt": {Dark: Palette{SurfaceAlt: v}},
		} {
			if th, err := Compile(c); err == nil {
				t.Errorf("%s=%q must be rejected, got CSS:\n%s", name, v, th.CSS)
			}
		}
	}
	for _, m := range []string{"Auto", "night", "system", "dark;", "light "} {
		if _, err := Compile(Config{Default: m}); err == nil {
			t.Errorf("default %q must be rejected", m)
		}
	}
}

func TestErrorsNameTheKeyAndDoNotEchoUnboundedInput(t *testing.T) {
	_, err := Compile(Config{Dark: Palette{Surface: "url(" + strings.Repeat("x", 500) + ")"}})
	if err == nil || !strings.Contains(err.Error(), "theme.dark.surface") || len(err.Error()) > 300 {
		t.Fatalf("unhelpful or unbounded error: %v", err)
	}
}

func TestGeneratedCSSContainsOnlyValidatedTokens(t *testing.T) {
	// Even with the most hostile accepted values the output is a fixed grammar.
	th := mustCompile(t, Config{Accent: "#ABCDEF", Light: Palette{Text: "#000000"}, Dark: Palette{Text: "#ffffff"}})
	line := regexp.MustCompile(`^(?:  )*(?:\/\* Generated at startup[^*]*\*\/|(?:@media \(prefers-color-scheme:dark\)|:root(?::not\(\[data-theme="light"\]\)|\[data-theme="dark"\])?)\{|\}|color-scheme:(?:light|dark);|--[a-z-]+:#[0-9A-F]{6};)$`)
	for _, l := range strings.Split(strings.TrimSuffix(string(th.CSS), "\n"), "\n") {
		if !line.MatchString(l) {
			t.Fatalf("unexpected CSS line %q", l)
		}
	}
}

func TestOutputIsDeterministicAndETagTracksContent(t *testing.T) {
	cfg := Config{Accent: "#0D9488", Dark: Palette{Background: "#000000"}}
	a, b := mustCompile(t, cfg), mustCompile(t, cfg)
	if !bytes.Equal(a.CSS, b.CSS) || a.ETag != b.ETag {
		t.Fatal("compilation must be deterministic")
	}
	if !regexp.MustCompile(`^"[0-9a-f]{16}"$`).MatchString(a.ETag) {
		t.Fatalf("ETag %s is not a strong validator", a.ETag)
	}
	if c := mustCompile(t, Config{Accent: "#0D9489"}); c.ETag == a.ETag {
		t.Fatal("a configuration change must change the ETag")
	}
	if c := mustCompile(t, Config{Accent: "#0d9488", Dark: Palette{Background: "#000000"}}); c.ETag != a.ETag {
		t.Fatal("case differences must normalize to the same stylesheet")
	}
}

// The derived tokens meet their contrast goals for the built-in palettes and
// for a range of operator accents, on both palettes.
func TestDerivedTokensMeetContrastGoals(t *testing.T) {
	accents := []string{"#2563EB", "#0D9488", "#DC2626", "#7C3AED", "#F59E0B", "#FFFF00", "#808080", "#000000", "#FFFFFF", "#10B981"}
	for _, a := range accents {
		th := mustCompile(t, Config{Accent: a})
		css := string(th.CSS)
		for _, sel := range []string{":root", `:root[data-theme="dark"]`} {
			b := block(t, css, sel)
			surfaces := []rgb{token(t, b, "--color-background"), token(t, b, "--color-surface"), token(t, b, "--color-surface-alt"), token(t, b, "--color-surface-hover")}
			link, focus := token(t, b, "--color-accent-text"), token(t, b, "--color-focus")
			for i, s := range append(surfaces, token(t, b, "--color-accent-subtle")) {
				if c := contrast(link, s); c < 4.5 {
					t.Errorf("accent %s %s: link text on bg#%d is %.2f:1", a, sel, i, c)
				}
			}
			if focus != link {
				t.Errorf("focus indicator must equal the readable accent")
			}
			acc, fg, hover := token(t, b, "--color-accent"), token(t, b, "--color-accent-foreground"), token(t, b, "--color-accent-hover")
			if contrast(fg, acc) < 4.5 || contrast(fg, hover) < 4.5 {
				t.Errorf("accent %s %s: button text %s on %s/%s is below 4.5:1", a, sel, fg, acc, hover)
			}
			for _, s := range surfaces[:3] {
				if c := contrast(token(t, b, "--color-border-control"), s); c < 3 {
					t.Errorf("accent %s %s: control border is %.2f:1", a, sel, c)
				}
			}
		}
	}
}

func TestStatusTokensAreReadableAndDistinctFromAccent(t *testing.T) {
	th := mustCompile(t, Config{})
	css := string(th.CSS)
	for _, sel := range []string{":root", `:root[data-theme="dark"]`} {
		b := block(t, css, sel)
		seen := map[rgb]string{token(t, b, "--color-accent"): "accent", token(t, b, "--color-accent-text"): "accent-text"}
		for _, s := range []string{"good", "bad", "warn"} {
			bg, fg := token(t, b, "--status-"+s+"-bg"), token(t, b, "--status-"+s+"-fg")
			if c := contrast(fg, bg); c < 4.5 {
				t.Errorf("%s %s pill is %.2f:1", sel, s, c)
			}
			for _, surf := range []string{"--color-background", "--color-surface", "--color-surface-alt", "--color-surface-hover"} {
				if c := contrast(fg, token(t, b, surf)); c < 4.5 {
					t.Errorf("%s %s text on %s is %.2f:1", sel, s, surf, c)
				}
			}
			if other, dup := seen[fg]; dup {
				t.Errorf("status %s collapses into %s", s, other)
			}
			seen[fg] = s
		}
	}
}

func TestStatusColorsDoNotFollowTheAccent(t *testing.T) {
	a, b := mustCompile(t, Config{}), mustCompile(t, Config{Accent: "#DC2626"})
	for _, s := range []string{"good", "bad", "warn"} {
		x := token(t, block(t, string(a.CSS), ":root"), "--status-"+s+"-fg")
		y := token(t, block(t, string(b.CSS), ":root"), "--status-"+s+"-fg")
		if x != y {
			t.Errorf("status %s changed with the accent", s)
		}
	}
}

func TestPoorContrastWarnsWithoutReplacingColors(t *testing.T) {
	th := mustCompile(t, Config{Light: Palette{TextMuted: "#B0B0B0"}, Accent: "#FFFF00"})
	joined := strings.Join(th.Warnings, "\n")
	for _, want := range []string{"theme.light: text_muted (#B0B0B0) on surface", "below the 4.50:1 target", "theme.light: accent (#FFFF00) on surface"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(string(th.CSS), "--color-text-muted:#B0B0B0;") || !strings.Contains(string(th.CSS), "--color-accent:#FFFF00;") {
		t.Fatal("operator colors must be used as configured")
	}
	again := mustCompile(t, Config{Light: Palette{TextMuted: "#B0B0B0"}, Accent: "#FFFF00"})
	if strings.Join(again.Warnings, "\n") != joined {
		t.Fatal("warnings must be deterministic")
	}
}

func TestUnreadableTextIsAnError(t *testing.T) {
	for name, c := range map[string]Config{
		"text equals surface":  {Light: Palette{Text: "#FFFFFF"}},
		"dark text on dark":    {Dark: Palette{Text: "#222222"}},
		"light text near bg":   {Light: Palette{Text: "#F0F0F0", Background: "#FFFFFF"}},
		"mid grey on mid grey": {Light: Palette{Text: "#999999", Surface: "#AAAAAA", Background: "#AAAAAA"}},
	} {
		_, err := Compile(c)
		if err == nil || !strings.Contains(err.Error(), "contrast") {
			t.Errorf("%s: want contrast error, got %v", name, err)
		}
	}
}

func TestColorMath(t *testing.T) {
	if c := contrast(black, white); c < 20.99 || c > 21.01 {
		t.Fatalf("black/white contrast %v", c)
	}
	if got := mix(rgb{0, 0, 0}, rgb{255, 255, 255}, 50); got != (rgb{128, 128, 128}) {
		t.Fatalf("mix %v", got)
	}
	if c, ok := parseHex("#0a1B2c"); !ok || c.String() != "#0A1B2C" {
		t.Fatalf("normalization: %v %v", c, ok)
	}
	if ratio(4.499) != "4.49:1" {
		t.Fatalf("ratio must round down, got %s", ratio(4.499))
	}
}
