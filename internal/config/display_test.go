package config

import (
	"os"
	"strings"
	"testing"
)

func TestDisplayDefaults(t *testing.T) {
	// A configuration that omits the whole section keeps working and gets the
	// documented defaults: raw JSON off, Neighbor column hidden (format name),
	// RPKI and ASPA columns shown.
	c, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	d := c.Display
	if d.RawJSON || d.RouteQualification || d.RouteColumns.Neighbor || d.RouteColumns.Format() != NeighborFormatName || !d.RouteColumns.RPKIColumn() || !d.RouteColumns.ASPAColumn() || d.RouteColumns.NeighborNames() {
		t.Fatalf("defaults: %+v", d)
	}
	// The zero Config (programmatic use) resolves to the same defaults.
	var z Config
	if z.Display.RawJSON || z.Display.RouteQualification || z.Display.RouteColumns.Format() != NeighborFormatName || !z.Display.RouteColumns.RPKIColumn() || !z.Display.RouteColumns.ASPAColumn() {
		t.Fatalf("zero value: %+v", z.Display)
	}
}

func TestDisplayExplicitSettings(t *testing.T) {
	for _, tc := range []struct {
		name, yaml                string
		raw, neighbor, rpki, aspa bool
		format                    string
		names                     bool
	}{
		{"raw only", "display:\n  raw_json: true\n", true, false, true, true, "name", false},
		{"section present, empty columns", "display:\n  route_columns: {}\n", false, false, true, true, "name", false},
		{"neighbor ip", "display:\n  route_columns:\n    neighbor: true\n    neighbor_format: ip\n", false, true, true, true, "ip", false},
		{"neighbor name", "display:\n  route_columns:\n    neighbor: true\n    neighbor_format: name\n", false, true, true, true, "name", true},
		{"neighbor both", "display:\n  route_columns:\n    neighbor: true\n    neighbor_format: both\n", false, true, true, true, "both", true},
		{"neighbor default format", "display:\n  route_columns:\n    neighbor: true\n", false, true, true, true, "name", true},
		{"hidden format still validated and kept", "display:\n  route_columns:\n    neighbor_format: both\n", false, false, true, true, "both", false},
		{"rpki off", "display:\n  route_columns:\n    rpki: false\n", false, false, false, true, "name", false},
		{"aspa off", "display:\n  route_columns:\n    aspa: false\n", false, false, true, false, "name", false},
		{"both validation off", "display:\n  route_columns:\n    rpki: false\n    aspa: false\n", false, false, false, false, "name", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Decode(strings.NewReader(validConfig + tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			rc := c.Display.RouteColumns
			if c.Display.RawJSON != tc.raw || rc.Neighbor != tc.neighbor || rc.RPKIColumn() != tc.rpki || rc.ASPAColumn() != tc.aspa || rc.Format() != tc.format || rc.NeighborNames() != tc.names {
				t.Fatalf("%+v", c.Display)
			}
		})
	}
}

func TestDisplayRejectsInvalidSettings(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"display:\n  route_columns:\n    neighbor_format: hostname\n", `neighbor_format "hostname" must be one of: ip, name, both`},
		{"display:\n  route_columns:\n    neighbor_format: IP\n", "neighbor_format"},
		{"display:\n  route_columns:\n    neighbor: true\n    neighbor_format: fqdn\n", "neighbor_format"},
		{"display:\n  route_columns:\n    rpki: maybe\n", ""},
		{"display:\n  raw_json: yes please\n", ""},
		{"display:\n  route_columns:\n    asn: true\n", ""},
		{"display:\n  raw: true\n", ""},
	} {
		_, err := Decode(strings.NewReader(validConfig + tc.yaml))
		if err == nil {
			t.Errorf("%q: accepted", tc.yaml)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error %q lacks %q", tc.yaml, err, tc.want)
		}
	}
}

func TestExampleConfigDocumentsDisplay(t *testing.T) {
	b, err := os.ReadFile("../../examples/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"display:", "raw_json: false", "route_qualification: false", "route_columns:", "neighbor: false", "neighbor_format: name", "rpki: true", "aspa: true"} {
		if !strings.Contains(text, want) {
			t.Errorf("example config lacks %q", want)
		}
	}
	c, err := Load("../../examples/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := c.Display
	if d.RawJSON || d.RouteColumns.Neighbor || d.RouteColumns.Format() != NeighborFormatName || !d.RouteColumns.RPKIColumn() || !d.RouteColumns.ASPAColumn() {
		t.Fatalf("example config must show the defaults: %+v", d)
	}
}

func TestExistingConfigWithoutDisplayStillLoads(t *testing.T) {
	// The committed example predates nothing here, but every earlier section
	// combination must stay valid without "display".
	c, err := Decode(strings.NewReader(validConfig + "features:\n  page_timing: false\nptr_enrichment:\n  enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Display.RawJSON || c.Display.RouteColumns.Neighbor {
		t.Fatal("display defaults changed")
	}
}
