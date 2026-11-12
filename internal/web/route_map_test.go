package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

func TestRouteMapLinkAndPageUseCurrentBackendPaths(t *testing.T) {
	doc := `{"rib":[{"prefix":"203.0.113.0/24","aspath":"64500 64496","best":true,"neighbor":{"description":"peer-a","remote_addr":"192.0.2.1"}},{"prefix":"203.0.113.0/24","aspath":"64501 64496","neighbor":{"description":"peer-b","remote_addr":"192.0.2.2"}},{"prefix":"198.51.100.0/24","aspath":"64502"}]}`
	handler := m482Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(doc)})
	page := get(handler, "/?type=prefix&q=203.0.113.0%2F24&backend=a").Body.String()
	if got := strings.Count(page, `class="map-link"`); got != 2 {
		t.Fatalf("map icon count %d, want one for each row in the two-path prefix group", got)
	}
	if strings.Contains(page, `aria-label="Open BGP path map for 198.51.100.0/24"`) {
		t.Fatal("single-path prefix has map link")
	}
	match := regexp.MustCompile(`href="([^"]*?/map\?[^"]+)"`).FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatal("map URL missing")
	}
	target := html.UnescapeString(match[1])
	mapURL, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	var payload routeMapPayload
	data, err := base64.RawURLEncoding.DecodeString(mapURL.Query().Get("data"))
	if err != nil || json.Unmarshal(data, &payload) != nil || payload.FetchedAt.IsZero() || time.Since(payload.FetchedAt) > time.Minute {
		t.Fatalf("map URL did not preserve the recent route fetch time: %v", payload.FetchedAt)
	}
	rr := get(handler, target)
	if rr.Code != http.StatusOK {
		t.Fatalf("map page status %d: %s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{"BGP path map", "203.0.113.0/24", "peer-a", "peer-b", "AS64500", "AS64501", "AS64496", `class="map-best-label"`, ">BEST</text>", "Route data fetched at", "UTC", "Refresh this prefix in a new tab"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("map missing %q", want)
		}
	}
	if !strings.Contains(rr.Body.String(), `datetime="`+routeMapTime(payload.FetchedAt)+`"`) {
		t.Error("displayed fetch time does not match the timestamp carried in the map URL")
	}
	if got := strings.Count(rr.Body.String(), ">AS64496</text>"); got != 1 {
		t.Errorf("shared ASN node rendered %d times, want once", got)
	}
	if got := strings.Count(rr.Body.String(), `<line class="map-edge`); got != 5 {
		t.Errorf("graph has %d edges, want 5 with the shared tail merged", got)
	}
	refresh := regexp.MustCompile(`<a href="([^"]+)" target="_blank" rel="noopener">Refresh this prefix in a new tab</a>`).FindStringSubmatch(rr.Body.String())
	if len(refresh) != 2 {
		t.Fatal("new-tab prefix refresh link missing")
	}
	refreshURL, err := url.Parse(html.UnescapeString(refresh[1]))
	if err != nil || refreshURL.Path != "/" || refreshURL.Query().Get("type") != "prefix" || refreshURL.Query().Get("q") != "203.0.113.0/24" || refreshURL.Query().Get("backend") != "a" {
		t.Fatalf("refresh link is not a path-only prefix query for the same backend: %q", refresh[1])
	}
}

func TestRouteMapRejectsMalformedPayloadAndBadParameters(t *testing.T) {
	handler := m482Handler(t, nil, backendSpec{"a", "Alpha", serveDoc(`{"rib":[]}`)})
	invalidTime, _ := json.Marshal(routeMapPayload{Paths: []routeMapPath{{ASPath: "13335"}, {ASPath: "15169"}}})
	for _, path := range []string{"/map", "/map?prefix=203.0.113.0%2F24&backend=a&data=%25", "/map?prefix=203.0.113.0%2F24&backend=unknown&data=e30", "/map?prefix=203.0.113.0%2F24&backend=a&data=e30&extra=1", "/map?prefix=203.0.113.0%2F24&backend=a&data=" + base64.RawURLEncoding.EncodeToString(invalidTime)} {
		if rr := get(handler, path); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", path, rr.Code)
		}
	}
}

