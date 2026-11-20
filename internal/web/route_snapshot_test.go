package web

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestRouteSnapshotRoundTripKeepsPageAndLiveSearch(t *testing.T) {
	doc := `{"rib":[{"prefix":"203.0.113.0/24","aspath":"64500 64496","best":true,"neighbor":{"description":"peer-a","remote_addr":"192.0.2.1"}}]}`
	h := newRouteFixtureHandler(t, nil, backendSpec{"a", "Alpha", serveDoc(doc)})
	page := get(h, "/?type=prefix&q=203.0.113.0%2F24&backend=a").Body.String()
	m := regexp.MustCompile(`data-copy-value="([^"]*)"`).FindStringSubmatch(page)
	if len(m) != 2 {
		t.Fatal("snapshot button did not contain a URL")
	}
	snapshotURL := html.UnescapeString(m[1])
	if !strings.HasPrefix(snapshotURL, "/snapshot?data=") {
		t.Fatalf("unexpected snapshot URL: %q", snapshotURL)
	}
	rr := get(h, snapshotURL)
	if rr.Code != 200 {
		t.Fatalf("snapshot status %d: %s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{"Route snapshot", "Snapshot created at", "Run this search live", "value=\"203.0.113.0/24\"", "Route details"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("snapshot missing %q: %s", want, rr.Body.String())
		}
	}
}

func TestRouteSnapshotRejectsInvalidAndExpandedGzip(t *testing.T) {
	h := newRouteFixtureHandler(t, nil, backendSpec{"a", "Alpha", serveDoc(`{"rib":[]}`)})
	bad := "/snapshot?data=" + url.QueryEscape("not-gzip")
	if rr := get(h, bad); rr.Code != 400 {
		t.Fatalf("invalid gzip status %d", rr.Code)
	}
	var bomb strings.Builder
	for i := 0; i < maxRouteSnapshotBytes+1; i++ {
		bomb.WriteByte('a')
	}
	data := gzipBytes(t, []byte(bomb.String()))
	if rr := get(h, "/snapshot?data="+url.QueryEscape(base64URL(data))); rr.Code != 400 {
		t.Fatalf("expanded gzip status %d", rr.Code)
	}
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func TestGunzipBoundedRejectsUnsafeStreams(t *testing.T) {
	good := gzipBytes(t, []byte("hello"))
	if out, err := gunzipBounded(good, 5); err != nil || string(out) != "hello" {
		t.Fatalf("valid stream: %q, %v", out, err)
	}
	var named bytes.Buffer
	nw := gzip.NewWriter(&named)
	nw.Name = "x"
	nw.Write([]byte("hello"))
	nw.Close()
	var extra bytes.Buffer
	ew := gzip.NewWriter(&extra)
	ew.Extra = []byte("ab")
	ew.Write([]byte("hello"))
	ew.Close()
	cases := map[string][]byte{
		"over limit":     good[:len(good):len(good)],
		"trailing bytes": append(append([]byte{}, good...), 0),
		"second member":  append(append([]byte{}, good...), good...),
		"truncated":      good[:len(good)-3],
		"header name":    named.Bytes(),
		"header extra":   extra.Bytes(),
		"empty":          nil,
	}
	for name, in := range cases {
		limit := 5
		if name == "over limit" {
			limit = 4
		}
		if _, err := gunzipBounded(in, limit); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRouteSnapshotRejectsMalformedGzipShapes(t *testing.T) {
	h := newRouteFixtureHandler(t, nil, backendSpec{"a", "Alpha", serveDoc(`{"rib":[]}`)})
	good := gzipBytes(t, []byte(`{}`))
	for name, data := range map[string][]byte{
		"trailing": append(append([]byte{}, good...), 'x'),
		"multi":    append(append([]byte{}, good...), good...),
		"short":    good[:len(good)/2],
	} {
		if rr := get(h, "/snapshot?data="+url.QueryEscape(base64URL(data))); rr.Code != 400 {
			t.Errorf("%s: status %d", name, rr.Code)
		}
	}
	if rr := get(h, "/snapshot?data="+strings.Repeat("A", maxRouteSnapshotEncoded+1)); rr.Code != 400 && rr.Code != 414 {
		t.Errorf("oversize encoded: status %d", rr.Code)
	}
	if rr := get(h, "/snapshot?data=AA&data=AA"); rr.Code != 400 {
		t.Errorf("repeated data: status %d", rr.Code)
	}
}
