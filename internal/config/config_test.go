package config

import (
	"strings"
	"testing"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/theme"
)

const validConfig = `server:
  listen: "127.0.0.1:8080"
  title: "Test LG"
looking_glass:
  default_backend: all
  query_timeout: 2s
  max_response_bytes: 1048576
  max_concurrent_backends: 2
backends:
  - id: one
    name: One
    url: http://127.0.0.1:9099
    enabled: true
`

func TestDecodeValidConfig(t *testing.T) {
	c, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Backends) != 1 || c.LookingGlass.QueryTimeout.String() != "2s" {
		t.Fatalf("unexpected config: %#v", c)
	}
}

func TestDecodeRejectsInvalidConfig(t *testing.T) {
	for name, raw := range map[string]string{
		"missing backend":        strings.Replace(validConfig, "    url: http://127.0.0.1:9099\n    enabled: true\n", "    url: relative\n    enabled: true\n", 1),
		"unknown field":          validConfig + "unexpected: true\n",
		"invalid listen address": strings.Replace(validConfig, "127.0.0.1:8080", "localhost", 1),
		"duplicate id":           strings.Replace(validConfig, "  - id: one\n    name: One\n    url: http://127.0.0.1:9099\n    enabled: true\n", "  - id: one\n    name: One\n    url: http://127.0.0.1:9099\n    enabled: true\n  - id: one\n    name: Two\n    url: http://127.0.0.1:9100\n    enabled: true\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(raw)); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestDefaultsApplyWhenSettingsOmitted(t *testing.T) {
	c, err := Decode(strings.NewReader(`server:
  listen: "127.0.0.1:8080"
  title: "T"
backends:
  - id: one
    name: One
    url: http://127.0.0.1:9099
    enabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	lg := c.LookingGlass
	if lg.DefaultBackend != "all" || lg.QueryTimeout != 30*time.Second || lg.MaxResponseBytes != 128<<20 || lg.PageSize != 100 || lg.MaxConcurrentQueries != 2 || lg.CacheTTL != 20*time.Second || lg.MaxRetainedBytes != 256<<20 || lg.CacheMaxBytes != 256<<20 {
		t.Fatalf("%+v", lg)
	}
}

// An existing config.yaml that predates the new settings keeps its explicit
// values and receives defaults only for the missing ones.
func TestExistingConfigStaysCompatible(t *testing.T) {
	c, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	lg := c.LookingGlass
	if lg.QueryTimeout != 2*time.Second || lg.MaxResponseBytes != 1048576 || lg.MaxConcurrentBackends != 2 || lg.PageSize != 100 || lg.MaxConcurrentQueries != 2 {
		t.Fatalf("%+v", lg)
	}
}

func TestPerBackendTimeoutOverride(t *testing.T) {
	c, err := Decode(strings.NewReader(validConfig + "  - id: slow\n    name: Slow\n    url: http://127.0.0.1:9100\n    enabled: true\n    query_timeout: 90s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TimeoutFor(c.Backends[0]) != 2*time.Second || c.TimeoutFor(c.Backends[1]) != 90*time.Second {
		t.Fatalf("%v %v", c.TimeoutFor(c.Backends[0]), c.TimeoutFor(c.Backends[1]))
	}
}

func TestNewSettingsValidation(t *testing.T) {
	for name, tc := range map[string]struct{ edit, want string }{
		"page size zero":   {"  page_size: 0\n", "page_size"},
		"page size huge":   {"  page_size: 100000\n", "page_size"},
		"no query slots":   {"  max_concurrent_queries: 0\n", "max_concurrent_queries"},
		"tiny retained":    {"  max_retained_bytes: 10\n", "max_retained_bytes"},
		"negative cache":   {"  cache_ttl: -1s\n", "cache_ttl"},
		"cache no budget":  {"  cache_ttl: 5s\n  cache_max_bytes: 10\n", "cache_max_bytes"},
		"negative timeout": {"  query_timeout: -5s\n", "query_timeout"},
		"unknown":          {"  max_response_mb: 5\n", "max_response_mb"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(validConfig, "\nbackends:", "\n"+strings.TrimSuffix(tc.edit, "\n")+"\nbackends:", 1)
			_, err := Decode(strings.NewReader(raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}
	raw := strings.Replace(validConfig, "    enabled: true\n", "    enabled: true\n    query_timeout: -1s\n", 1)
	if _, err := Decode(strings.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "query_timeout") {
		t.Fatalf("negative backend timeout: %v", err)
	}
	raw = strings.Replace(validConfig, "\nbackends:", "\n  cache_ttl: 0s\nbackends:", 1)
	if c, err := Decode(strings.NewReader(raw)); err != nil || c.LookingGlass.CacheTTL != 0 {
		t.Fatalf("cache must be disableable: %v", err)
	}
}

func TestMemoryBudget(t *testing.T) {
	lg := Defaults()
	// 2 x 256 MiB retained + 256 MiB cache = 768 MiB; +25% and +64 MiB.
	if got, want := lg.MemoryBudget(), int64(768<<20+192<<20+64<<20); got != want {
		t.Fatalf("%d != %d", got, want)
	}
	lg.CacheTTL = 0
	if got, want := lg.MemoryBudget(), int64(512<<20+128<<20+64<<20); got != want {
		t.Fatalf("cache disabled: %d != %d", got, want)
	}
}

func TestObservabilityDefaultsPreserveOldConfigs(t *testing.T) {
	c, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Logging.Level != "info" || c.Logging.Format != "json" || c.Metrics.Enabled || c.Metrics.Listen != "127.0.0.1:9090" {
		t.Fatalf("defaults: %+v %+v", c.Logging, c.Metrics)
	}
}

func TestObservabilityValidation(t *testing.T) {
	with := func(extra string) string { return validConfig + extra }
	for name, tc := range map[string]struct {
		raw string
		ok  bool
	}{
		"debug json":               {with("logging:\n  level: debug\n  format: json\n"), true},
		"warn text":                {with("logging:\n  level: warn\n  format: text\n"), true},
		"error":                    {with("logging:\n  level: error\n"), true},
		"bad level":                {with("logging:\n  level: verbose\n"), false},
		"bad format":               {with("logging:\n  format: xml\n"), false},
		"unknown logging field":    {with("logging:\n  file: /tmp/x\n"), false},
		"metrics loopback":         {with("metrics:\n  enabled: true\n  listen: \"127.0.0.1:9090\"\n"), true},
		"metrics ipv6 loopback":    {with("metrics:\n  enabled: true\n  listen: \"[::1]:9090\"\n"), true},
		"metrics default listen":   {with("metrics:\n  enabled: true\n"), true},
		"metrics management addr":  {with("metrics:\n  enabled: true\n  listen: \"10.0.0.5:9090\"\n"), true},
		"metrics no host":          {with("metrics:\n  enabled: true\n  listen: \":9090\"\n"), false},
		"metrics bad port":         {with("metrics:\n  enabled: true\n  listen: \"127.0.0.1:0\"\n"), false},
		"metrics same port":        {with("metrics:\n  enabled: true\n  listen: \"127.0.0.1:8080\"\n"), false},
		"metrics wildcard clash":   {with("metrics:\n  enabled: true\n  listen: \"0.0.0.0:8080\"\n"), false},
		"disabled metrics ignored": {with("metrics:\n  enabled: false\n  listen: \"bogus\"\n"), true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(tc.raw))
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v, err=%v", tc.ok, err)
			}
		})
	}
}

func TestMetricsLoopbackOnly(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:9090": true, "[::1]:9090": true, "localhost:9090": true, "0.0.0.0:9090": false, "10.0.0.5:9090": false, "[::]:9090": false} {
		if got := (Metrics{Listen: addr}).LoopbackOnly(); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
}

func TestASNEnrichmentDefaultsAndValidation(t *testing.T) {
	c, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.ASNEnrichment.Enabled || c.ASNEnrichment.Provider != "team_cymru" || c.ASNEnrichment.MaxLookupsPerPage != 64 || c.ASNEnrichment.Cache.PositiveTTL != 168*time.Hour {
		t.Fatalf("unexpected defaults: %+v", c.ASNEnrichment)
	}
	for _, mutate := range []func(*ASNEnrichment){func(a *ASNEnrichment) { a.Provider = "other" }, func(a *ASNEnrichment) { a.Timeout = 0 }, func(a *ASNEnrichment) { a.MaxConcurrent = 0 }, func(a *ASNEnrichment) { a.MaxLookupsPerPage = 257 }, func(a *ASNEnrichment) { a.Cache.MaxEntries = 0 }, func(a *ASNEnrichment) { a.Cache.Persist = true; a.Cache.Path = "relative" }} {
		bad := c
		mutate(&bad.ASNEnrichment)
		if bad.Validate() == nil {
			t.Fatal("invalid ASN settings accepted")
		}
	}
}

func TestThemeSectionIsOptionalAndPartial(t *testing.T) {
	c, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatalf("a configuration without a theme section must keep working: %v", err)
	}
	if c.Theme.Default != "" || c.Theme.Accent != "" {
		t.Fatalf("omitted theme must stay zero so built-in defaults apply: %#v", c.Theme)
	}
	for name, section := range map[string]string{
		"accent only":      "theme:\n  accent: \"#0D9488\"\n",
		"default only":     "theme:\n  default: dark\n",
		"one palette key":  "theme:\n  light:\n    border: \"#CCCCCC\"\n",
		"full light":       "theme:\n  light:\n    background: \"#F7F8FA\"\n    surface: \"#FFFFFF\"\n    surface_alt: \"#F0F2F5\"\n    surface_hover: \"#E8ECF1\"\n    border: \"#D8DDE3\"\n    text: \"#252B32\"\n    text_muted: \"#626A76\"\n",
		"empty section":    "theme:\n",
		"lowercase colors": "theme:\n  accent: \"#0d9488\"\n  dark:\n    text: \"#ffffff\"\n",
	} {
		if _, err := Decode(strings.NewReader(validConfig + section)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestThemeSectionRejectsInvalidValues(t *testing.T) {
	for name, section := range map[string]string{
		"unknown key":      "theme:\n  accents: \"#0D9488\"\n",
		"unknown palette":  "theme:\n  light:\n    foreground: \"#000000\"\n",
		"bad mode":         "theme:\n  default: sepia\n",
		"color name":       "theme:\n  accent: teal\n",
		"short hex":        "theme:\n  accent: \"#fff\"\n",
		"css function":     "theme:\n  dark:\n    surface: \"url(https://example.net/x)\"\n",
		"css variable":     "theme:\n  light:\n    text: \"var(--color-surface)\"\n",
		"injection":        "theme:\n  accent: \"#FFFFFF;} body{display:none\"\n",
		"unreadable text":  "theme:\n  dark:\n    text: \"#1B1E23\"\n",
		"not a scalar":     "theme:\n  accent: [\"#0D9488\"]\n",
		"unknown dark key": "theme:\n  dark:\n    accent: \"#0D9488\"\n",
	} {
		if _, err := Decode(strings.NewReader(validConfig + section)); err == nil {
			t.Errorf("%s: expected a startup error", name)
		}
	}
}

// The documented example must stay valid and must spell out exactly the
// built-in themes, so the example cannot drift from the real defaults.
func TestExampleThemeMatchesBuiltInDefaults(t *testing.T) {
	c, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	example, err := theme.Compile(c.Theme)
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := theme.Compile(theme.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if string(example.CSS) != string(builtin.CSS) || len(example.Warnings) != 0 {
		t.Fatalf("example theme differs from the built-in defaults (warnings %v)", example.Warnings)
	}
}

func TestPTRDefaultsAndValidation(t *testing.T) {
	c, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.PTREnrichment.Enabled || c.PTREnrichment.Cache.PositiveTTL != 720*time.Hour || c.PTREnrichment.Cache.NegativeTTL != 24*time.Hour || c.PTREnrichment.MaxConcurrent != 4 || c.PTREnrichment.MaxLookupsPerPage != 64 || c.PTREnrichment.Cache.MaxEntries != 10000 || c.PTREnrichment.Timeout != 2*time.Second {
		t.Fatalf("defaults: %+v", c.PTREnrichment)
	}
	for _, mutate := range []func(*PTREnrichment){func(p *PTREnrichment) { p.Timeout = 0 }, func(p *PTREnrichment) { p.Timeout = time.Minute + 1 }, func(p *PTREnrichment) { p.MaxConcurrent = 0 }, func(p *PTREnrichment) { p.MaxLookupsPerPage = 257 }, func(p *PTREnrichment) { p.Cache.PositiveTTL = 0 }, func(p *PTREnrichment) { p.Cache.NegativeTTL = 0 }, func(p *PTREnrichment) { p.Cache.MaxEntries = 10001 }} {
		bad := c
		mutate(&bad.PTREnrichment)
		if bad.Validate() == nil {
			t.Fatal("invalid PTR settings accepted")
		}
	}
}

func TestSessionFeatureFlags(t *testing.T) {
	with := func(features string) string {
		return strings.Replace(validConfig, "\nbackends:", "\n"+features+"backends:", 1)
	}
	for name, tc := range map[string]struct {
		yaml               string
		bgp, rtr, rtrAddrs bool
	}{
		"features omitted entirely":  {validConfig, true, true, false},
		"section without the flags":  {with("features:\n  communities: false\n"), true, true, false},
		"both enabled":               {with("features:\n  bgp_neighbors: true\n  rtr_sessions: true\n"), true, true, false},
		"both disabled":              {with("features:\n  bgp_neighbors: false\n  rtr_sessions: false\n"), false, false, false},
		"only bgp enabled":           {with("features:\n  bgp_neighbors: true\n  rtr_sessions: false\n"), true, false, false},
		"only rtr enabled":           {with("features:\n  bgp_neighbors: false\n  rtr_sessions: true\n"), false, true, false},
		"rtr disabled, bgp omitted":  {with("features:\n  rtr_sessions: false\n"), true, false, false},
		"bgp disabled, rtr omitted":  {with("features:\n  bgp_neighbors: false\n"), false, true, false},
		"rtr addresses opted in":     {with("features:\n  rtr_session_addresses: true\n"), true, true, true},
		"rtr addresses without rtr":  {with("features:\n  rtr_sessions: false\n  rtr_session_addresses: true\n"), true, false, true},
		"community flags unaffected": {with("features:\n  communities: false\n  large_communities: false\n  extended_communities: true\n  bgp_neighbors: false\n"), false, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Decode(strings.NewReader(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			f := c.Features
			if f.BGPNeighborsEnabled() != tc.bgp || f.RTRSessionsEnabled() != tc.rtr || f.RTRSessionAddressesEnabled() != tc.rtrAddrs {
				t.Fatalf("bgp=%v rtr=%v addrs=%v", f.BGPNeighborsEnabled(), f.RTRSessionsEnabled(), f.RTRSessionAddressesEnabled())
			}
		})
	}
	// The community flags keep their own defaults.
	c, _ := Decode(strings.NewReader(validConfig))
	if !c.Features.CommunityEnabled() || !c.Features.LargeCommunityEnabled() || c.Features.ExtendedCommunityEnabled() {
		t.Fatal("community defaults changed")
	}
}

func TestSessionFeatureFlagsAreStrictlyTyped(t *testing.T) {
	for name, yaml := range map[string]string{
		"string value": "features:\n  rtr_sessions: maybe\n",
		"typo":         "features:\n  rtr_session: true\n",
		"list":         "features:\n  bgp_neighbors: [true]\n",
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(validConfig, "\nbackends:", "\n"+yaml+"backends:", 1)
			if _, err := Decode(strings.NewReader(raw)); err == nil {
				t.Fatal("expected a decode error")
			}
		})
	}
}

func TestPageTimingFlag(t *testing.T) {
	with := func(features string) string {
		return strings.Replace(validConfig, "\nbackends:", "\n"+features+"backends:", 1)
	}
	for name, tc := range map[string]struct {
		yaml string
		want bool
	}{
		"features omitted":  {validConfig, true},
		"flag omitted":      {with("features:\n  communities: false\n"), true},
		"explicitly on":     {with("features:\n  page_timing: true\n"), true},
		"explicitly off":    {with("features:\n  page_timing: false\n"), false},
		"independent flags": {with("features:\n  page_timing: false\n  bgp_neighbors: true\n  rtr_sessions: true\n"), false},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Decode(strings.NewReader(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.Features.PageTimingEnabled(); got != tc.want {
				t.Fatalf("PageTimingEnabled = %v, want %v", got, tc.want)
			}
		})
	}
	for name, yaml := range map[string]string{
		"string value": "features:\n  page_timing: maybe\n",
		"typo":         "features:\n  page_timings: true\n",
		"list":         "features:\n  page_timing: [true]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(strings.Replace(validConfig, "\nbackends:", "\n"+yaml+"backends:", 1))); err == nil {
				t.Fatal("expected a decode error")
			}
		})
	}
	c, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Features.PageTiming == nil || !c.Features.PageTimingEnabled() {
		t.Fatal("the example must show page_timing at its default (true)")
	}
}

func TestExampleConfigDocumentsSessionFlags(t *testing.T) {
	c, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f := c.Features
	if f.BGPNeighbors == nil || f.RTRSessions == nil || f.RTRSessionAddresses == nil {
		t.Fatal("the example must show every session flag")
	}
	if !f.BGPNeighborsEnabled() || !f.RTRSessionsEnabled() || f.RTRSessionAddressesEnabled() {
		t.Fatal("the example must show the defaults")
	}
}
