// Package communities loads operator-supplied BGP community definitions in the
// legacy NLNOG looking glass text format and answers description lookups.
//
// The format is the one described by the NLNOG looking glass README
// (https://github.com/NLNOG/lg.ring.nlnog.net) and implemented by its
// read_communities and get_community_descr_from_list functions: one definition
// per line, "community,description", split at the first comma, '#' comments.
// A community is standard (A:B), large (A:B:C) or extended (label A[:B]); each
// numeric component may be a literal, an inclusive range (lo-hi) or a pattern
// of literal digits, 'x' (exactly one decimal digit) and 'nnn' (one or more
// decimal digits). Descriptions may refer to the digits matched by 'x' and
// 'nnn' as $0, $1, ... in left-to-right order.
//
// Deviations from the upstream implementation, all deliberate:
//
//   - Patterns match the whole decimal component. Upstream anchors only the
//     start, so 65535:x0 would also match 65535:100.
//   - Ranges (65535:0-100) work in any component. Upstream documents them but
//     its code cannot evaluate them.
//   - 'x' and 'nnn' may be mixed in one definition (65535:x:nnn, as the README
//     example shows); upstream silently ignores 'x' when 'nnn' is present.
//   - Definitions match by the actual community value, not by the ASN in the
//     file name, so 65535:nnn in as10965.txt works. Upstream looks a community
//     up only in the file named after its first component.
//   - Lines that cannot be understood are startup errors with file and line,
//     never silently ignored.
//   - Overlapping definitions are ordered by specificity (see Set.Lookup)
//     instead of file order.
//   - "<ASN>" is expanded to the file's ASN in every asN.txt file, not only in
//     symbolic links.
//
// Nothing here compiles regular expressions: the supported syntax is parsed
// into a small matcher with bounded work per lookup. A Set is immutable after
// loading and safe for concurrent use.
package communities

import (
	"strconv"
	"strings"
)

// Kind distinguishes the three community families; they are never conflated.
type Kind uint8

// The community kinds.
const (
	Standard Kind = iota + 1
	Large
	Extended
)

func (k Kind) String() string {
	switch k {
	case Standard:
		return "standard"
	case Large:
		return "large"
	case Extended:
		return "extended"
	}
	return "unknown"
}

// ParseKind converts the String form back to a Kind.
func ParseKind(s string) (Kind, bool) {
	switch s {
	case "standard":
		return Standard, true
	case "large":
		return Large, true
	case "extended":
		return Extended, true
	}
	return 0, false
}

// Definition is one loaded definition, for listing in the Community Explorer.
type Definition struct {
	Kind        Kind
	Key         string // canonical definition text, e.g. "64500:400-499" or "soo 64500:x"
	Description string // as written in the file (substitution references unexpanded)
	File        string // base name of the defining file
	Source      string // "AS64500" or "Well-known": derived from the file name
	Line        int
	Exact       bool // no ranges or wildcards
}

// Stats describes a loaded Set for startup diagnostics.
type Stats struct {
	Files, Definitions, Exact, Patterns int
	Standard, Large, Extended           int
	// Ignored lists directory entries that were not loaded (other .txt files).
	Ignored []string
}

type exactKey struct {
	kind  Kind
	canon string
}

type bucketKey struct {
	kind  Kind
	label string
	first string // canonical literal first component, "" for non-literal ones
}

// Set is the immutable compiled definition set. A nil *Set is valid and empty.
type Set struct {
	exact    map[exactKey]*entry
	patterns map[bucketKey][]*entry
	defs     []Definition
	stats    Stats
}

// Definitions returns every definition in documented load order (well-known
// first, then ascending ASN, then file order). The slice is shared and must not
// be modified.
func (s *Set) Definitions() []Definition {
	if s == nil {
		return nil
	}
	return s.defs
}

// Stats returns the load statistics.
func (s *Set) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	return s.stats
}

// Len is the number of definitions.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.defs)
}

