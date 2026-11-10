package bgp

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// SearchType is the legacy single-filter selector (`type` in public URLs).
type SearchType string

const (
	SearchPrefix         SearchType = "prefix"
	SearchASN            SearchType = "asn"
	SearchLargeCommunity SearchType = "large-community"
)

// Endpoint selects the bgplgd RIB endpoint.
type Endpoint string

const (
	EndpointRIB    Endpoint = "rib"
	EndpointRIBIn  Endpoint = "rib-in"
	EndpointRIBOut Endpoint = "rib-out"
)

// Path returns the bgplgd path for the endpoint.
func (e Endpoint) Path() string {
	switch e {
	case EndpointRIBIn:
		return "/rib/in"
	case EndpointRIBOut:
		return "/rib/out"
	default:
		return "/rib"
	}
}

// Label is the human-readable endpoint name.
func (e Endpoint) Label() string {
	switch e {
	case EndpointRIBIn:
		return "Adj-RIB-In"
	case EndpointRIBOut:
		return "Adj-RIB-Out"
	default:
		return "Loc-RIB"
	}
}

// Filter keys. They are deliberately identical to bgplgd's /rib parameters so
// that public application URLs and upstream requests share one vocabulary.
const (
	KeyPrefix         = "prefix"
	KeyAS             = "as"
	KeyCommunity      = "community"
	KeyExtCommunity   = "ext-community"
	KeyLargeCommunity = "large-community"
	KeyNeighbor       = "neighbor"
	KeyGroup          = "group"
	KeyAF             = "af"
	KeyRIB            = "rib"
	KeyOVS            = "ovs"
	KeyAVS            = "avs"
	KeyBest           = "best"
	KeyError          = "error"
	KeyFiltered       = "filtered"
	KeyInvalid        = "invalid"
	KeyLeaked         = "leaked"
	KeyAll            = "all"
	KeyOrShorter      = "or-shorter"
	KeyEndpoint       = "endpoint" // application-level, selects the bgplgd path
)

// StringFilterKeys and FlagFilterKeys list the supported filters in canonical
// order. Canonical public URLs and upstream summaries use this order.
var (
	StringFilterKeys = []string{KeyPrefix, KeyAS, KeyCommunity, KeyExtCommunity, KeyLargeCommunity, KeyNeighbor, KeyGroup, KeyAF, KeyRIB, KeyOVS, KeyAVS}
	FlagFilterKeys   = []string{KeyBest, KeyError, KeyFiltered, KeyInvalid, KeyLeaked, KeyAll, KeyOrShorter}
	// RouteStateKeys are mutually exclusive: bgplgd silently honours only the first.
	RouteStateKeys = []string{KeyBest, KeyError, KeyFiltered, KeyInvalid, KeyLeaked}
)

// Query is a compound, server-side /rib query. Every field is optional; the
// zero Query selects nothing. Each filter has at most one value: bgplgd rejects
// repeated parameters and has no OR semantics.
type Query struct {
	Endpoint Endpoint

	Prefix         string
	AS             string
	Community      string
	ExtCommunity   string
	LargeCommunity string
	Neighbor       string
	Group          string
	AF             string
	RIB            string
	OVS            string
	AVS            string

	Best, Error, Filtered, Invalid, Leaked, All, OrShorter bool
}

// SimpleQuery builds a legacy single-filter query.
func SimpleQuery(t SearchType, value string) Query {
	var q Query
	switch t {
	case SearchPrefix:
		q.Prefix = value
	case SearchASN:
		q.AS = value
	case SearchLargeCommunity:
		q.LargeCommunity = value
	}
	return q
}

// ValidationError lists every problem found in a query.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

func (q Query) endpoint() Endpoint {
	if q.Endpoint == "" {
		return EndpointRIB
	}
	return q.Endpoint
}

func (q *Query) stringField(key string) *string {
	switch key {
	case KeyPrefix:
		return &q.Prefix
	case KeyAS:
		return &q.AS
	case KeyCommunity:
		return &q.Community
	case KeyExtCommunity:
		return &q.ExtCommunity
	case KeyLargeCommunity:
		return &q.LargeCommunity
	case KeyNeighbor:
		return &q.Neighbor
	case KeyGroup:
		return &q.Group
	case KeyAF:
		return &q.AF
	case KeyRIB:
		return &q.RIB
	case KeyOVS:
		return &q.OVS
	case KeyAVS:
		return &q.AVS
	}
	return nil
}

