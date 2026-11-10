// Package theme turns the optional YAML "theme" section into a validated,
// immutable stylesheet of CSS custom properties. Operators supply only
// #RRGGBB colors; every other value is derived deterministically, and the CSS
// is assembled from those validated values alone, so configuration can never
// inject CSS. It has no web or BGP dependencies.
package theme

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Default selection modes.
const (
	ModeAuto  = "auto"
	ModeLight = "light"
	ModeDark  = "dark"
)

// Palette holds the independently overridable colors of one color scheme.
// Every field is optional; an empty value inherits the built-in default.
type Palette struct {
	Background   string `yaml:"background"`
	Surface      string `yaml:"surface"`
	SurfaceAlt   string `yaml:"surface_alt"`
	SurfaceHover string `yaml:"surface_hover"`
	Border       string `yaml:"border"`
	Text         string `yaml:"text"`
	TextMuted    string `yaml:"text_muted"`
}

// Config is the YAML "theme" section. The zero value selects the built-in
// themes, so configurations written before it existed keep working.
type Config struct {
	// Default is the initial scheme: auto (follow the browser, the default),
	// light or dark. A visitor's own choice overrides it in that browser.
	Default string `yaml:"default"`
	// Accent is the single accent color shared by both palettes.
	Accent string  `yaml:"accent"`
	Light  Palette `yaml:"light"`
	Dark   Palette `yaml:"dark"`
}

// Built-in defaults.
const DefaultAccent = "#2563EB"

var (
	defaultLight = Palette{Background: "#F7F8FA", Surface: "#FFFFFF", SurfaceAlt: "#F0F2F5", SurfaceHover: "#E8ECF1", Border: "#D8DDE3", Text: "#252B32", TextMuted: "#626A76"}
	defaultDark  = Palette{Background: "#111316", Surface: "#1B1E23", SurfaceAlt: "#23272D", SurfaceHover: "#292D34", Border: "#30353C", Text: "#E8EBEF", TextMuted: "#A0A7B2"}
)

// Semantic status hues. They convey operational meaning (valid, invalid,
// warning) and are deliberately not the accent and not configurable; the
// tokens derived from them are adjusted per palette to stay readable.
var (
	statusLight = map[string]rgb{"good": {0x1A, 0x7F, 0x4B}, "bad": {0xC6, 0x28, 0x28}, "warn": {0x9A, 0x67, 0x00}}
	statusDark  = map[string]rgb{"good": {0x3F, 0xB9, 0x7A}, "bad": {0xF2, 0x70, 0x6B}, "warn": {0xE3, 0xB3, 0x41}}
	statusOrder = []string{"good", "bad", "warn"}
)

// Contrast targets (WCAG 2.x AA). Below hardMin a text/background pair is
// treated as unreadable and rejected at startup; between hardMin and the
// target it only produces a warning.
const (
	textTarget = 4.5
	uiTarget   = 3.0
	hardMin    = 3.0
)

// Theme is the compiled, immutable result.
type Theme struct {
	// Default is auto, light or dark.
	Default string
	// CSS is the complete stylesheet, generated once.
	CSS []byte
	// ETag is a strong validator derived from the CSS bytes.
	ETag string
	// Warnings are contrast shortfalls in operator colors, in a fixed order.
	Warnings []string
}

// Compile validates the configuration and generates the stylesheet.
func Compile(c Config) (*Theme, error) {
	mode := c.Default
	switch mode {
	case "":
		mode = ModeAuto
	case ModeAuto, ModeLight, ModeDark:
	default:
		return nil, fmt.Errorf("theme.default %s must be auto, light or dark", quote(mode))
	}
	accent, err := color("theme.accent", c.Accent, DefaultAccent)
	if err != nil {
		return nil, err
	}
	light, err := resolve("theme.light", c.Light, defaultLight, accent, statusLight)
	if err != nil {
		return nil, err
	}
	dark, err := resolve("theme.dark", c.Dark, defaultDark, accent, statusDark)
	if err != nil {
		return nil, err
	}
	var warnings []string
	for _, p := range []*resolved{light, dark} {
		w, err := p.audit()
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, w...)
	}
	css := render(mode, light, dark)
	sum := sha256.Sum256(css)
	return &Theme{Default: mode, CSS: css, ETag: `"` + hex.EncodeToString(sum[:8]) + `"`, Warnings: warnings}, nil
}

