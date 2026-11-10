package theme

import (
	"fmt"
	"math"
)

// rgb is an sRGB color. All derivation is done with integer mixing on this
// type so that the generated stylesheet is byte-for-byte deterministic.
type rgb [3]uint8

// parseHex accepts only the strict form #RRGGBB (either hex case). Shorthand,
// alpha channels, color names and every CSS function or expression are
// rejected, so nothing but six hex digits can ever reach the stylesheet.
func parseHex(s string) (rgb, bool) {
	if len(s) != 7 || s[0] != '#' {
		return rgb{}, false
	}
	var c rgb
	for i := 0; i < 3; i++ {
		hi, ok1 := hexVal(s[1+2*i])
		lo, ok2 := hexVal(s[2+2*i])
		if !ok1 || !ok2 {
			return rgb{}, false
		}
		c[i] = hi<<4 | lo
	}
	return c, true
}

func hexVal(b byte) (uint8, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

// String is the canonical form: upper-case #RRGGBB.
func (c rgb) String() string { return fmt.Sprintf("#%02X%02X%02X", c[0], c[1], c[2]) }

var (
	white = rgb{255, 255, 255}
	black = rgb{0, 0, 0}
)

// mix returns a blended toward b by pct percent (0..100), rounding half up.
func mix(a, b rgb, pct int) rgb {
	var out rgb
	for i := range out {
		out[i] = uint8((int(a[i])*(100-pct) + int(b[i])*pct + 50) / 100)
	}
	return out
}

func linear(v uint8) float64 {
	c := float64(v) / 255
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// luminance is the WCAG 2.x relative luminance. Each product is converted
// explicitly so that no platform fuses it into a multiply-add, which keeps the
// derivation identical on every architecture.
func luminance(c rgb) float64 {
	return float64(0.2126*linear(c[0])) + float64(0.7152*linear(c[1])) + float64(0.0722*linear(c[2]))
}

// contrast is the WCAG contrast ratio between two colors (1..21).
func contrast(a, b rgb) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// minContrast is the lowest contrast of c against any of the backgrounds.
func minContrast(c rgb, backgrounds []rgb) float64 {
	lowest := math.Inf(1)
	for _, b := range backgrounds {
		lowest = min(lowest, contrast(c, b))
	}
	return lowest
}

// ratio formats a contrast ratio rounded down to two decimals, so a value
// below its target never prints as if it met it.
func ratio(r float64) string { return fmt.Sprintf("%.2f:1", math.Floor(r*100)/100) }

// pole is the extreme that text on the given surfaces is derived toward: black
// for light surfaces, white for dark ones (0.179 is where both contrast
// equally with mid grey).
func pole(surface rgb) rgb {
	if luminance(surface) > 0.179 {
		return black
	}
	return white
}

// shift moves base toward target in 2% steps and returns the first color that
// reaches the contrast goal against every background. If none does it returns
// the pole itself, the best contrast attainable in that direction.
func shift(base, target rgb, goal float64, backgrounds []rgb) rgb {
	for pct := 0; pct <= 100; pct += 2 {
		if c := mix(base, target, pct); minContrast(c, backgrounds) >= goal {
			return c
		}
	}
	return target
}
