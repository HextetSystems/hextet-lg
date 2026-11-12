package bgp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// genRoute returns one realistic bgpctl -j route object (tab-indented).
func genRoute(i int, v6 bool) string {
	prefix := fmt.Sprintf("10.%d.%d.0/24", (i>>8)&255, i&255)
	if v6 {
		prefix = fmt.Sprintf("2001:db8:%x::/48", i)
	}
	return fmt.Sprintf("\t{\n\t\t\"prefix\": %q,\n\t\t\"aspath\": \"64500 %d\",\n\t\t\"exit_nexthop\": \"192.0.2.1\",\n\t\t\"true_nexthop\": \"192.0.2.1\",\n\t\t\"neighbor\": { \"description\": \"peer-a\", \"remote_addr\": \"192.0.2.1\", \"bgp_id\": \"192.0.2.1\" },\n\t\t\"valid\": true,\n\t\t\"best\": true,\n\t\t\"source\": \"external\",\n\t\t\"ovs\": \"valid\",\n\t\t\"avs\": \"unknown\",\n\t\t\"origin\": \"igp\",\n\t\t\"metric\": 0,\n\t\t\"localpref\": 100,\n\t\t\"weight\": 0,\n\t\t\"dmetric\": 0,\n\t\t\"last_update\": \"1h\",\n\t\t\"last_update_sec\": 3600,\n\t\t\"communities\": [ \"64500:1\" ],\n\t\t\"extended_communities\": [ \"rt 64500:1\" ],\n\t\t\"large_communities\": [ \"395089:0:6\", \"395089:5:2\" ]\n\t}", prefix, 65000+i%100)
}

// genReader streams {"rib":[...]} on demand without ever holding the document.
type genReader struct {
	n, i    int
	v6      bool
	pending []byte
	state   int // 0 head, 1 routes, 2 tail, 3 done
	tail    string
	read    int64
}

func newGen(n int, v6 bool) *genReader {
	return &genReader{n: n, v6: v6, tail: "\n]\n}\n"}
}

func (g *genReader) Read(p []byte) (int, error) {
	for len(g.pending) == 0 {
		switch g.state {
		case 0:
			g.pending, g.state = []byte("{\n\"rib\": [\n"), 1
		case 1:
			if g.i >= g.n {
				g.state = 2
				continue
			}
			s := genRoute(g.i, g.v6)
			if g.i > 0 {
				s = ",\n" + s
			}
			g.i++
			g.pending = []byte(s)
		case 2:
			g.pending, g.state = []byte(g.tail), 3
		default:
			return 0, io.EOF
		}
	}
	n := copy(p, g.pending)
	g.pending = g.pending[n:]
	g.read += int64(n)
	return n, nil
}

func collect(doc string) ([]Route, bool, error) {
	var routes []Route
	failed, err := decodeStream(strings.NewReader(doc), func(r Route) error { routes = append(routes, r); return nil })
	return routes, failed, err
}

func TestDecodeStreamComplete(t *testing.T) {
	for name, tc := range map[string]struct {
		doc  string
		want int
	}{
		"bgpctl empty document": {"{\n\n}\n", 0},
		"empty rib":             {`{"rib":[]}`, 0},
		"bare array":            {`[]`, 0},
		"one route":             {`{"rib":[{"prefix":"8.8.8.0/24"}]}`, 1},
		"legacy routes key":     {`{"routes":[{"prefix":"a"},{"prefix":"b"}]}`, 2},
		"status ok":             {`{"rib":[{"prefix":"a"}],"status":"OK"}`, 1},
		"null rib":              {`{"rib":null}`, 0},
		"trailing whitespace":   {"{\"rib\":[{\"prefix\":\"a\"}]}\n\n  ", 1},
	} {
		t.Run(name, func(t *testing.T) {
			routes, failed, err := collect(tc.doc)
			if err != nil || failed || len(routes) != tc.want {
				t.Fatalf("routes=%d failed=%v err=%v", len(routes), failed, err)
			}
		})
	}
}

func TestDecodeStreamRouteAttributes(t *testing.T) {
	doc := "{\"rib\":[" + genRoute(1, false) + "," + genRoute(2, true) + `,{"prefix":"192.0.2.0/24","aspath":"64500 {64501,64502}"}` + "]}"
	routes, _, err := collect(doc)
	if err != nil || len(routes) != 3 {
		t.Fatalf("%v %d", err, len(routes))
	}
	r := routes[0]
	if r.Prefix != "10.0.1.0/24" || r.Neighbor != "peer-a (192.0.2.1)" || r.LocalPref == nil || *r.LocalPref != 100 || r.Valid == nil || !*r.Valid || !r.Best {
		t.Fatalf("attributes: %#v", r)
	}
	if len(r.Communities) != 1 || len(r.ExtCommunities) != 1 || len(r.LargeCommunities) != 2 || r.OriginASN != "65001" {
		t.Fatalf("communities must stay distinct: %#v", r)
	}
	if strings.ContainsAny(r.Raw, "\n\t") || !strings.HasPrefix(r.Raw, `{"prefix":"10.0.1.0/24"`) {
		t.Fatalf("raw JSON not compacted: %q", r.Raw)
	}
	if routes[1].Prefix != "2001:db8:2::/48" {
		t.Fatal("ipv6 prefix", routes[1].Prefix)
	}
	if o := routes[2]; o.LocalPref != nil || o.Valid != nil || o.Neighbor != "" || o.OriginASN != "64502" {
		t.Fatalf("missing optional attributes must stay absent: %#v", o)
	}
}

