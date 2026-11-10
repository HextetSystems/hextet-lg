package communities

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds on what one definition may ask the matcher to do. Together with the
// load limits they keep lookups cheap whatever the files contain.
const (
	maxComponentChars = 16 // characters in one component pattern
	maxNNN            = 3  // 'nnn' wildcards in one definition
)

type tokKind uint8

const (
	tokLit   tokKind = iota // literal decimal digits
	tokDigit                // 'x': exactly one decimal digit
	tokNum                  // 'nnn': one or more decimal digits
)

type tok struct {
	kind tokKind
	lit  string
}

type compKind uint8

const (
	compLit compKind = iota
	compRange
	compPat
)

// comp matches one numeric component of a community.
type comp struct {
	kind   compKind
	lit    string // canonical decimal text (compLit)
	lo, hi uint64 // inclusive bounds (compRange)
	toks   []tok  // compPat
}

// part is a piece of a description: literal text, or a reference to a capture.
type part struct {
	text string
	ref  int // -1: literal text
}

// entry is one compiled definition.
type entry struct {
	kind  Kind
	label string
	comps []comp
	ncap  int
	space float64
	order int
	desc  string
	parts []part // nil: the description has no substitutions
}

// parsedLine is the result of parsing one "community,description" line.
type parsedLine struct {
	e     *entry
	key   string // canonical definition text
	exact bool
}

var errNoComma = errors.New("missing ',' between community and description")

// parseLine parses one trimmed, non-comment line. order numbers the definition
// in load order for the final precedence tie-break.
func parseLine(line string, order int) (parsedLine, error) {
	i := strings.IndexByte(line, ',')
	if i < 0 {
		return parsedLine{}, errNoComma
	}
	key, desc := strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
	if key == "" {
		return parsedLine{}, errors.New("empty community")
	}
	if desc == "" {
		return parsedLine{}, errors.New("empty description")
	}
	if len(desc) > MaxDescriptionBytes {
		return parsedLine{}, fmt.Errorf("description is %d bytes, the limit is %d", len(desc), MaxDescriptionBytes)
	}
	if err := checkText(desc); err != nil {
		return parsedLine{}, fmt.Errorf("description: %w", err)
	}
	e := &entry{order: order, desc: desc}
	var body string
	if strings.IndexByte(key, ' ') >= 0 || strings.IndexByte(key, '\t') >= 0 {
		f := strings.Fields(key)
		if len(f) != 2 {
			return parsedLine{}, fmt.Errorf("unsupported community %q: expected \"label value\" for an extended community", key)
		}
		e.kind, e.label, body = Extended, strings.ToLower(f[0]), f[1]
		if len(e.label) > maxLabel || !validLabel(e.label) {
			return parsedLine{}, fmt.Errorf("unsupported extended community label %q", f[0])
		}
	} else {
		e.kind, body = Standard, key
	}
	parts := strings.Split(body, ":")
	switch {
	case e.kind == Extended && (len(parts) == 1 || len(parts) == 2):
	case e.kind == Standard && len(parts) == 2:
	case e.kind == Standard && len(parts) == 3:
		e.kind = Large
	default:
		return parsedLine{}, fmt.Errorf("unsupported community %q: expected A:B (standard), A:B:C (large) or \"label A[:B]\" (extended)", key)
	}
	maxN := uint64(maxU32)
	if e.kind == Standard {
		maxN = maxStd
	}
	var canon []string
	nnn := 0
	e.space = 1
	exact := true
	for _, p := range parts {
		c, space, caps, nums, err := parseComp(p, maxN)
		if err != nil {
			return parsedLine{}, fmt.Errorf("community %q: %w", key, err)
		}
		e.comps = append(e.comps, c)
		e.space *= space
		e.ncap += caps
		nnn += nums
		exact = exact && c.kind == compLit
		canon = append(canon, c.text())
	}
	if nnn > maxNNN {
		return parsedLine{}, fmt.Errorf("community %q has %d 'nnn' wildcards, the limit is %d", key, nnn, maxNNN)
	}
	if e.ncap > maxCaptures {
		return parsedLine{}, fmt.Errorf("community %q has %d wildcards, the limit is %d", key, e.ncap, maxCaptures)
	}
	canonical := strings.Join(canon, ":")
	if e.kind == Extended {
		canonical = e.label + " " + canonical
	}
	if !exact {
		var err error
		if e.parts, err = parseDescription(desc, e.ncap); err != nil {
			return parsedLine{}, err
		}
	}
	return parsedLine{e: e, key: canonical, exact: exact}, nil
}

