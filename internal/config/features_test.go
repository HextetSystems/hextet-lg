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

func TestExampleConfig(t *testing.T) {
	if _, err := Load("../../examples/config.example.yaml"); err != nil {
		t.Fatal(err)
	}
}
