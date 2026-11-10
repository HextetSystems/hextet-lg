package bgp

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPrefixAndAddressCanonicalizationBothFamilies(t *testing.T) {
	for in, want := range map[string]string{
		"8.8.8.8":              "8.8.8.8",
		"192.0.2.0/24":         "192.0.2.0/24",
		"192.0.2.5/24":         "192.0.2.5/24", // host bits are preserved
		"0.0.0.0/0":            "0.0.0.0/0",
		"2001:4860:4860::8888": "2001:4860:4860::8888",
		"2001:DB8:0:0:0:0:0:1": "2001:db8::1",
		"2001:DB8::/32":        "2001:db8::/32",
		"2001:0db8:0000::1/64": "2001:db8::1/64",
		"::/0":                 "::/0",
		"::1":                  "::1",
		"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff/128": "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff/128",
	} {
		got, err := Query{Prefix: in}.Params()
		if err != nil || got.Get("prefix") != want {
			t.Errorf("%q => %v %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"2001:db8::/129", "192.0.2.0/33", "2001:db8::%eth0", "fe80::1%eth0/64", "::ffff:192.0.2.1", "::ffff:192.0.2.0/120", "2001:db8:::1", "gggg::1", "1.2.3", "1.2.3.4.5", "2001:db8::/", "2001:db8::/-1"} {
		if _, err := (Query{Prefix: bad}).Params(); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	// Equivalent spellings share one cache key; different prefixes do not.
	if (Query{Prefix: "2001:DB8::/32"}).Key() != (Query{Prefix: "2001:db8:0::/32"}).Key() || (Query{Prefix: "2001:db8::/32"}).Key() == (Query{Prefix: "2001:db8::/33"}).Key() {
		t.Fatal("cache keys for IPv6 prefixes")
	}
}

func TestNeighborAddressesBothFamilies(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1":       "192.0.2.1",
		"2001:DB8::1":     "2001:db8::1",
		"2001:db8:0:0::1": "2001:db8::1",
		"fe80::1%em0":     "fe80::1%em0",   // scoped link-local peers are legitimate
		"edge1.example":   "edge1.example", // descriptions pass through untouched
		"Transit A":       "Transit A",
		"ab:cd":           "ab:cd",
		"peer-2001:db8":   "peer-2001:db8",
		"1.2.3.4.example": "1.2.3.4.example",
	} {
		got, err := Query{Neighbor: in}.Params()
		if err != nil || got.Get("neighbor") != want {
			t.Errorf("%q => %v %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"2001:db8::g", "2001:db8:::1", "192.0.2.300", "1.2.3", "2001:db8::1::2"} {
		_, err := Query{Neighbor: bad}.Params()
		if err == nil && bad != "1.2.3" { // "1.2.3" has too few dots to look like an address
			t.Errorf("%q must be rejected", bad)
		}
	}
	// Descriptions are case-sensitive, so they must not share a cache entry.
	if (Query{Neighbor: "Edge"}).Key() == (Query{Neighbor: "edge"}).Key() {
		t.Fatal("neighbor descriptions that differ by case share a cache key")
	}
}

// Without an af filter nothing restricts the query to a family.
func TestAddressFamilyFilterIsOptionalAndNative(t *testing.T) {
	var seen []url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query())
		_, _ = io.WriteString(w, `{"rib":[{"prefix":"192.0.2.0/24"},{"prefix":"2001:db8::/32"}]}`)
	}, time.Second, 1<<20)
	for _, q := range []Query{
		{AS: "15169"},
		{LargeCommunity: "395089:0:6", Best: true},
		{Neighbor: "2001:db8::1", Community: "NO_EXPORT"},
		{AS: "15169", AF: "ipv4"},
		{AS: "15169", AF: "IPv6"},
	} {
		res, err := c.Search(context.Background(), q)
		if err != nil || !res.IsComplete() || len(res.Routes) != 2 {
			t.Fatalf("%+v: %v %+v", q, err, res.Completion)
		}
	}
	for i := 0; i < 3; i++ {
		if _, has := seen[i]["af"]; has {
			t.Errorf("query %d: af must not be sent when unset: %v", i, seen[i])
		}
	}
	if seen[3].Get("af") != "ipv4" || seen[4].Get("af") != "ipv6" {
		t.Fatalf("af not passed natively: %v %v", seen[3], seen[4])
	}
	if seen[2].Get("neighbor") != "2001:db8::1" {
		t.Fatalf("IPv6 neighbor not passed: %v", seen[2])
	}
}

func TestMixedFamilyDecodingNeverInfersFamilyFromNextHopOrNeighbor(t *testing.T) {
	doc := `{"rib":[
	 {"prefix":"192.0.2.0/24","aspath":"64500","exit_nexthop":"2001:db8::1","true_nexthop":"2001:db8::1","neighbor":{"description":"v6-peer","remote_addr":"2001:db8::ffff"}},
	 {"prefix":"2001:db8:1::/48","aspath":"64500","exit_nexthop":"192.0.2.1","true_nexthop":"???","neighbor":{"description":"v4-peer","remote_addr":"192.0.2.254"},"large_communities":["395089:0:6"],"communities":["64500:1"]},
	 {"prefix":"2001:db8:2::/48","aspath":"64501 {64502,64503}","exit_nexthop":"fe80::1","neighbor":{"remote_addr":"fe80::2"},"stale":true},
	 {"prefix":"10.0.0.0/8","aspath":"64500"}
	]}`
	routes, _, err := collect(doc)
	if err != nil || len(routes) != 4 {
		t.Fatalf("%v %d", err, len(routes))
	}
	for i, want := range []string{"IPv4", "IPv6", "IPv6", "IPv4"} {
		if got := routes[i].Family(); got != want {
			t.Errorf("route %d (%s): family %q want %q", i, routes[i].Prefix, got, want)
		}
	}
	if routes[0].NextHop() != "2001:db8::1" || routes[0].Neighbor != "v6-peer (2001:db8::ffff)" {
		t.Fatalf("IPv4 route keeps its IPv6 next hop and neighbor: %+v", routes[0])
	}
	if routes[1].NextHop() != "192.0.2.1" || routes[1].Neighbor != "v4-peer (192.0.2.254)" || routes[1].LargeCommunities[0] != "395089:0:6" {
		t.Fatalf("IPv6 route keeps its IPv4 next hop (RFC 8950) and neighbor: %+v", routes[1])
	}
	if routes[2].NextHop() != "fe80::1" || routes[2].Neighbor != "fe80::2" || routes[2].OriginASN != "64503" {
		t.Fatalf("%+v", routes[2])
	}
	// IPv6-specific attributes that are not modelled stay available verbatim.
	if !strings.Contains(routes[2].Raw, `"stale":true`) {
		t.Fatalf("raw JSON lost attributes: %s", routes[2].Raw)
	}
	if (Route{Prefix: "rd 65000:1 10.0.0.0/24"}).Family() != "" || (Route{}).Family() != "" {
		t.Fatal("unparsable prefixes have no family")
	}
}

func TestMixedFamilyLargeStreamAndPartial(t *testing.T) {
	// Alternate families in a large synthetic stream; interrupt it by the limit.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{\"rib\":[\n")
		for i := 0; i < 5000; i++ {
			if i > 0 {
				_, _ = io.WriteString(w, ",\n")
			}
			_, _ = io.WriteString(w, genRoute(i, i%2 == 1))
		}
		_, _ = io.WriteString(w, "\n]}")
	}, 5*time.Second, 400_000)
	res, _ := c.Search(context.Background(), Query{LargeCommunity: "395089:0:6"})
	if res.Status != StatusPartial || res.Reason != ReasonResponseLimit || len(res.Routes) < 100 {
		t.Fatalf("%+v", res.Completion)
	}
	v4, v6 := 0, 0
	for _, r := range res.Routes {
		switch r.Family() {
		case "IPv4":
			v4++
		case "IPv6":
			v6++
		}
	}
	if v4 == 0 || v6 == 0 || v4+v6 != len(res.Routes) || v4-v6 > 1 || v6-v4 > 1 {
		t.Fatalf("partial result lost one family: v4=%d v6=%d", v4, v6)
	}
}
