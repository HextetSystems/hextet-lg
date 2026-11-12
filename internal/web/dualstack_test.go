package web

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

const longV6 = "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff/128" // 43 characters: wider than any IPv4 prefix

// mixedDoc interleaves IPv4 and IPv6 routes, including cross-family next hops
// and neighbors, a maximum-length IPv6 prefix and a link-local next hop.
func mixedDoc(n int) string {
	var b strings.Builder
	b.WriteString("{\n\"rib\": [\n")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",\n")
		}
		switch i % 4 {
		case 0: // IPv4 route, IPv4 next hop and neighbor
			fmt.Fprintf(&b, `{"prefix":"10.%d.%d.0/24","aspath":"64500 65001","exit_nexthop":"192.0.2.1","true_nexthop":"192.0.2.1","neighbor":{"description":"v4-peer","remote_addr":"192.0.2.254"},"valid":true,"best":true,"ovs":"valid","avs":"unknown","localpref":100,"metric":0,"large_communities":["395089:0:8"]}`, (i>>8)&255, i&255)
		case 1: // IPv6 route, IPv6 next hop, IPv4 neighbor
			fmt.Fprintf(&b, `{"prefix":"2001:db8:%x::/48","aspath":"64500 65002","exit_nexthop":"2001:db8:ffff::1","true_nexthop":"???","neighbor":{"description":"v4-peer","remote_addr":"192.0.2.254"},"valid":true,"ovs":"not-found","avs":"unknown","localpref":100,"metric":10,"large_communities":["395089:0:8"]}`, i)
		case 2: // IPv6 route, IPv4 next hop, IPv6 neighbor
			fmt.Fprintf(&b, `{"prefix":"2001:db8:%x:1::/64","aspath":"64500 65003","exit_nexthop":"192.0.2.1","true_nexthop":"192.0.2.1","neighbor":{"description":"v6-peer","remote_addr":"2001:db8:ffff::2"},"valid":true,"ovs":"valid","avs":"unknown","localpref":200,"large_communities":["395089:0:8"],"stale":true}`, i)
		default: // IPv4 route, IPv6 next hop, link-local, widest IPv6 values
			fmt.Fprintf(&b, `{"prefix":"%s","aspath":"64500","exit_nexthop":"fe80::1","true_nexthop":"fe80::1","neighbor":{"remote_addr":"fe80::2"},"large_communities":["395089:0:8"]}`, longV6)
		}
	}
	b.WriteString("\n]\n}\n")
	return b.String()
}

func TestMixedFamilyHTMLTextAndDetailAreComplete(t *testing.T) {
	h := m48Handler(t, func(c *config.Config) { c.Display.RawJSON = true; c.Display.RouteColumns.Neighbor = true }, backendSpec{"a", "Alpha", serveDoc(mixedDoc(8))})
	base := "/?large-community=395089%3A0%3A8&backend=a"

	html := get(h, base+"&view=table").Body.String()
	for _, want := range []string{"10.0.0.0/24", "2001:db8:1::/48", "2001:db8:2:1::/64", longV6, "2001:db8:ffff::1", "fe80::1", "v6-peer", "2001:db8:ffff::2", "v4-peer", "192.0.2.254", "IPv4", "IPv6", "fe80::2"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	// Details: family comes from the prefix, and unmodelled IPv6 attributes remain in the raw JSON.
	if !strings.Contains(html, "&#34;stale&#34;: true") || !strings.Contains(html, "Address family") {
		t.Error("expanded details lack the family or raw attributes")
	}

	text := get(h, base+"&view=text&output=plain").Body.String()
	if !strings.Contains(text, longV6) || !strings.Contains(text, "2001:db8:1::/48") || !strings.Contains(text, "10.0.0.0/24") {
		t.Fatal("plaintext summary lost or truncated addresses")
	}
	// Columns stay aligned even though one prefix is wider than the IPv4 default.
	var cols []int
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "* ") || (strings.HasPrefix(line, "  ") && strings.Contains(line, "/")) {
			f := strings.Fields(line)
			nh := f[len(f)-1]
			_ = nh
			if i := strings.Index(line, " 192.0.2.1 "); i > 0 {
				cols = append(cols, i)
			}
		}
	}
	for _, c := range cols {
		if c != cols[0] {
			t.Fatalf("next-hop column misaligned: %v", cols)
		}
	}
	if len(cols) == 0 {
		t.Fatal("no aligned rows found")
	}
	hdr := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "Prefix") && strings.Contains(line, "Exit next hop") {
			hdr = line
		}
	}
	if strings.Index(hdr, "Exit next hop") != len(longV6)+3 {
		t.Fatalf("header not widened to the longest prefix: %q (exit next hop at %d)", hdr, strings.Index(hdr, "Exit next hop"))
	}

	detail := get(h, base+"&view=detail&output=plain").Body.String()
	for _, want := range []string{"Prefix: " + longV6 + "\n  Address family: IPv6", "Prefix: 10.0.0.0/24\n  Address family: IPv4", "Exit next hop: 2001:db8:ffff::1", "Neighbor: v6-peer (2001:db8:ffff::2)", "Exit next hop: 192.0.2.1", "Neighbor: fe80::2", "Large communities: 395089:0:8"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q", want)
		}
	}
	// The inline text view (HTML page) carries the same long values.
	if !strings.Contains(get(h, base+"&view=text").Body.String(), longV6) {
		t.Fatal("inline text view lost the wide prefix")
	}
}

