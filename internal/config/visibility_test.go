package config

import (
	"strings"
	"testing"
)

func TestVisibilityConfig(t *testing.T) {
	cases := []struct {
		name, section string
		valid         bool
	}{
		{"absent", "", true}, {"empty", "route_visibility:\n  exclude:\n    ipv4: []\n    ipv6: []\n", true},
		{"valid", "route_visibility:\n  exclude:\n    ipv4:\n      - {prefix: 10.0.0.0/8, match: or-longer}\n    ipv6:\n      - {prefix: 'fd00::/8', match: exact}\n", true},
		{"cidr", "      - {prefix: bad, match: exact}\n", false}, {"family", "      - {prefix: 'fd00::/8', match: exact}\n", false},
		{"mode", "      - {prefix: 10.0.0.0/8, match: longer}\n", false}, {"missing mode", "      - {prefix: 10.0.0.0/8}\n", false},
		{"missing prefix", "      - {match: exact}\n", false}, {"host bits", "      - {prefix: 10.1.0.0/8, match: exact}\n", false},
		{"mapped", "      - {prefix: '::ffff:192.0.2.0/120', match: exact}\n", false},
		{"mapped ipv6", "route_visibility:\n  exclude:\n    ipv6:\n      - {prefix: '::ffff:192.0.2.0/120', match: exact}\n", false},
		{"ipv4 in ipv6", "route_visibility:\n  exclude:\n    ipv6:\n      - {prefix: 10.0.0.0/8, match: exact}\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.section
			if strings.HasPrefix(s, "      -") {
				s = "route_visibility:\n  exclude:\n    ipv4:\n" + s
			}
			_, err := Decode(strings.NewReader(validConfig + "\n" + s))
			if (err == nil) != tc.valid {
				t.Fatalf("error: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "route_visibility.exclude.") {
				t.Fatal(err)
			}
		})
	}
}

func TestTrackedExampleConfiguration(t *testing.T) {
	if _, err := Load("../../configs/config.example.yaml"); err != nil {
		t.Fatal(err)
	}
}