func (q *Query) flagField(key string) *bool {
	switch key {
	case KeyBest:
		return &q.Best
	case KeyError:
		return &q.Error
	case KeyFiltered:
		return &q.Filtered
	case KeyInvalid:
		return &q.Invalid
	case KeyLeaked:
		return &q.Leaked
	case KeyAll:
		return &q.All
	case KeyOrShorter:
		return &q.OrShorter
	}
	return nil
}

// Get returns the value of a string filter, or "1" for a set flag.
func (q Query) Get(key string) string {
	if p := q.stringField(key); p != nil {
		return *p
	}
	if p := q.flagField(key); p != nil && *p {
		return "1"
	}
	return ""
}

// Filter is one active filter.
type Filter struct{ Key, Value string }

// Filters returns the active filters in canonical order.
func (q Query) Filters() []Filter {
	var out []Filter
	for _, k := range StringFilterKeys {
		if v := q.Get(k); v != "" {
			out = append(out, Filter{k, v})
		}
	}
	for _, k := range FlagFilterKeys {
		if v := q.Get(k); v != "" {
			out = append(out, Filter{k, v})
		}
	}
	return out
}

// IsEmpty reports whether no filter is set.
func (q Query) IsEmpty() bool { return len(q.Filters()) == 0 }

// Expensive reports whether the query lacks a prefix and may therefore ask
// OpenBGPD to walk a large part of the RIB.
func (q Query) Expensive() bool { return q.Prefix == "" }

// Simple reports whether the query is exactly one of the legacy single
// filters (prefix, source AS or Large Community) on the default endpoint.
func (q Query) Simple() (SearchType, string, bool) {
	if q.endpoint() != EndpointRIB {
		return "", "", false
	}
	f := q.Filters()
	if len(f) != 1 {
		return "", "", false
	}
	switch f[0].Key {
	case KeyPrefix:
		return SearchPrefix, f[0].Value, true
	case KeyAS:
		return SearchASN, f[0].Value, true
	case KeyLargeCommunity:
		return SearchLargeCommunity, f[0].Value, true
	}
	return "", "", false
}

// Normalize trims and canonicalizes values and validates every value and
// every combination. It reports all problems at once and never drops a filter.
func (q Query) Normalize() (Query, error) {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	switch q.Endpoint {
	case "", EndpointRIB:
		q.Endpoint = EndpointRIB
	case EndpointRIBIn, EndpointRIBOut:
	default:
		bad("unsupported endpoint %q (use rib, rib-in or rib-out)", string(q.Endpoint))
		q.Endpoint = EndpointRIB
	}

	for _, key := range StringFilterKeys {
		p := q.stringField(key)
		*p = strings.TrimSpace(*p)
		if *p == "" {
			continue
		}
		v, err := normalizeValue(key, *p)
		if err != nil {
			bad("%s: %v", key, err)
			continue
		}
		*p = v
	}

	if q.Neighbor != "" && q.Group != "" {
		bad("neighbor and group are mutually exclusive")
	}
	var states []string
	for _, k := range RouteStateKeys {
		if q.Get(k) != "" {
			states = append(states, k)
		}
	}
	if len(states) > 1 {
		bad("%s are mutually exclusive; choose one route state", strings.Join(states, ", "))
	}
	if q.All && q.OrShorter {
		bad("all and or-shorter are mutually exclusive")
	}
	if (q.All || q.OrShorter) && q.Prefix == "" {
		which := KeyAll
		if q.OrShorter {
			which = KeyOrShorter
		}
		bad("%s requires a prefix", which)
	}
	if q.RIB != "" && q.Endpoint != EndpointRIB {
		bad("rib (routing table name) is only supported on the Loc-RIB endpoint, not %s", q.Endpoint.Label())
	}
	if len(problems) > 0 {
		return q, &ValidationError{Problems: problems}
	}
	if q.IsEmpty() {
		return q, &ValidationError{Problems: []string{"specify at least one filter"}}
	}
	return q, nil
}

// Validate reports whether the query is valid.
func (q Query) Validate() error { _, err := q.Normalize(); return err }