// Omitting af must not silently exclude IPv6 (or IPv4); af is passed natively.
func TestOmittingAddressFamilyReturnsBothFamilies(t *testing.T) {
	var mu sync.Mutex
	var queries []url.Values
	doc := mixedDoc(8)
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		_, _ = io.WriteString(w, doc)
	}})
	body := get(h, "/?as=64500&backend=a&view=table").Body.String()
	if !strings.Contains(body, "2001:db8:1::/48") || !strings.Contains(body, "10.0.0.0/24") || !strings.Contains(body, " 8 routes ") {
		t.Fatal("mixed results missing")
	}
	if _, has := queries[0]["af"]; has {
		t.Fatalf("an address family was imposed: %v", queries[0])
	}
	get(h, "/?as=64500&af=IPv6&backend=a&view=table")
	if queries[1].Get("af") != "ipv6" {
		t.Fatalf("af=ipv6 not passed: %v", queries[1])
	}
	// The simple IPv4-style flow keeps working unchanged.
	get(h, "/?type=prefix&q=8.8.8.8&backend=a&view=table")
	if queries[2].Get("prefix") != "8.8.8.8" || len(queries[2]) != 1 {
		t.Fatalf("%v", queries[2])
	}
}

func TestIPv6PrefixSearchesAndStableURLs(t *testing.T) {
	var got []url.Values
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Query())
		_, _ = io.WriteString(w, mixedDoc(250))
	}})
	// Legacy simple form with an IPv6 prefix, in mixed case.
	rr := get(h, "/?type=prefix&q=2001%3ADB8%3A%3A%2F32&backend=a&view=table")
	if rr.Code != 200 || got[0].Get("prefix") != "2001:db8::/32" {
		t.Fatalf("%d %v", rr.Code, got)
	}
	// Canonical, escaped next-page URL.
	if !strings.Contains(rr.Body.String(), `href="/?type=prefix&amp;q=2001%3Adb8%3A%3A%2F32&amp;backend=a&amp;view=table&amp;page=2"`) {
		t.Fatal("IPv6 next-page link is not the canonical escaped URL")
	}
	// Compound IPv6 query: round trip through the canonical URL.
	rr = get(h, "/?prefix=2001:db8::/32&or-shorter=1&af=ipv6&neighbor=2001:DB8:FFFF::2&community=NO_EXPORT&backend=a&view=table")
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	last := got[len(got)-1]
	want := url.Values{"prefix": {"2001:db8::/32"}, "or-shorter": {"1"}, "af": {"ipv6"}, "neighbor": {"2001:db8:ffff::2"}, "community": {"NO_EXPORT"}}
	if len(last) != len(want) {
		t.Fatalf("%v", last)
	}
	for k, v := range want {
		if last.Get(k) != v[0] {
			t.Errorf("%s=%q want %q", k, last.Get(k), v[0])
		}
	}
	next := nextLink(rr.Body.String())
	// Canonical order: prefix, as, community, ext-community, large-community,
	// neighbor, group, af, rib, ovs, avs, then flags; backend, view, page last.
	if want := "/?prefix=2001%3Adb8%3A%3A%2F32&community=NO_EXPORT&neighbor=2001%3Adb8%3Affff%3A%3A2&af=ipv6&or-shorter=1&backend=a&view=table&page=2"; next != want {
		t.Fatalf("canonical URL\n got %s\nwant %s", next, want)
	}
	p2 := get(h, next)
	if p2.Code != 200 || !strings.Contains(p2.Body.String(), "Page 2 of 3") {
		t.Fatalf("following the link: %d", p2.Code)
	}
}