// quote shows an operator value in an error without letting a very long value
// flood the log.
func quote(s string) string {
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return fmt.Sprintf("%q", s)
}

func color(key, value, def string) (rgb, error) {
	if value == "" {
		value = def
	}
	c, ok := parseHex(value)
	if !ok {
		return rgb{}, fmt.Errorf("%s %s is not a color: use the strict form #RRGGBB (for example %s)", key, quote(value), def)
	}
	return c, nil
}

// resolved is one palette with every derived value.
type resolved struct {
	name                                                    string // theme.light or theme.dark
	background, surface, surfaceAlt, surfaceHover, border   rgb
	text, textMuted                                         rgb
	accent, accentHover, accentSubtle, accentText, accentFg rgb
	controlBorder                                           rgb
	status                                                  map[string][2]rgb // name -> {bg, fg}
	dark                                                    bool
}

func resolve(name string, p, def Palette, accent rgb, hues map[string]rgb) (*resolved, error) {
	r := &resolved{name: name, accent: accent, status: map[string][2]rgb{}}
	fields := []struct {
		key  string
		in   string
		def  string
		into *rgb
	}{
		{"background", p.Background, def.Background, &r.background},
		{"surface", p.Surface, def.Surface, &r.surface},
		{"surface_alt", p.SurfaceAlt, def.SurfaceAlt, &r.surfaceAlt},
		{"surface_hover", p.SurfaceHover, def.SurfaceHover, &r.surfaceHover},
		{"border", p.Border, def.Border, &r.border},
		{"text", p.Text, def.Text, &r.text},
		{"text_muted", p.TextMuted, def.TextMuted, &r.textMuted},
	}
	for _, f := range fields {
		c, err := color(name+"."+f.key, f.in, f.def)
		if err != nil {
			return nil, err
		}
		*f.into = c
	}
	r.derive(hues)
	return r, nil
}

func (r *resolved) surfaces() []rgb {
	return []rgb{r.background, r.surface, r.surfaceAlt, r.surfaceHover}
}

// derive computes every value the operator did not have to configure:
//
//   - accent-subtle: the accent blended 12% into the surface.
//   - accent-text: the accent moved toward the palette's pole (black on light
//     palettes, white on dark ones) in 2% steps until it reaches 4.5:1 on every
//     surface and on accent-subtle. Used for links, active navigation and the
//     focus ring. A sufficiently contrasting accent is used unchanged.
//   - accent-foreground: white or black, whichever is more readable on the
//     accent (white on a tie). One of them always reaches 4.5:1.
//   - accent-hover: the accent moved 14% away from the foreground color (darker
//     under white text, lighter under black text), so hover never lowers the
//     text contrast.
//   - border-control: the border moved toward the text color in 5% steps until
//     it reaches 3:1 on the background, surface and surface-alt, so form
//     controls keep a visible boundary while decorative borders stay light.
//   - status tokens: for good/bad/warn, bg is the status hue blended 16% into
//     the surface and fg is the hue moved toward the pole until it reaches
//     4.5:1 on that bg and on every surface.
func (r *resolved) derive(hues map[string]rgb) {
	p := pole(r.surface)
	r.dark = p == white
	r.accentSubtle = mix(r.surface, r.accent, 12)
	r.accentText = shift(r.accent, p, textTarget, append(r.surfaces(), r.accentSubtle))
	r.accentFg, r.accentHover = white, mix(r.accent, black, 14)
	if contrast(black, r.accent) > contrast(white, r.accent) {
		r.accentFg, r.accentHover = black, mix(r.accent, white, 14)
	}
	r.controlBorder = r.border
	for pct := 0; pct <= 100; pct += 5 {
		r.controlBorder = mix(r.border, r.text, pct)
		if minContrast(r.controlBorder, []rgb{r.background, r.surface, r.surfaceAlt}) >= uiTarget {
			break
		}
	}
	for _, s := range statusOrder {
		bg := mix(r.surface, hues[s], 16)
		fg := shift(hues[s], p, textTarget, append(r.surfaces(), bg))
		r.status[s] = [2]rgb{bg, fg}
	}
}

type pair struct {
	fgName, bgName string
	fg, bg         rgb
	goal           float64
	hard           bool
}

