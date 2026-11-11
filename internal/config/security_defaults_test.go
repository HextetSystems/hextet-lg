package config

import (
	"strings"
	"testing"
)

// A configuration that says nothing about the optional, disclosure-related
// settings must come up with every one of them in its safe position (M5.2).
func TestM52MinimalConfigHasSafeDefaults(t *testing.T) {
	c, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Display.RawJSON {
		t.Error("display.raw_json must default to false")
	}
	if c.Features.RTRSessionAddressesEnabled() {
		t.Error("features.rtr_session_addresses must default to false")
	}
	if c.Features.ExtendedCommunityEnabled() {
		t.Error("features.extended_communities must default to false")
	}
	if c.Metrics.Enabled || !c.Metrics.LoopbackOnly() {
		t.Errorf("metrics must be off and loopback by default: %+v", c.Metrics)
	}
	if c.ASNEnrichment.Enabled || c.PTREnrichment.Enabled || c.ASNEnrichment.Cache.Persist {
		t.Error("DNS enrichment and cache persistence must be opt-in")
	}
	if c.CommunityDefinitions.Enabled || c.Display.RouteColumns.Neighbor {
		t.Error("community definitions and the Neighbor column must be opt-in")
	}
	if len(c.RouteVisibility.Exclude.IPv4)+len(c.RouteVisibility.Exclude.IPv6) != 0 {
		t.Error("no route is excluded by default")
	}
	if c.Site.Branding.Directory != "" || c.Site.Branding.Logo != "" || c.Site.Branding.Favicon != "" {
		t.Error("no branding asset is read by default")
	}
	if c.Server.RatePerSecond <= 0 || c.Server.LiveRatePerSecond <= 0 || c.Server.MaxURLBytes <= 0 || c.Server.MaxHeaderBytes <= 0 || c.Server.ShutdownGrace <= 0 {
		t.Errorf("request limits must have positive defaults: %+v", c.Server)
	}
}

// Unsafe or ambiguous values fail startup; none of them is silently ignored,
// defaulted or coerced into something that enables a disclosure.
func TestM52UnsafeConfigurationFailsClearly(t *testing.T) {
	backend := "    url: http://127.0.0.1:9099\n"
	withURL := func(u string) string { return strings.Replace(validConfig, backend, "    url: "+u+"\n", 1) }
	extra := func(section string) string { return validConfig + section }
	cases := map[string]string{
		// Backend URLs: only absolute http(s) without credentials, query or fragment.
		"file url":       withURL("file:///etc/passwd"),
		"ftp url":        withURL("ftp://127.0.0.1/"),
		"gopher url":     withURL("gopher://127.0.0.1:70/_x"),
		"unix url":       withURL("unix:///var/run/bgpd.sock"),
		"javascript url": withURL(`"javascript:alert(1)"`),
		"no scheme":      withURL("127.0.0.1:9099"),
		"scheme only":    withURL(`"http://"`),
		"credentials":    withURL("http://user:secret@127.0.0.1:9099"),
		"username":       withURL("http://user@127.0.0.1:9099"),
		"query":          withURL(`"http://127.0.0.1:9099/?x=1"`),
		"fragment":       withURL(`"http://127.0.0.1:9099/#x"`),
		"empty url":      withURL(`""`),
		// Strict decoding: a misspelled privacy switch must not silently mean "default".
		"misspelled raw json":       extra("display:\n  rawjson: false\n"),
		"misspelled rtr addresses":  extra("features:\n  rtr_session_address: false\n"),
		"unknown top-level section": extra("debug:\n  pprof: true\n"),
		"raw json not boolean":      extra("display:\n  raw_json: \"sometimes\"\n"),
		"rtr addresses not boolean": extra("features:\n  rtr_session_addresses: 2\n"),
		// Operator text and assets.
		"website javascript":   extra("site:\n  website: \"javascript:alert(1)\"\n"),
		"website data":         extra("site:\n  website: \"data:text/html,x\"\n"),
		"contact with header":  extra("site:\n  contact: \"a@example.net\\nBcc: x@example.net\"\n"),
		"logo with path":       extra("site:\n  branding:\n    directory: /srv/brand\n    logo: ../secret.png\n"),
		"logo absolute":        extra("site:\n  branding:\n    directory: /srv/brand\n    logo: /etc/passwd.png\n"),
		"favicon subdirectory": extra("site:\n  branding:\n    directory: /srv/brand\n    favicon: a/b.ico\n"),
		"logo without dir":     extra("site:\n  branding:\n    logo: logo.png\n"),
		"logo type":            extra("site:\n  branding:\n    directory: /srv/brand\n    logo: logo.html\n"),
		// Theme values are colors, never CSS.
		"theme css injection": extra("theme:\n  accent: \"red;} body{background:url(//evil.example)}\"\n"),
		"theme named color":   extra("theme:\n  accent: red\n"),
		// Visibility rules must be exact; a typo must not quietly expose a prefix.
		"visibility bad cidr":  extra("route_visibility:\n  exclude:\n    ipv4:\n      - prefix: 198.51.100.0\n        match: exact\n"),
		"visibility bad match": extra("route_visibility:\n  exclude:\n    ipv4:\n      - prefix: 198.51.100.0/24\n        match: longer\n"),
		"visibility family":    extra("route_visibility:\n  exclude:\n    ipv4:\n      - prefix: 2001:db8::/32\n        match: exact\n"),
		"visibility host bits": extra("route_visibility:\n  exclude:\n    ipv6:\n      - prefix: 2001:db8::1/32\n        match: exact\n"),
		// Enrichment and definitions.
		"asn persist relative":   extra("asn_enrichment:\n  enabled: true\n  cache:\n    persist: true\n    path: cache.json\n"),
		"asn persist traversal":  extra("asn_enrichment:\n  enabled: true\n  cache:\n    persist: true\n    path: /var/lib/hextet-lg/../../etc/x\n"),
		"asn unknown provider":   extra("asn_enrichment:\n  provider: \"evil.example\"\n"),
		"ptr unbounded workers":  extra("ptr_enrichment:\n  max_concurrent: 100000\n"),
		"ptr unbounded page":     extra("ptr_enrichment:\n  max_lookups_per_page: 100000\n"),
		"asn unbounded cache":    extra("asn_enrichment:\n  cache:\n    max_entries: 100000000\n"),
		"definitions no dir":     extra("community_definitions:\n  enabled: true\n"),
		"metrics on public port": extra("metrics:\n  enabled: true\n  listen: \"0.0.0.0:8080\"\n"),
		"metrics no host":        extra("metrics:\n  enabled: true\n  listen: \":9090\"\n"),
	}
	for name, doc := range cases {
		if _, err := Decode(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: error repeats a credential: %v", name, err)
		}
	}
	if _, err := Decode(strings.NewReader(validConfig)); err != nil {
		t.Fatalf("the base document must be valid for the cases above to mean anything: %v", err)
	}
}
