package bgp

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestQueryValidCombinations(t *testing.T) {
	for name, q := range map[string]Query{
		"prefix":                 {Prefix: "8.8.8.8"},
		"ipv6 prefix":            {Prefix: "2001:db8::/32"},
		"as and large community": {AS: "AS15169", LargeCommunity: "395089:0:6"},
		"three communities":      {Community: "65000:100", ExtCommunity: "rt 65000:1", LargeCommunity: "395089:0:*"},
		"well-known community":   {Community: "no_export", AF: "IPv4"},
		"neighbor with state":    {Neighbor: "peer-a", Best: true, OVS: "valid", AVS: "unknown"},
		"group with rib table":   {Group: "transit", RIB: "Loc-RIB"},
		"prefix all":             {Prefix: "192.0.2.0/24", All: true},
		"prefix or-shorter+as":   {Prefix: "192.0.2.0/24", OrShorter: true, AS: "64500"},
		"wildcard large":         {LargeCommunity: "neighbor-as:*:local-as"},
		"error only":             {Error: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := q.Params(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQueryRejections(t *testing.T) {
	for name, tc := range map[string]struct {
		q    Query
		want string
	}{
		"empty":                 {Query{}, "at least one filter"},
		"neighbor and group":    {Query{Neighbor: "a", Group: "b"}, "mutually exclusive"},
		"two route states":      {Query{Best: true, Leaked: true}, "mutually exclusive"},
		"three route states":    {Query{Best: true, Error: true, Filtered: true}, "best, error, filtered"},
		"all and or-shorter":    {Query{Prefix: "10.0.0.0/8", All: true, OrShorter: true}, "all and or-shorter"},
		"all without prefix":    {Query{AS: "64500", All: true}, "all requires a prefix"},
		"or-shorter w/o prefix": {Query{Best: true, OrShorter: true}, "or-shorter requires a prefix"},
		"bad prefix":            {Query{Prefix: "bad"}, "IPv4 or IPv6"},
		"v4-mapped prefix":      {Query{Prefix: "::ffff:192.0.2.1"}, "IPv4 or IPv6"},
		"asn zero":              {Query{AS: "0"}, "source ASN"},
		"asn too large":         {Query{AS: "4294967296"}, "source ASN"},
		"asn non numeric":       {Query{AS: "AS15x"}, "source ASN"},
		"bad community":         {Query{Community: "1:2:3"}, "as:value"},
		"community overflow":    {Query{Community: "65536:1"}, "exceeds"},
		"bad large":             {Query{LargeCommunity: "1:2"}, "x:x:x"},
		"large overflow":        {Query{LargeCommunity: "1:2:4294967296"}, "unsigned 32-bit"},
		"bad af":                {Query{AF: "ipx"}, "ipv4, ipv6"},
		"bad ovs":               {Query{OVS: "maybe"}, "valid, invalid or not-found"},
		"bad avs":               {Query{AVS: "not-found"}, "valid, invalid or unknown"},
		"control characters":    {Query{Neighbor: "a\nb"}, "characters"},
		"too long":              {Query{Group: strings.Repeat("a", 256)}, "255"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.q.Params()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestQueryReportsAllProblems(t *testing.T) {
	_, err := Query{Neighbor: "a", Group: "b", AS: "x", Best: true, Error: true}.Normalize()
	ve, ok := err.(*ValidationError)
	if !ok || len(ve.Problems) != 3 {
		t.Fatalf("want 3 problems, got %#v", err)
	}
}

func TestUpstreamParams(t *testing.T) {
	q := Query{AS: "AS15169", LargeCommunity: "395089:0:6", Community: "no_export", ExtCommunity: "rt 65000:1", Neighbor: "peer a&b", AF: "IPv6", OVS: "valid", AVS: "unknown", Best: true, Prefix: "2001:db8::/32", OrShorter: true}
	v, err := q.Params()
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"as": {"15169"}, "large-community": {"395089:0:6"}, "community": {"NO_EXPORT"}, "ext-community": {"rt 65000:1"}, "neighbor": {"peer a&b"}, "af": {"ipv6"}, "ovs": {"valid"}, "avs": {"unknown"}, "best": {"1"}, "prefix": {"2001:db8::/32"}, "or-shorter": {"1"}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got %v", v)
	}
	// Standard, extended and large communities stay distinct upstream keys.
	enc := v.Encode()
	for _, s := range []string{"community=NO_EXPORT", "ext-community=rt+65000%3A1", "large-community=395089%3A0%3A6", "neighbor=peer+a%26b"} {
		if !strings.Contains(enc, s) {
			t.Errorf("%q missing from %s", s, enc)
		}
	}
	// A single occurrence of each key: repeated values are never produced.
	for k, vals := range v {
		if len(vals) != 1 {
			t.Errorf("%s repeated", k)
		}
	}
}

func TestParseQuery(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want Query
	}{
		"legacy prefix default type": {"q=8.8.8.8", Query{Prefix: "8.8.8.8"}},
		"legacy prefix":              {"type=prefix&q=8.8.8.8&backend=all&view=text", Query{Prefix: "8.8.8.8"}},
		"legacy asn":                 {"type=asn&q=AS15169", Query{AS: "AS15169"}},
		"legacy large":               {"type=large-community&q=395089%3A0%3A6", Query{LargeCommunity: "395089:0:6"}},
		"compound":                   {"as=15169&large-community=395089%3A0%3A6&best=1&af=ipv4", Query{AS: "15169", LargeCommunity: "395089:0:6", Best: true, AF: "ipv4"}},
		"legacy plus filters":        {"type=asn&q=15169&community=NO_EXPORT&neighbor=x", Query{AS: "15169", Community: "NO_EXPORT", Neighbor: "x"}},
		"empty form fields ignored":  {"type=prefix&q=&as=&community=&best=&endpoint=&large-community=1%3A2%3A3", Query{LargeCommunity: "1:2:3"}},
		"flag zero is unset":         {"best=0&as=64500", Query{AS: "64500"}},
	} {
		t.Run(name, func(t *testing.T) {
			v, _ := url.ParseQuery(tc.in)
			got, err := ParseQuery(v)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestParseQueryErrors(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"repeated as":         {"as=1&as=2", "more than once"},
		"repeated prefix":     {"prefix=10.0.0.0/8&prefix=11.0.0.0/8", "no OR"},
		"repeated q":          {"q=a&q=b", "more than once"},
		"bad flag":            {"best=yes&as=1", "expected 1"},
		"unsupported type":    {"type=aspath&q=1", "unsupported search type"},
		"type and filter dup": {"type=asn&q=1&as=2", "both as q"},
	} {
		t.Run(name, func(t *testing.T) {
			v, _ := url.ParseQuery(tc.in)
			if _, err := ParseQuery(v); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q got %v", tc.want, err)
			}
		})
	}
}

// Canonical public pairs must parse back to the same normalized query.
func TestPublicPairsRoundTrip(t *testing.T) {
	for _, q := range []Query{
		{Prefix: "8.8.8.8"},
		{AS: "15169"},
		{LargeCommunity: "395089:0:6"},
		{AS: "15169", LargeCommunity: "395089:0:6"},
		{Prefix: "2001:db8::/32", OrShorter: true, OVS: "invalid", AF: "ipv6"},
		{ExtCommunity: "rt 65000:1", Group: "g", Leaked: true},
	} {
		n, err := q.Normalize()
		if err != nil {
			t.Fatal(err)
		}
		v := url.Values{}
		for _, p := range n.PublicPairs() {
			v.Add(p[0], p[1])
		}
		back, err := ParseQuery(v)
		if err != nil {
			t.Fatal(err)
		}
		back, err = back.Normalize()
		if err != nil || !reflect.DeepEqual(back, n) {
			t.Fatalf("round trip %#v -> %v -> %#v (%v)", n, v, back, err)
		}
	}
}

func TestSimpleDetectionAndCacheKey(t *testing.T) {
	if typ, val, ok := (Query{AS: "15169"}).Simple(); !ok || typ != SearchASN || val != "15169" {
		t.Fatal("simple as")
	}
	if _, _, ok := (Query{AS: "15169", Best: true}).Simple(); ok {
		t.Fatal("compound is not simple")
	}
	a := Query{AS: "AS15169", Best: true}.Key()
	b := Query{Best: true, AS: "15169"}.Key()
	if a != b || a == (Query{AS: "15169"}).Key() {
		t.Fatalf("cache keys %q %q", a, b)
	}
	if (Query{AS: "1"}).Expensive() != true || (Query{Prefix: "10.0.0.0/8", AS: "1"}).Expensive() {
		t.Fatal("expensive classification")
	}
}