// audit checks the readability of the final colors. A pair below the target
// is a warning; primary text below 3:1 on the page or panel background is an
// error because the page would be unreadable. Operator colors are never
// silently replaced.
func (r *resolved) audit() ([]string, error) {
	named := []struct {
		n string
		c rgb
		h bool // hard-checked background
	}{{"background", r.background, true}, {"surface", r.surface, true}, {"surface_alt", r.surfaceAlt, false}, {"surface_hover", r.surfaceHover, false}}
	var checks []pair
	for _, fg := range []struct {
		n string
		c rgb
	}{{"text", r.text}, {"text_muted", r.textMuted}} {
		for _, bg := range named {
			checks = append(checks, pair{fg.n, bg.n, fg.c, bg.c, textTarget, fg.n == "text" && bg.h})
		}
	}
	checks = append(checks,
		pair{"accent", "background", r.accent, r.background, uiTarget, false},
		pair{"accent", "surface", r.accent, r.surface, uiTarget, false},
		pair{"accent link text (derived)", "surface_hover", r.accentText, r.surfaceHover, textTarget, false},
		pair{"accent foreground (derived)", "accent", r.accentFg, r.accent, textTarget, false},
		pair{"accent foreground (derived)", "accent hover (derived)", r.accentFg, r.accentHover, textTarget, false},
	)
	var warnings []string
	for _, c := range checks {
		got := contrast(c.fg, c.bg)
		if c.hard && got < hardMin {
			return nil, fmt.Errorf("%s: %s (%s) on %s (%s) has contrast %s; at least %s is required for readable text", r.name, c.fgName, c.fg, c.bgName, c.bg, ratio(got), ratio(hardMin))
		}
		if got < c.goal {
			warnings = append(warnings, fmt.Sprintf("%s: %s (%s) on %s (%s) has contrast %s, below the %s target", r.name, c.fgName, c.fg, c.bgName, c.bg, ratio(got), ratio(c.goal)))
		}
	}
	return warnings, nil
}

func (r *resolved) tokens() [][2]string {
	t := [][2]string{
		{"--color-background", r.background.String()},
		{"--color-surface", r.surface.String()},
		{"--color-surface-alt", r.surfaceAlt.String()},
		{"--color-surface-hover", r.surfaceHover.String()},
		{"--color-border", r.border.String()},
		{"--color-border-control", r.controlBorder.String()},
		{"--color-text", r.text.String()},
		{"--color-text-muted", r.textMuted.String()},
		{"--color-accent", r.accent.String()},
		{"--color-accent-hover", r.accentHover.String()},
		{"--color-accent-subtle", r.accentSubtle.String()},
		{"--color-accent-text", r.accentText.String()},
		{"--color-accent-foreground", r.accentFg.String()},
		{"--color-focus", r.accentText.String()},
	}
	for _, s := range statusOrder {
		t = append(t, [2]string{"--status-" + s + "-bg", r.status[s][0].String()}, [2]string{"--status-" + s + "-fg", r.status[s][1].String()})
	}
	return t
}

func (r *resolved) block(selector string) string {
	scheme := "light"
	if r.dark {
		scheme = "dark"
	}
	var b strings.Builder
	b.WriteString(selector + "{\n  color-scheme:" + scheme + ";\n")
	for _, kv := range r.tokens() {
		b.WriteString("  " + kv[0] + ":" + kv[1] + ";\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// render lays out the cascade. The palette matching the configured default is
// the plain :root rule, so the page is themed without JavaScript. A visitor's
// stored choice arrives as data-theme="light|dark" on <html> and overrides
// it; in auto mode the dark palette also follows prefers-color-scheme unless
// light was chosen explicitly.
func render(mode string, light, dark *resolved) []byte {
	var b strings.Builder
	b.WriteString("/* Generated at startup by hextet-lg from the validated \"theme\" configuration. */\n")
	switch mode {
	case ModeLight:
		b.WriteString(light.block(":root"))
		b.WriteString(dark.block(`:root[data-theme="dark"]`))
	case ModeDark:
		b.WriteString(dark.block(":root"))
		b.WriteString(light.block(`:root[data-theme="light"]`))
	default:
		b.WriteString(light.block(":root"))
		b.WriteString("@media (prefers-color-scheme:dark){\n" + indent(dark.block(`:root:not([data-theme="light"])`)) + "}\n")
		b.WriteString(dark.block(`:root[data-theme="dark"]`))
	}
	return []byte(b.String())
}

func indent(s string) string {
	lines := strings.SplitAfter(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "") + "\n"
}