// text is the canonical spelling of a component.
func (c comp) text() string {
	switch c.kind {
	case compLit:
		return c.lit
	case compRange:
		return strconv.FormatUint(c.lo, 10) + "-" + strconv.FormatUint(c.hi, 10)
	}
	var b strings.Builder
	for _, t := range c.toks {
		switch t.kind {
		case tokLit:
			b.WriteString(t.lit)
		case tokDigit:
			b.WriteByte('x')
		case tokNum:
			b.WriteString("nnn")
		}
	}
	return b.String()
}

// parseComp parses one component: a canonical literal, an inclusive range, or
// a pattern of digits, 'x' and 'nnn'. It returns the component, the estimated
// number of values it can match, and its number of captures and 'nnn' tokens.
func parseComp(s string, maxN uint64) (c comp, space float64, caps, nums int, err error) {
	if s == "" {
		return c, 0, 0, 0, errors.New("empty component")
	}
	if len(s) > maxComponentChars {
		return c, 0, 0, 0, fmt.Errorf("component %q is longer than %d characters", s, maxComponentChars)
	}
	if lo, hi, found := strings.Cut(s, "-"); found {
		a, aCanon, aOK := parseDecimal(lo, maxN)
		b, bCanon, bOK := parseDecimal(hi, maxN)
		switch {
		case !aOK || !bOK:
			return c, 0, 0, 0, fmt.Errorf("unsupported range %q: expected lo-hi with decimal numbers up to %d", s, maxN)
		case !aCanon || !bCanon:
			return c, 0, 0, 0, fmt.Errorf("range %q: leading zeros are not allowed", s)
		case a > b:
			return c, 0, 0, 0, fmt.Errorf("bad range %q: start is greater than end", s)
		}
		return comp{kind: compRange, lo: a, hi: b}, float64(b-a) + 1, 0, 0, nil
	}
	var toks []tok
	fixed := 0
	for i := 0; i < len(s); {
		switch ch := s[i]; {
		case ch >= '0' && ch <= '9':
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			toks = append(toks, tok{kind: tokLit, lit: s[i:j]})
			fixed += j - i
			i = j
		case ch == 'x' || ch == 'X':
			toks = append(toks, tok{kind: tokDigit})
			fixed++
			caps++
			i++
		case ch == 'n' || ch == 'N':
			if i+3 > len(s) || !strings.EqualFold(s[i:i+3], "nnn") {
				return c, 0, 0, 0, fmt.Errorf("unsupported syntax %q in component %q (only digits, x, nnn and lo-hi ranges are supported)", s[i:], s)
			}
			if len(toks) > 0 && toks[len(toks)-1].kind == tokNum {
				return c, 0, 0, 0, fmt.Errorf("component %q: adjacent 'nnn' wildcards are ambiguous", s)
			}
			toks = append(toks, tok{kind: tokNum})
			caps++
			nums++
			i += 3
		default:
			return c, 0, 0, 0, fmt.Errorf("unsupported character %q in component %q (only digits, x, nnn and lo-hi ranges are supported)", string(ch), s)
		}
	}
	if len(toks) == 1 && toks[0].kind == tokLit {
		n, canonical, ok := parseDecimal(s, maxN)
		switch {
		case !ok:
			return c, 0, 0, 0, fmt.Errorf("component %q is not a number up to %d", s, maxN)
		case !canonical:
			return c, 0, 0, 0, fmt.Errorf("component %q: leading zeros are not allowed", s)
		}
		return comp{kind: compLit, lit: strconv.FormatUint(n, 10)}, 1, 0, 0, nil
	}
	// A pattern must be able to match at least one in-range decimal value.
	if fixed+nums > 10 {
		return c, 0, 0, 0, fmt.Errorf("component %q can never match: it needs more than 10 digits", s)
	}
	var low strings.Builder
	for _, t := range toks {
		if t.kind == tokLit {
			low.WriteString(t.lit)
		} else {
			low.WriteByte('0')
		}
	}
	if n, _ := strconv.ParseUint(low.String(), 10, 64); n > maxN {
		return c, 0, 0, 0, fmt.Errorf("component %q can never match: its smallest value exceeds %d", s, maxN)
	}
	space = 1
	switch {
	case nums > 0:
		space = float64(maxN+1) / pow10(fixed)
		if space < 1 {
			space = 1
		}
	default:
		space = pow10(caps)
	}
	return comp{kind: compPat, toks: toks}, space, caps, nums, nil
}