func TestRouteMapFeatureCanBeDisabled(t *testing.T) {
	doc := `{"rib":[{"prefix":"203.0.113.0/24","aspath":"13335"},{"prefix":"203.0.113.0/24","aspath":"15169"}]}`
	off := false
	handler := m482Handler(t, func(c *config.Config) { c.Features.BGPMaps = &off }, backendSpec{"a", "Alpha", serveDoc(doc)})
	page := get(handler, "/?type=prefix&q=203.0.113.0%2F24&backend=a").Body.String()
	if strings.Contains(page, `class="map-link"`) {
		t.Fatal("map link was shown while BGP maps are disabled")
	}
	payload, _ := json.Marshal(routeMapPayload{FetchedAt: time.Now().UTC(), Paths: []routeMapPath{{ASPath: "13335"}, {ASPath: "15169"}}})
	path := "/map?prefix=203.0.113.0%2F24&backend=a&data=" + base64.RawURLEncoding.EncodeToString(payload)
	if rr := get(handler, path); rr.Code != http.StatusNotFound {
		t.Fatalf("disabled map route status %d, want 404", rr.Code)
	}
}

func TestRouteMapEscapesLabelsAndUsesASNEnrichment(t *testing.T) {
	release := make(chan struct{})
	close(release)
	calls := make(chan string, 4)
	cfg, closeAll := testConfig(defaults(), backendSpec{"a", "Alpha", serveDoc(`{"rib":[]}`)})
	defer closeAll()
	cfg.ASNEnrichment = config.ASNDefaults()
	cfg.ASNEnrichment.Enabled = true
	h, err := NewHandler(cfg, WithASNResolver(mockASNResolver{release, calls}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	payload, _ := json.Marshal(routeMapPayload{FetchedAt: time.Now().UTC(), Paths: []routeMapPath{{ASPath: "13335", Peer: "<script>bad</script>"}, {ASPath: "15169"}}})
	values := url.Values{"prefix": {"203.0.113.0/24"}, "backend": {"a"}, "data": {base64.RawURLEncoding.EncodeToString(payload)}}
	rr := get(h.Routes(), "/map?"+values.Encode())
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "<script>bad") || !strings.Contains(rr.Body.String(), "&lt;script&gt;bad&lt;/script&gt;") {
		t.Fatal("peer label was not escaped in SVG")
	}
	if !strings.Contains(rr.Body.String(), `data-asn="13335" data-asn-label="true"`) {
		t.Fatal("ASN node is not marked for configured enrichment")
	}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("map did not schedule ASN lookups")
	}
}

func TestRouteMapURLHonorsConfiguredRequestLimits(t *testing.T) {
	rows := make([]string, 2)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"prefix":"203.0.113.0/24","aspath":%q}`, strings.Repeat("64500 ", 60))
	}
	doc := `{"rib":[` + strings.Join(rows, ",") + `]}`
	h := m482Handler(t, func(c *config.Config) { c.Server.MaxURLBytes = 700 }, backendSpec{"a", "Alpha", serveDoc(doc)})
	page := get(h, "/?type=prefix&q=203.0.113.0%2F24&backend=a").Body.String()
	if strings.Contains(page, `class="map-link"`) {
		t.Fatal("map link exceeded configured request URL limit")
	}
}

func TestRouteMapMergesASNsAcrossDifferentPathLengths(t *testing.T) {
	model, ok := buildRouteMap("203.0.113.0/24", "a", []routeMapPath{
		{ASPath: "13335 15169", Best: true},
		{ASPath: "20473 13335 15169"},
	}, false)
	if !ok {
		t.Fatal("valid paths did not produce a graph")
	}
	count := 0
	for _, node := range model.Nodes {
		if node.ASN == "13335" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared AS13335 rendered %d times, want one merged node", count)
	}
	if len(model.Edges) != 5 {
		t.Fatalf("graph has %d edges, want the shorter path to skip the extra hop", len(model.Edges))
	}
	var sharedY, branchY int
	for _, node := range model.Nodes {
		if node.ASN == "13335" {
			sharedY = node.Y
		}
		if node.ASN == "20473" {
			branchY = node.Y
		}
	}
	if sharedY != 110 || branchY != 160 {
		t.Fatalf("path lanes collapsed: shared ASN y=%d, alternate branch y=%d; want 110 and 160", sharedY, branchY)
	}
}