func TestDecodeStreamMultiplePathsPerPrefix(t *testing.T) {
	routes, _, err := collect(`{"rib":[{"prefix":"8.8.8.0/24","aspath":"1"},{"prefix":"8.8.8.0/24","aspath":"2"},{"prefix":"8.8.8.0/24","aspath":"3"}]}`)
	if err != nil || len(routes) != 3 || routes[0].ASPath != "1" || routes[2].ASPath != "3" {
		t.Fatalf("%v %#v", err, routes)
	}
}

func TestDecodeStreamFailures(t *testing.T) {
	two := `{"prefix":"a"},{"prefix":"b"}`
	for name, tc := range map[string]struct {
		doc        string
		routes     int
		is         error
		wantSyntax bool
	}{
		"incomplete final route":   {`{"rib":[` + two + `,{"prefix":"c","aspa`, 2, errTruncated, false},
		"missing closing array":    {`{"rib":[` + two, 2, errTruncated, false},
		"missing closing object":   {`{"rib":[` + two + `]`, 2, errTruncated, false},
		"unexpected eof empty":     {``, 0, errTruncated, false},
		"eof after open":           {`{"rib":[`, 0, errTruncated, false},
		"invalid json mid-route":   {`{"rib":[` + two + `,{"prefix":}]}`, 2, nil, true},
		"invalid json garbage":     {`{"rib":[` + two + `, garbage]}`, 2, nil, true},
		"trailing comma":           {`{"rib":[{"prefix":"a"},]}`, 1, nil, true},
		"trailing data":            {`{"rib":[` + two + `]} {"x":1}`, 2, errTrailing, false},
		"trailing garbage":         {`{"rib":[` + two + `]}xyz`, 2, errTrailing, false},
		"route not an object":      {`{"rib":[` + two + `,5]}`, 2, errStructure, false},
		"null route":               {`{"rib":[null]}`, 0, errStructure, false},
		"rib not an array":         {`{"rib":{"prefix":"a"}}`, 0, errStructure, false},
		"unexpected top level":     {`"hello"`, 0, errStructure, false},
		"wrong document":           {`{"neighbors":[]}`, 0, errStructure, false},
		"unexpected key after rib": {`{"rib":[` + two + `],"extra":1}`, 2, errStructure, false},
	} {
		t.Run(name, func(t *testing.T) {
			routes, _, err := collect(tc.doc)
			if err == nil {
				t.Fatal("document must not be reported complete")
			}
			if len(routes) != tc.routes {
				t.Fatalf("routes=%d want %d (err %v)", len(routes), tc.routes, err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("err=%v want %v", err, tc.is)
			}
			if tc.wantSyntax && !isSyntaxError(err) {
				t.Fatalf("expected JSON syntax error, got %T %v", err, err)
			}
		})
	}
}

func TestDecodeStreamStatusFailed(t *testing.T) {
	_, failed, err := collect(`{"status":"FAILED","error":"permission denied"}`)
	if err != nil || !failed {
		t.Fatal(failed, err)
	}
}

// A large document is processed incrementally: the first route is delivered
// after reading only a tiny fraction of the stream, and nothing buffers it all.
func TestDecodeStreamIsIncremental(t *testing.T) {
	g := newGen(60000, false)
	var readAtFirst int64
	count := 0
	_, err := decodeStream(g, func(Route) error {
		if count == 0 {
			readAtFirst = g.read
		}
		count++
		return nil
	})
	if err != nil || count != 60000 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if readAtFirst > 16*1024 || g.read < 20<<20 {
		t.Fatalf("not streaming: read %d bytes before first route, %d total", readAtFirst, g.read)
	}
}

func TestDecodeStreamIPv6Large(t *testing.T) {
	n := 0
	if _, err := decodeStream(newGen(5000, true), func(r Route) error {
		if !strings.Contains(r.Prefix, "2001:db8:") {
			t.Fatal(r.Prefix)
		}
		n++
		return nil
	}); err != nil || n != 5000 {
		t.Fatal(n, err)
	}
}

func TestBoundedReader(t *testing.T) {
	// Exactly at the limit is fine.
	br := &boundedReader{r: strings.NewReader("12345"), max: 5}
	b, err := io.ReadAll(br)
	if err != nil || string(b) != "12345" || br.err != nil {
		t.Fatalf("%q %v", b, err)
	}
	// One byte over fails, and the failure is sticky.
	br = &boundedReader{r: strings.NewReader("123456"), max: 5}
	b, err = io.ReadAll(br)
	if !errors.Is(err, ErrResponseLimit) || len(b) != 5 || br.n != 5 {
		t.Fatalf("%q %v n=%d", b, err, br.n)
	}
	if _, err = br.Read(make([]byte, 4)); !errors.Is(err, ErrResponseLimit) {
		t.Fatal("limit error must be sticky")
	}
}

func TestMemSizeGrowsWithContent(t *testing.T) {
	var buf bytes.Buffer
	small, _ := parseRoute([]byte(`{"prefix":"a"}`), &buf)
	big, _ := parseRoute([]byte(strings.TrimSpace(genRoute(1, false))), &buf)
	if small.MemSize() <= 0 || big.MemSize() <= small.MemSize() {
		t.Fatalf("%d %d", small.MemSize(), big.MemSize())
	}
}

func TestNextHopUsesOnlyExitNexthop(t *testing.T) {
	for _, tc := range []struct {
		r    Route
		want string
	}{
		{Route{TrueNexthop: "192.0.2.1", ExitNexthop: "198.51.100.1"}, "198.51.100.1"},
		{Route{TrueNexthop: "???", ExitNexthop: "198.51.100.1"}, "198.51.100.1"},
		{Route{TrueNexthop: "192.0.2.1"}, ""},
		{Route{ExitNexthop: "198.51.100.1"}, "198.51.100.1"},
	} {
		if got := tc.r.NextHop(); got != tc.want {
			t.Errorf("%+v => %q", tc.r, got)
		}
	}
}