// Lookup returns the description of the best definition matching value, which
// must be a community of the given kind in OpenBGPD's output notation. The
// result is supplementary presentation data: an unknown or unparsable value
// simply yields ("", false).
//
// Precedence is deterministic and independent of map order:
//
//  1. an exact definition;
//  2. otherwise the pattern definition that can match the fewest values (a
//     range or digit wildcards before an unconstrained "nnn");
//  3. otherwise the one defined first (files in documented order, then line).
//
// The size of a definition's value space is estimated per component: a literal
// is 1, a range is hi-lo+1, each 'x' multiplies by 10 and a pattern with 'nnn'
// is the component maximum divided by 10 for each fixed digit.
func (s *Set) Lookup(kind Kind, value string) (string, bool) {
	if s == nil || len(s.defs) == 0 {
		return "", false
	}
	p, ok := parseValue(kind, value)
	if !ok {
		return "", false
	}
	if e, ok := s.exact[exactKey{kind, p.canon}]; ok {
		return e.desc, true
	}
	var caps [maxCaptures]string
	var best *entry
	var bestCaps [maxCaptures]string
	for _, first := range [2]string{p.s[0], ""} {
		for _, e := range s.patterns[bucketKey{kind, p.label, first}] {
			if best != nil && !e.before(best) {
				break // buckets are sorted: nothing later can win
			}
			if e.match(&p, &caps) {
				best, bestCaps = e, caps
				break
			}
		}
	}
	if best == nil {
		return "", false
	}
	return best.render(&bestCaps), true
}

// pv is a parsed route community value in canonical form.
type pv struct {
	canon string
	label string // extended only, lower case
	n     int
	s     [3]string // canonical decimal components
	num   [3]uint64
}

// wellKnown maps the names OpenBGPD prints for the registered well-known
// standard communities to their numeric form (RFC 1997, 3765, 7999, 8326).
var wellKnown = map[string]string{
	"GRACEFUL_SHUTDOWN":   "65535:0",
	"BLACKHOLE":           "65535:666",
	"NO_EXPORT":           "65535:65281",
	"NO_ADVERTISE":        "65535:65282",
	"NO_EXPORT_SUBCONFED": "65535:65283",
	"NO_PEER":             "65535:65284",
}

// parseValue parses and canonicalizes a route community: decimal components
// without leading zeros, lower-case extended label, one separating space.
// Values that merely use a different spelling of the same community (leading
// zeros, label case, extra spaces, well-known names) map to the same canon.
func parseValue(kind Kind, v string) (p pv, ok bool) {
	v = strings.TrimSpace(v)
	body, maxN, minParts, maxParts := v, uint64(maxU32), 3, 3
	clean := true
	switch kind {
	case Standard:
		if n, found := wellKnown[v]; found {
			v = n
		}
		body, maxN, minParts, maxParts = v, maxStd, 2, 2
	case Large:
	case Extended:
		i := strings.IndexByte(v, ' ')
		if i <= 0 || i > maxLabel {
			return p, false
		}
		p.label, body = v[:i], v[i+1:]
		if t := strings.TrimLeft(body, " "); t != body {
			clean, body = false, t
		}
		if lower := strings.ToLower(p.label); lower != p.label {
			clean, p.label = false, lower
		}
		if !validLabel(p.label) {
			return p, false
		}
		minParts, maxParts = 1, 2
	default:
		return p, false
	}
	for rest := body; ; {
		if p.n == maxParts {
			return p, false
		}
		part, tail, more := strings.Cut(rest, ":")
		n, canonical, valid := parseDecimal(part, maxN)
		if !valid {
			return p, false
		}
		p.s[p.n], p.num[p.n] = part, n
		if !canonical {
			clean = false
			p.s[p.n] = strconv.FormatUint(n, 10)
		}
		p.n++
		if !more {
			break
		}
		rest = tail
	}
	if p.n < minParts {
		return p, false
	}
	if clean {
		p.canon = v
		if kind == Extended {
			p.canon = p.label + " " + body
		}
		return p, true
	}
	var b strings.Builder
	if kind == Extended {
		b.WriteString(p.label)
		b.WriteByte(' ')
	}
	for i := 0; i < p.n; i++ {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(p.s[i])
	}
	p.canon = b.String()
	return p, true
}

// parseDecimal parses a non-empty ASCII decimal string of at most 10 digits no
// greater than max, reporting whether it is in canonical form (no leading zero).
func parseDecimal(s string, max uint64) (n uint64, canonical, ok bool) {
	if s == "" || len(s) > 10 {
		return 0, false, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false, false
		}
		n = n*10 + uint64(s[i]-'0')
	}
	if n > max {
		return 0, false, false
	}
	return n, len(s) == 1 || s[0] != '0', true
}

const (
	maxStd      = 65535
	maxU32      = 4294967295
	maxLabel    = 16
	maxCaptures = 16
)

// validLabel accepts the extended-community labels OpenBGPD prints (rt, soo,
// odi, bdc, srcas, l2vid, ...): lower-case letters, digits and '-'.
func validLabel(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func (e *entry) before(o *entry) bool {
	if e.space != o.space {
		return e.space < o.space
	}
	return e.order < o.order
}
