package visibility

import "testing"

func TestMatching(t *testing.T) {
	cases := []struct {
		rule     Rule
		v4       bool
		route    string
		excluded bool
	}{
		{Rule{"10.0.0.0/8", "exact"}, true, "10.0.0.0/8", true},
		{Rule{"10.0.0.0/8", "exact"}, true, "10.1.0.0/16", false},
		{Rule{"10.0.0.0/8", "or-longer"}, true, "10.1.0.0/16", true},
		{Rule{"10.0.0.0/8", "or-longer"}, true, "10.0.0.0/7", false},
		{Rule{"fd00::/8", "exact"}, false, "fd00::/8", true},
		{Rule{"fd00::/8", "exact"}, false, "fd12::/16", false},
		{Rule{"fd00::/8", "or-longer"}, false, "fd12::/16", true},
		{Rule{"fd00::/8", "or-longer"}, false, "fc00::/7", false},
		{Rule{"0.0.0.0/0", "or-longer"}, true, "192.0.2.1/32", true},
		{Rule{"192.0.2.1/32", "exact"}, true, "192.0.2.1/32", true},
		{Rule{"::/0", "or-longer"}, false, "2001:db8::1/128", true},
		{Rule{"2001:db8::1/128", "exact"}, false, "2001:db8::1/128", true},
		{Rule{"::/0", "or-longer"}, false, "192.0.2.0/24", false},
	}
	for _, tc := range cases {
		c := Config{}
		if tc.v4 {
			c.Exclude.IPv4 = []Rule{tc.rule}
		} else {
			c.Exclude.IPv6 = []Rule{tc.rule}
		}
		p, err := Compile(c)
		if err != nil || p.Excludes(tc.route) != tc.excluded {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	p, err := Compile(Config{Exclude: Exclusions{IPv4: []Rule{{"10.0.0.0/8", "or-longer"}, {"10.0.0.0/8", "or-longer"}, {"10.1.0.0/16", "exact"}}}})
	if err != nil || len(p.rules) != 2 || !p.Excludes("10.1.0.0/16") {
		t.Fatal("duplicates or overlapping rules")
	}
	empty, _ := Compile(Config{})
	if empty.Excludes("invalid") {
		t.Fatal("empty policy changed behavior")
	}
}
