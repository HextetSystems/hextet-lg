package web

import (
	"strings"
	"testing"
)

const classifierYAML = `
routeros:
  lists: ["1:0:1"]
  filters: {"1:0:2": "Customer classification"}
  patterns: ["1:[57]:.*"]
communities:
  - {value: "1:0:1", name: a, description: a, category: c}
  - {value: "1:0:2", name: b, description: b, category: c}
  - {value: "1:5:9", name: c, description: c, category: c}
  - {value: "1:7:2", name: d, description: d, category: c}
  - {value: "1:6:1", name: e, description: e, category: c}
  - {value: "1:0:3", name: f, description: f, category: c, status: unresolved}
  - {value: "1:0:4", name: g, description: g, category: c, status: unresolved}
`

func TestCommunityClassification(t *testing.T) {
	list, err := loadCommunities([]byte(classifierYAML))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{
		"1:0:1": {StatusImplemented, EvidenceList, ""},
		"1:0:2": {StatusImplemented, EvidenceFilter, "Customer classification"},
		"1:5:9": {StatusImplemented, EvidencePattern, "1:[57]:.*"},
		"1:7:2": {StatusImplemented, EvidencePattern, "1:[57]:.*"},
		"1:6:1": {StatusDocumented, "", ""},
		"1:0:3": {StatusUnresolved, "", ""},
	}
	for _, c := range list {
		w, ok := want[c.Value]
		if !ok {
			continue
		}
		if c.Status != w[0] || c.Evidence != w[1] || c.EvidenceNote != w[2] {
			t.Errorf("%s: %+v", c.Value, c)
		}
	}
}

func TestCommunityPatternsAreAnchored(t *testing.T) {
	list, err := loadCommunities([]byte(`
routeros: {patterns: ["1:[5]:.*"]}
communities:
  - {value: "1:5:2", name: a, description: a, category: c}
  - {value: "11:5:2", name: b, description: b, category: c}
  - {value: "1:55:2", name: c, description: c, category: c}
`))
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Status != StatusImplemented || list[1].Status != StatusDocumented || list[2].Status != StatusDocumented {
		t.Fatalf("%+v", list)
	}
}

func TestCommunityYAMLRejectsBadInput(t *testing.T) {
	for name, y := range map[string]string{
		"bad regexp": "routeros: {patterns: [\"(\"]}\ncommunities: []\n",
		"duplicate":  "communities:\n  - {value: a}\n  - {value: a}\n",
		"unknown":    "bogus: 1\n",
	} {
		if _, err := loadCommunities([]byte(y)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The embedded reference must reflect the RouterOS evidence from the brief.
func TestEmbeddedCommunityReference(t *testing.T) {
	data, err := assets.ReadFile("communities.yaml")
	if err != nil {
		t.Fatal(err)
	}
	list, err := loadCommunities(data)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Community{}
	for _, c := range list {
		by[c.Value] = c
	}
	evidence := strings.Fields(`395089:0:1 395089:0:2 395089:0:3 395089:0:4 395089:0:6 395089:0:8 395089:5:1 395089:5:2 395089:5:3 395089:5:4 395089:6:10 395089:6:11 395089:8:703 395089:1:50 395089:1:150 395089:1:450 395089:1:550 395089:2:0 395089:3:0 395089:4:0 395089:301:0 395089:302:0 395089:303:0 395089:7:1 395089:7:3 395089:7:4 395089:7:5`)
	for _, v := range evidence {
		c, ok := by[v]
		if !ok {
			t.Errorf("%s missing from reference", v)
			continue
		}
		if c.Status != StatusImplemented {
			t.Errorf("%s: %s, want Implemented", v, c.Status)
		}
	}
	for v, kind := range map[string]string{"395089:0:2": EvidenceFilter, "395089:1:150": EvidenceList, "395089:7:2": EvidencePattern, "395089:7:6": EvidencePattern, "395089:7:7": EvidencePattern, "395089:7:1": EvidenceFilter} {
		if by[v].Evidence != kind {
			t.Errorf("%s evidence %q want %q", v, by[v].Evidence, kind)
		}
	}
	if by["395089:0:2"].EvidenceNote != "Customer classification" || by["395089:8:703"].EvidenceNote != "MBIX" {
		t.Fatalf("%+v %+v", by["395089:0:2"], by["395089:8:703"])
	}
	// Meaning is never invented for communities the README does not describe.
	for _, v := range []string{"395089:5:3", "395089:6:11"} {
		if !strings.Contains(by[v].Name, "Not described") || !strings.Contains(by[v].Notes, "not inferred") {
			t.Errorf("%s: %+v", v, by[v])
		}
	}
}

func TestCommunityPageStatusesAndSearchLinks(t *testing.T) {
	h := newTestHandler(t, defaults(), backendSpec{"a", "Alpha", serveDoc("{}")})
	body := get(h, "/communities?backend=a").Body.String()
	for _, want := range []string{"Implemented", "routing filter: Customer classification", "community list", "configured pattern: 395089:[05678]:.*", `href="/?type=large-community&amp;q=395089%3A0%3A6&amp;backend=a"`, "not independent verification", "Documented/planned"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `muted">Unresolved`) {
		t.Fatal("no entry should be unresolved with the current evidence")
	}
	// Filtering is unchanged: q and category.
	body = get(h, "/communities?q=transit&category=Route+classification").Body.String()
	if !strings.Contains(body, "395089:0:6") || strings.Contains(body, "395089:0:8") {
		t.Fatal("filter")
	}
	// The status is searchable too.
	if !strings.Contains(get(h, "/communities?q=implemented").Body.String(), "395089:301:0") {
		t.Fatal("status search")
	}
}