func pow10(n int) float64 {
	v := 1.0
	for ; n > 0; n-- {
		v *= 10
	}
	return v
}

// parseDescription splits a description into literal text and $N references to
// the digits captured by 'x' and 'nnn' wildcards. A reference to a capture the
// definition does not have is an error; nothing else in a description is
// interpreted.
func parseDescription(desc string, ncap int) ([]part, error) {
	var parts []part
	var text strings.Builder
	refs := 0
	for i := 0; i < len(desc); {
		if desc[i] != '$' || i+1 >= len(desc) || desc[i+1] < '0' || desc[i+1] > '9' {
			text.WriteByte(desc[i])
			i++
			continue
		}
		j := i + 1
		for j < len(desc) && desc[j] >= '0' && desc[j] <= '9' && j-i <= 3 {
			j++
		}
		n, _ := strconv.Atoi(desc[i+1 : j])
		if n >= ncap {
			if ncap == 0 {
				return nil, fmt.Errorf("description refers to $%d but the definition has no wildcard captures (only x and nnn capture)", n)
			}
			return nil, fmt.Errorf("description refers to $%d but the definition has only %d capture(s), numbered $0 to $%d", n, ncap, ncap-1)
		}
		if text.Len() > 0 {
			parts = append(parts, part{text: text.String(), ref: -1})
			text.Reset()
		}
		parts = append(parts, part{ref: n})
		refs++
		i = j
	}
	if refs == 0 {
		return nil, nil
	}
	if text.Len() > 0 {
		parts = append(parts, part{text: text.String(), ref: -1})
	}
	return parts, nil
}

// checkText rejects invalid UTF-8 and control characters other than tab.
func checkText(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("invalid UTF-8")
	}
	for _, r := range s {
		if r != '\t' && (unicode.IsControl(r) || r == '�') {
			return fmt.Errorf("control or invalid character U+%04X", r)
		}
	}
	return nil
}

// render expands $N references with the matched digits.
func (e *entry) render(caps *[maxCaptures]string) string {
	if e.parts == nil {
		return e.desc
	}
	var b strings.Builder
	b.Grow(len(e.desc) + 8)
	for _, p := range e.parts {
		if p.ref < 0 {
			b.WriteString(p.text)
		} else {
			b.WriteString(caps[p.ref])
		}
	}
	return b.String()
}

// match reports whether the definition matches the community, filling caps
// with the digits matched by each wildcard in left-to-right order.
func (e *entry) match(p *pv, caps *[maxCaptures]string) bool {
	if e.kind == Extended && p.label != e.label || len(e.comps) != p.n {
		return false
	}
	n := 0
	for i := range e.comps {
		c := &e.comps[i]
		switch c.kind {
		case compLit:
			if c.lit != p.s[i] {
				return false
			}
		case compRange:
			if p.num[i] < c.lo || p.num[i] > c.hi {
				return false
			}
		case compPat:
			if !matchToks(c.toks, p.s[i], caps, &n) {
				return false
			}
		}
	}
	return true
}

// matchToks matches the whole of s. 'nnn' is greedy with backtracking, so the
// captures equal those of the equivalent anchored regular expression
// (\d)/(\d+), without compiling one. s has at most 10 digits and a definition
// at most maxNNN 'nnn' tokens, so the work is bounded.
func matchToks(toks []tok, s string, caps *[maxCaptures]string, n *int) bool {
	if len(toks) == 0 {
		return s == ""
	}
	switch t := toks[0]; t.kind {
	case tokLit:
		return strings.HasPrefix(s, t.lit) && matchToks(toks[1:], s[len(t.lit):], caps, n)
	case tokDigit:
		if s == "" {
			return false
		}
		caps[*n] = s[:1]
		*n++
		if matchToks(toks[1:], s[1:], caps, n) {
			return true
		}
		*n--
		return false
	default:
		for l := len(s); l >= 1; l-- {
			caps[*n] = s[:l]
			*n++
			if matchToks(toks[1:], s[l:], caps, n) {
				return true
			}
			*n--
		}
		return false
	}
}
