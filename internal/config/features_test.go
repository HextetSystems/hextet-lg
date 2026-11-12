package config

import (
	"strings"
	"testing"
)

func TestCommunityFeatureDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		yaml                      string
		standard, large, extended bool
	}{
		{"", true, true, false},
		{"features:\n  communities: true\n  large_communities: true\n  extended_communities: true\n", true, true, true},
		{"features:\n  communities: false\n  large_communities: false\n  extended_communities: false\n", false, false, false},
		{"features:\n  communities: false\n  extended_communities: true\n", false, true, true},
	} {
		c, err := Decode(strings.NewReader(validConfig + tc.yaml))
		if err != nil {
			t.Fatal(err)
		}
		if c.Features.CommunityEnabled() != tc.standard || c.Features.LargeCommunityEnabled() != tc.large || c.Features.ExtendedCommunityEnabled() != tc.extended {
			t.Fatalf("%q: %+v", tc.yaml, c.Features)
		}
	}
}

func TestBGPMapFeatureDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{name: "omitted defaults on", yaml: validConfig, want: true},
		{name: "explicitly on", yaml: validConfig + "features:\n  bgp_maps: true\n", want: true},
		{name: "explicitly off", yaml: validConfig + "features:\n  bgp_maps: false\n", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Decode(strings.NewReader(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.Features.BGPMapsEnabled(); got != tc.want {
				t.Fatalf("BGPMapsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
	for name, value := range map[string]string{"non-boolean": "maybe", "list": "[true]"} {
		t.Run(name, func(t *testing.T) {
			raw := validConfig + "features:\n  bgp_maps: " + value + "\n"
			if _, err := Decode(strings.NewReader(raw)); err == nil {
				t.Fatal("expected invalid bgp_maps value to be rejected")
			}
		})
	}
	example, err := Load("../../examples/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if example.Features.BGPMaps == nil || !example.Features.BGPMapsEnabled() {
		t.Fatal("the example must show bgp_maps enabled by default")
	}
}

func TestExampleConfig(t *testing.T) {
	if _, err := Load("../../examples/config.example.yaml"); err != nil {
		t.Fatal(err)
	}
}