// Params builds the upstream query string with net/url. The query is
// validated first; each filter appears exactly once.
func (q Query) Params() (url.Values, error) {
	n, err := q.Normalize()
	if err != nil {
		return nil, err
	}
	v := url.Values{}
	for _, f := range n.Filters() {
		v.Set(f.Key, f.Value)
	}
	return v, nil
}

// Key identifies the query (endpoint plus filters) for caching.
func (q Query) Key() string {
	n, err := q.Normalize()
	if err != nil {
		return "invalid"
	}
	v := url.Values{}
	for _, f := range n.Filters() {
		v.Set(f.Key, f.Value) // values are canonical; some (descriptions) are case-sensitive
	}
	return string(n.Endpoint) + "?" + v.Encode()
}

// ParseQuery builds a Query from public URL values. It accepts the legacy
// `type`+`q` single-search form and the compound named-filter form, which may
// be mixed. Empty values are ignored (browsers submit empty form fields), a
// flag value of "0" means unset, and any repeated parameter is an error.
func ParseQuery(v url.Values) (Query, error) {
	var q Query
	var problems []string
	single := func(key string) string {
		vals := v[key]
		if len(vals) > 1 {
			problems = append(problems, fmt.Sprintf("%s was given more than once; repeated values are not supported (no OR queries)", key))
		}
		if len(vals) == 0 {
			return ""
		}
		return strings.TrimSpace(vals[0])
	}

	q.Endpoint = Endpoint(single(KeyEndpoint))
	for _, key := range StringFilterKeys {
		*q.stringField(key) = single(key)
	}
	for _, key := range FlagFilterKeys {
		switch s := single(key); s {
		case "", "0":
		case "1":
			*q.flagField(key) = true
		default:
			problems = append(problems, fmt.Sprintf("%s: expected 1", key))
		}
	}

	// Legacy simple search: type=prefix|asn|large-community and q=value.
	typ, val := single("type"), single("q")
	if val != "" {
		var key string
		switch SearchType(typ) {
		case "", SearchPrefix:
			key = KeyPrefix
		case SearchASN:
			key = KeyAS
		case SearchLargeCommunity:
			key = KeyLargeCommunity
		default:
			problems = append(problems, "unsupported search type")
		}
		if key != "" {
			if p := q.stringField(key); *p != "" {
				problems = append(problems, fmt.Sprintf("%s was given both as q (type=%s) and as a %s filter", key, typ, key))
			} else {
				*p = val
			}
		}
	} else if typ != "" {
		switch SearchType(typ) {
		case SearchPrefix, SearchASN, SearchLargeCommunity:
		default:
			problems = append(problems, "unsupported search type")
		}
	}

	if len(problems) > 0 {
		return q, &ValidationError{Problems: problems}
	}
	return q, nil
}

// PublicPairs returns the canonical application URL query pairs for a query:
// the legacy `type`/`q` form for a single prefix, source-AS or Large Community
// search (so existing bookmarks stay canonical), otherwise named filters in
// canonical order. Unvalidated queries are emitted as given.
func (q Query) PublicPairs() [][2]string {
	if t, val, ok := q.Simple(); ok {
		return [][2]string{{"type", string(t)}, {"q", val}}
	}
	var pairs [][2]string
	if q.endpoint() != EndpointRIB {
		pairs = append(pairs, [2]string{KeyEndpoint, string(q.Endpoint)})
	}
	for _, f := range q.Filters() {
		pairs = append(pairs, [2]string{f.Key, f.Value})
	}
	return pairs
}

func normalizeValue(key, v string) (string, error) {
	if len(v) > 255 {
		return "", errors.New("value is longer than 255 characters")
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return "", errors.New("value contains characters that OpenBGPD does not accept")
		}
	}
	switch key {
	case KeyPrefix:
		return normalizePrefix(v)
	case KeyAS:
		s := strings.TrimPrefix(strings.ToUpper(v), "AS")
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil || n == 0 {
			return "", errors.New("enter a valid source ASN (1–4294967295)")
		}
		return strconv.FormatUint(n, 10), nil
	case KeyCommunity:
		return normalizeCommunity(v)
	case KeyLargeCommunity:
		return normalizeLarge(v)
	case KeyNeighbor:
		return normalizeNeighbor(v)
	case KeyExtCommunity, KeyGroup, KeyRIB:
		// OpenBGPD validates the grammar of these (ext-community has many subtypes).
		return v, nil
	case KeyAF:
		s := strings.ToLower(v)
		switch s {
		case "ipv4", "ipv6", "vpnv4", "vpnv6":
			return s, nil
		}
		return "", errors.New("expected ipv4, ipv6, vpnv4 or vpnv6")
	case KeyOVS:
		switch v {
		case "valid", "invalid", "not-found":
			return v, nil
		}
		return "", errors.New("expected valid, invalid or not-found")
	case KeyAVS:
		switch v {
		case "valid", "invalid", "unknown":
			return v, nil
		}
		return "", errors.New("expected valid, invalid or unknown")
	}
	return v, nil
}

