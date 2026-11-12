package config

import (
	"strings"
	"testing"
)

const siteBase = `server:
  listen: "127.0.0.1:8080"
looking_glass:
  default_backend: all
backends:
  - id: one
    name: One
    url: http://127.0.0.1:9099
    enabled: true
`

func decodeSite(t *testing.T, extra string) (Config, error) {
	t.Helper()
	return Decode(strings.NewReader(siteBase + extra))
}

func TestSiteSectionIsOptionalAndTitleNoLongerRequired(t *testing.T) {
	// Neither site nor server.title: the generic defaults apply.
	c, err := decodeSite(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Site.Name != "" || c.Site.Subtitle != nil || c.Site.ASN != 0 || c.Server.Title != "" {
		t.Fatalf("zero site expected: %+v", c.Site)
	}
	// An empty section is the same.
	if _, err := decodeSite(t, "site: {}\n"); err != nil {
		t.Fatal(err)
	}
	// A configuration written for earlier versions (server.title only) loads.
	if c, err := Decode(strings.NewReader(strings.Replace(siteBase, `listen: "127.0.0.1:8080"`, "listen: \"127.0.0.1:8080\"\n  title: \"Old LG\"", 1))); err != nil || c.Server.Title != "Old LG" {
		t.Fatalf("legacy title: %v %q", err, c.Server.Title)
	}
}

func TestSiteExplicitValues(t *testing.T) {
	c, err := decodeSite(t, `site:
  name: "Example LG"
  subtitle: ""
  operator: "Example Networks"
  asn: AS64500
  website: "https://example.net"
  contact: "noc@example.net"
  branding:
    footer: ""
`)
	if err != nil {
		t.Fatal(err)
	}
	s := c.Site
	if s.Name != "Example LG" || s.Subtitle == nil || *s.Subtitle != "" || s.ASN != 64500 || s.Branding.Footer == nil || *s.Branding.Footer != "" {
		t.Fatalf("%+v", s)
	}
}

func TestSiteInvalidValuesFailStartup(t *testing.T) {
	for name, extra := range map[string]string{
		"asn text":          "site:\n  asn: example\n",
		"asn negative":      "site:\n  asn: -5\n",
		"asn too big":       "site:\n  asn: 4294967296\n",
		"asn reserved":      "site:\n  asn: 23456\n",
		"website js":        "site:\n  website: \"javascript:alert(1)\"\n",
		"website data":      "site:\n  website: \"data:text/html,x\"\n",
		"website relative":  "site:\n  website: example.net\n",
		"contact":           "site:\n  contact: \"not an address\"\n",
		"name control":      "site:\n  name: \"a\\u0007b\"\n",
		"logo without dir":  "site:\n  branding:\n    logo: logo.png\n",
		"logo traversal":    "site:\n  branding:\n    directory: /etc/hextet-lg/branding\n    logo: ../logo.png\n",
		"logo type":         "site:\n  branding:\n    directory: /etc/hextet-lg/branding\n    logo: logo.exe\n",
		"unknown field":     "site:\n  colour: red\n",
		"unknown branding":  "site:\n  branding:\n    css: \"body{}\"\n",
		"name not a string": "site:\n  name: [a]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSite(t, extra); err == nil {
				t.Fatal("expected a startup error")
			}
		})
	}
}

func TestExampleConfigDecodes(t *testing.T) {
	c, err := Load("../../examples/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Site.Name == "" || c.Site.Operator == "" {
		t.Fatalf("example should demonstrate the site section: %+v", c.Site)
	}
	// It must not name Hextet's own deployment.
	if strings.Contains(strings.ToLower(c.Site.Operator+c.Site.Name+c.Site.Website+c.Site.Contact), "hextet") || c.Site.ASN == 395089 {
		t.Fatalf("example site section is operator-specific: %+v", c.Site)
	}
}
