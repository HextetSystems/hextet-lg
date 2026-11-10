package config

import (
	"strings"
	"testing"
	"time"
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