// normalizePrefix accepts an IPv4 or IPv6 address or prefix and returns its
// canonical text (IPv6 compressed and lower-case). Host bits are preserved so
// that OpenBGPD sees exactly the prefix the user asked about. IPv4-mapped IPv6
// addresses and zones are rejected: they are ambiguous in a route lookup.
func normalizePrefix(v string) (string, error) {
	const msg = "enter an IPv4 or IPv6 address or prefix"
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		if err != nil || p.Addr().Is4In6() || p.Addr().Zone() != "" {
			return "", errors.New(msg)
		}
		return p.String(), nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil || a.Is4In6() || a.Zone() != "" {
		return "", errors.New(msg)
	}
	return a.String(), nil
}

// normalizeNeighbor accepts a neighbor address (IPv4 or IPv6, canonicalized) or
// a free-form description. Text that is evidently meant as an address but does
// not parse is rejected instead of being sent as a description that matches
// nothing.
func normalizeNeighbor(v string) (string, error) {
	if a, err := netip.ParseAddr(v); err == nil {
		return a.String(), nil
	}
	if looksLikeAddress(v) {
		return "", errors.New("not a valid IPv4 or IPv6 address (give a neighbor address or its description)")
	}
	return v, nil
}

// looksLikeAddress reports whether v is evidently an attempt at an IP address,
// so that a typo such as "2001:db8::g" or "192.0.2.300" is reported instead of
// being sent to OpenBGPD as a description that matches nothing. An IPv6 attempt
// has "::" or the full eight-group shape; an IPv4 attempt is four dotted
// numbers. Ordinary descriptions ("edge1.example", "peer-a", "ab:cd") are not.
func looksLikeAddress(v string) bool {
	host, _, _ := strings.Cut(v, "%")
	if host == "" {
		return false
	}
	alnum := true
	for _, r := range host {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == ':' || r == '.') {
			alnum = false
		}
	}
	colons := strings.Count(host, ":")
	if colons >= 2 && alnum && (strings.Contains(host, "::") || colons == 7) {
		return true
	}
	return colons == 0 && strings.Count(host, ".") == 3 && strings.Trim(host, "0123456789.") == ""
}

var wellKnownCommunities = []string{"GRACEFUL_SHUTDOWN", "NO_EXPORT", "NO_ADVERTISE", "NO_EXPORT_SUBCONFED", "NO_PEER", "BLACKHOLE", "DOWNGRADE"}

// communityField accepts a number up to max, or bgpctl's wildcard tokens.
func communityField(s string, max uint64) error {
	switch s {
	case "*", "neighbor-as", "local-as":
		return nil
	}
	if s == "" {
		return errors.New("empty field")
	}
	if _, err := strconv.ParseUint(s, 10, 64); err != nil {
		return fmt.Errorf("%q is not a number, *, neighbor-as or local-as", s)
	}
	n, _ := strconv.ParseUint(s, 10, 64)
	if n > max {
		return fmt.Errorf("%s exceeds %d", s, max)
	}
	return nil
}

func normalizeCommunity(v string) (string, error) {
	for _, w := range wellKnownCommunities {
		if strings.EqualFold(v, w) {
			return w, nil
		}
	}
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return "", errors.New("enter a standard community as as:value (16-bit fields) or a well-known name")
	}
	for _, p := range parts {
		if err := communityField(p, 65535); err != nil {
			return "", err
		}
	}
	return v, nil
}

func normalizeLarge(v string) (string, error) {
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return "", errors.New("enter a large community in x:x:x format")
	}
	for _, p := range parts {
		if err := communityField(p, 4294967295); err != nil {
			return "", errors.New("each large community field must be an unsigned 32-bit integer (or *, neighbor-as, local-as)")
		}
	}
	return v, nil
}
