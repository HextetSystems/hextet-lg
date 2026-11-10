// Package visibility compiles application-wide route disclosure rules.
package visibility

import (
	"fmt"
	"net/netip"
)

type Rule struct {
	Prefix string `yaml:"prefix"`
	Match  string `yaml:"match"`
}
type Exclusions struct {
	IPv4 []Rule `yaml:"ipv4"`
	IPv6 []Rule `yaml:"ipv6"`
}
type Config struct {
	Exclude Exclusions `yaml:"exclude"`
}
type compiledRule struct {
	prefix netip.Prefix
	longer bool
}
type Policy struct{ rules []compiledRule }

// Compile validates all rules and discards duplicates in their original order.
func Compile(c Config) (Policy, error) {
	var p Policy
	seen := make(map[compiledRule]bool)
	for _, family := range []struct {
		name  string
		rules []Rule
		v4    bool
	}{{"ipv4", c.Exclude.IPv4, true}, {"ipv6", c.Exclude.IPv6, false}} {
		for i, r := range family.rules {
			label := fmt.Sprintf("route_visibility.exclude.%s[%d]", family.name, i)
			prefix, err := netip.ParsePrefix(r.Prefix)
			if err != nil {
				return Policy{}, fmt.Errorf("%s.prefix must be a valid CIDR", label)
			}
			if prefix.Addr().Is4In6() || prefix.Addr().Is4() != family.v4 {
				return Policy{}, fmt.Errorf("%s.prefix has the wrong address family or is IPv4-mapped IPv6", label)
			}
			if prefix != prefix.Masked() {
				return Policy{}, fmt.Errorf("%s.prefix must be a canonical network prefix", label)
			}
			if r.Match != "exact" && r.Match != "or-longer" {
				return Policy{}, fmt.Errorf("%s.match must be exact or or-longer", label)
			}
			rule := compiledRule{prefix, r.Match == "or-longer"}
			if !seen[rule] {
				p.rules = append(p.rules, rule)
				seen[rule] = true
			}
		}
	}
	return p, nil
}

// Excludes fails closed on unparseable or mapped destinations when rules exist.
func (p Policy) Excludes(destination string) bool {
	if len(p.rules) == 0 {
		return false
	}
	prefix, err := netip.ParsePrefix(destination)
	if err != nil || prefix.Addr().Is4In6() {
		return true
	}
	prefix = prefix.Masked()
	for _, r := range p.rules {
		if r.prefix.Addr().Is4() != prefix.Addr().Is4() {
			continue
		}
		if prefix == r.prefix || (r.longer && prefix.Bits() >= r.prefix.Bits() && r.prefix.Contains(prefix.Addr())) {
			return true
		}
	}
	return false
}