func TestIPv6QueryValidationMessages(t *testing.T) {
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", serveDoc("{}")})
	for q, want := range map[string]string{
		"prefix=2001:db8::/129":   "IPv4 or IPv6",
		"prefix=::ffff:192.0.2.1": "IPv4 or IPv6",
		"prefix=fe80::1%25eth0":   "IPv4 or IPv6",
		"neighbor=2001:db8::g":    "valid IPv4 or IPv6 address",
		"neighbor=192.0.2.300":    "valid IPv4 or IPv6 address",
		"af=ipv5&as=1":            "ipv4, ipv6",
	} {
		rr := get(h, "/?"+q+"&backend=a&view=text&output=plain")
		if rr.Code != 400 || !strings.Contains(rr.Body.String(), want) {
			t.Errorf("%s => %d %s", q, rr.Code, rr.Body.String())
		}
	}
}

func TestMixedFamilyPaginationAndPartialResults(t *testing.T) {
	lg := defaults()
	lg.QueryTimeout = 300 * time.Millisecond
	doc := strings.TrimSuffix(strings.TrimSpace(mixedDoc(300)), "]\n}")
	doc = strings.TrimSuffix(doc, "\n") + `,{"prefix":"2001:db8:dead::/4`
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Replace(doc, "\n]\n}", "", 1))
		flushW(w)
		<-r.Context().Done()
	}})
	base := "/?large-community=395089%3A0%3A8&backend=a&view=table"
	p1 := get(h, base).Body.String()
	p3 := get(h, base+"&page=3").Body.String()
	if rowCount(p1) != 100 || rowCount(p3) != 100 || !strings.Contains(p3, "Page 3 of 3") {
		t.Fatalf("rows %d %d", rowCount(p1), rowCount(p3))
	}
	for name, p := range map[string]string{"page1": p1, "page3": p3} {
		if !strings.Contains(p, "Partial results") || !strings.Contains(p, "IPv6") || strings.Contains(p, "2001:db8:dead") {
			t.Errorf("%s: partial banner missing, or the incomplete IPv6 route leaked", name)
		}
	}
	// Every page contains both families, so neither is lost to pagination.
	if !strings.Contains(p1, "2001:db8:1::/48") || !strings.Contains(p1, "10.0.0.0/24") {
		t.Fatal("page 1 lacks one family")
	}
	plain := get(h, strings.Replace(base, "view=table", "view=text", 1)+"&output=plain")
	if plain.Header().Get("X-Hextet-LG-Completeness") != "partial" || strings.Count(plain.Body.String(), "2001:db8:") < 150 {
		t.Fatalf("plain partial: %s", plain.Header().Get("X-Hextet-LG-Backends"))
	}
}

func TestIPv6NeighborsAndSummaryCountsOnNeighborsPage(t *testing.T) {
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"neighbors":[
		 {"remote_as":"64500","remote_addr":"2001:db8:ffff::2","description":"v6-peer","state":"Established","stats":{"prefixes":{"received":120000}}},
		 {"remote_as":"64501","remote_addr":"192.0.2.254","description":"v4-peer","state":"Idle"},
		 {"remote_as":"64502","remote_addr":"fe80::2%em0","state":"Established"}]}`)
	}})
	for _, page := range []string{"/neighbors?backend=a"} {
		body := get(h, page).Body.String()
		for _, want := range []string{"2001:db8:ffff::2", "192.0.2.254", "fe80::2%em0", "120000"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q", page, want)
			}
		}
	}
	if !strings.Contains(get(h, "/neighbors?backend=a").Body.String(), ">2</strong><span>Established") {
		t.Error("summary must count IPv6 and IPv4 sessions together")
	}
}

func TestCacheTreatsFamiliesAsDistinctQueries(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	lg := defaults()
	lg.CacheTTL = time.Minute
	doc := mixedDoc(8)
	h := newTestHandler(t, lg, backendSpec{"a", "Alpha", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.RawQuery]++
		mu.Unlock()
		_, _ = io.WriteString(w, doc)
	}})
	for _, u := range []string{"/?as=64500&backend=a", "/?as=64500&backend=a&page=1", "/?as=64500&af=ipv6&backend=a", "/?as=64500&af=ipv4&backend=a", "/?as=64500&af=IPV6&backend=a&page=1"} {
		get(h, u)
	}
	// No-af, ipv6 and ipv4 are three distinct upstream queries, each run once.
	if len(hits) != 3 {
		t.Fatalf("expected 3 distinct upstream queries, got %v", hits)
	}
	for q, n := range hits {
		if n != 1 {
			t.Errorf("%s ran %d times", q, n)
		}
	}
}

var nextRe = regexp.MustCompile(`rel="next" href="([^"]*)"`)

// nextLink returns the page's "next" link with HTML escaping undone.
func nextLink(body string) string {
	m := nextRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "&amp;", "&")
}
