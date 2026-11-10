package config

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/HextetSystems/hextet-lg/internal/theme"
	"github.com/HextetSystems/hextet-lg/internal/visibility"
)

// Features holds optional switches. The three community flags control search
// availability only. The session flags control whole views and are
// independent of each other: bgp_neighbors governs the BGP session pages
// (/neighbors and /summary), rtr_sessions governs the RPKI-RTR page (/rtr).
type Features struct {
	Communities         *bool `yaml:"communities"`
	LargeCommunities    *bool `yaml:"large_communities"`
	ExtendedCommunities *bool `yaml:"extended_communities"`
	BGPNeighbors        *bool `yaml:"bgp_neighbors"`
	RTRSessions         *bool `yaml:"rtr_sessions"`
	// RTRSessionAddresses publishes RTR validator addresses, ports and local
	// addresses on /rtr. It is off by default: validators are usually internal
	// infrastructure, so sessions are identified by the operator's description
	// (or an ordinal) unless this is enabled.
	RTRSessionAddresses *bool `yaml:"rtr_session_addresses"`
}

func enabled(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}
func (f Features) CommunityEnabled() bool         { return enabled(f.Communities, true) }
func (f Features) LargeCommunityEnabled() bool    { return enabled(f.LargeCommunities, true) }
func (f Features) ExtendedCommunityEnabled() bool { return enabled(f.ExtendedCommunities, false) }
func (f Features) BGPNeighborsEnabled() bool      { return enabled(f.BGPNeighbors, true) }
func (f Features) RTRSessionsEnabled() bool       { return enabled(f.RTRSessions, true) }
func (f Features) RTRSessionAddressesEnabled() bool {
	return enabled(f.RTRSessionAddresses, false)
}

// CommunityDefinitions configures the optional, operator-supplied BGP
// community definitions (legacy NLNOG text format). They are disabled by
// default; when enabled the directory is required and is read once at startup.
type CommunityDefinitions struct {
	Enabled   bool   `yaml:"enabled"`
	Directory string `yaml:"directory"`
}

type ASNCache struct {
	PositiveTTL time.Duration `yaml:"positive_ttl"`
	NegativeTTL time.Duration `yaml:"negative_ttl"`
	MaxEntries  int           `yaml:"max_entries"`
	Persist     bool          `yaml:"persist"`
	Path        string        `yaml:"path"`
}
type ASNEnrichment struct {
	Enabled           bool          `yaml:"enabled"`
	Provider          string        `yaml:"provider"`
	Timeout           time.Duration `yaml:"timeout"`
	MaxConcurrent     int           `yaml:"max_concurrent"`
	MaxLookupsPerPage int           `yaml:"max_lookups_per_page"`
	Cache             ASNCache      `yaml:"cache"`
}

func ASNDefaults() ASNEnrichment {
	return ASNEnrichment{Provider: "team_cymru", Timeout: 2 * time.Second, MaxConcurrent: 4, MaxLookupsPerPage: 64, Cache: ASNCache{PositiveTTL: 168 * time.Hour, NegativeTTL: 24 * time.Hour, MaxEntries: 10000, Path: "/var/lib/hextet-lg/asn-cache.json"}}
}

type PTRCache struct {
	PositiveTTL time.Duration `yaml:"positive_ttl"`
	NegativeTTL time.Duration `yaml:"negative_ttl"`
	MaxEntries  int           `yaml:"max_entries"`
}
type PTREnrichment struct {
	Enabled           bool          `yaml:"enabled"`
	Timeout           time.Duration `yaml:"timeout"`
	MaxConcurrent     int           `yaml:"max_concurrent"`
	MaxLookupsPerPage int           `yaml:"max_lookups_per_page"`
	Cache             PTRCache      `yaml:"cache"`
}

func PTRDefaults() PTREnrichment {
	return PTREnrichment{Timeout: 2 * time.Second, MaxConcurrent: 4, MaxLookupsPerPage: 64, Cache: PTRCache{PositiveTTL: 720 * time.Hour, NegativeTTL: 24 * time.Hour, MaxEntries: 10000}}
}

type Config struct {
	PTREnrichment        PTREnrichment        `yaml:"ptr_enrichment"`
	ASNEnrichment        ASNEnrichment        `yaml:"asn_enrichment"`
	Features             Features             `yaml:"features"`
	CommunityDefinitions CommunityDefinitions `yaml:"community_definitions"`
	RouteVisibility      visibility.Config    `yaml:"route_visibility"`
	// Theme is the optional color configuration; the zero value selects the
	// built-in light and dark themes.
	Theme          theme.Config `yaml:"theme"`
	policy         visibility.Policy
	policyCompiled bool
	policySource   visibility.Config
	Server         Server       `yaml:"server"`
	LookingGlass   LookingGlass `yaml:"looking_glass"`
	Backends       []Backend    `yaml:"backends"`
	Logging        Logging      `yaml:"logging"`
	Metrics        Metrics      `yaml:"metrics"`
}

// Logging configures structured logging. Both settings are optional.
type Logging struct {
	// Level is debug, info, warn or error (default info).
	Level string `yaml:"level"`
	// Format is json or text (default json).
	Format string `yaml:"format"`
}

// Metrics configures the optional Prometheus listener. It is a separate
// listener, never a route on the public one, and is disabled by default.
type Metrics struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

// Default logging and metrics settings.
const (
	DefaultLogLevel      = "info"
	DefaultLogFormat     = "json"
	DefaultMetricsListen = "127.0.0.1:9090"
)

type Server struct {
	Listen            string        `yaml:"listen"`
	Title             string        `yaml:"title"`
	MaxURLBytes       int           `yaml:"max_url_bytes"`
	MaxQueryBytes     int           `yaml:"max_query_bytes"`
	MaxQueryParams    int           `yaml:"max_query_params"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`
	RatePerSecond     float64       `yaml:"rate_per_second"`
	RateBurst         int           `yaml:"rate_burst"`
	LiveRatePerSecond float64       `yaml:"live_rate_per_second"`
	LiveRateBurst     int           `yaml:"live_rate_burst"`
	ShutdownGrace     time.Duration `yaml:"shutdown_grace"`
}

// Default resource limits. They are initial operational defaults, not hard
// limits: every value can be changed in the looking_glass section.
const (
	DefaultQueryTimeout          = 30 * time.Second
	DefaultMaxResponseBytes      = 128 << 20
	DefaultMaxRetainedBytes      = 256 << 20
	DefaultPageSize              = 100
	DefaultMaxConcurrentQueries  = 2
	DefaultMaxConcurrentBackends = 4
	DefaultCacheTTL              = 20 * time.Second
	DefaultCacheMaxBytes         = 256 << 20
	maxPageSize                  = 1000
)

type LookingGlass struct {
	DefaultBackend        string        `yaml:"default_backend"`
	MaxTotalRetainedBytes int64         `yaml:"max_total_retained_bytes"`
	MaxConcurrentWork     int           `yaml:"max_concurrent_work"`
	MaxQueuedWork         int           `yaml:"max_queued_work"`
	QueueWait             time.Duration `yaml:"queue_wait"`
	MaxQueuedQueries      int           `yaml:"max_queued_queries"`
	// QueryTimeout bounds one backend query, including reading the whole
	// response body. Individual backends may override it.
	QueryTimeout time.Duration `yaml:"query_timeout"`
	// MaxResponseBytes bounds the upstream response body per backend query.
	MaxResponseBytes int64 `yaml:"max_response_bytes"`
	// MaxRetainedBytes bounds the estimated memory retained for decoded routes
	// of one backend query.
	MaxRetainedBytes int64 `yaml:"max_retained_bytes"`
	// PageSize is the number of routes per HTML page (and optional plaintext page).
	PageSize int `yaml:"page_size"`
	// MaxConcurrentQueries bounds simultaneous expensive upstream route queries
	// (source-AS and Large Community searches) across the whole instance.
	MaxConcurrentQueries int `yaml:"max_concurrent_queries"`
	// MaxConcurrentBackends bounds backend fan-out within one request.
	MaxConcurrentBackends int `yaml:"max_concurrent_backends"`
	// CacheTTL is the lifetime of cached route results; zero disables caching.
	CacheTTL time.Duration `yaml:"cache_ttl"`
	// CacheMaxBytes is the global memory budget of the result cache.
	CacheMaxBytes int64 `yaml:"cache_max_bytes"`
}

type Backend struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Location string `yaml:"location"`
	URL      string `yaml:"url"`
	Enabled  bool   `yaml:"enabled"`
	// QueryTimeout optionally overrides looking_glass.query_timeout.
	QueryTimeout time.Duration `yaml:"query_timeout"`
}

// Defaults returns the LookingGlass settings used when the YAML omits them.
func Defaults() LookingGlass {
	return LookingGlass{
		DefaultBackend:        "all",
		MaxTotalRetainedBytes: 768 << 20,
		MaxConcurrentWork:     8,
		MaxQueuedWork:         16,
		QueueWait:             2 * time.Second,
		MaxQueuedQueries:      16,
		QueryTimeout:          DefaultQueryTimeout,
		MaxResponseBytes:      DefaultMaxResponseBytes,
		MaxRetainedBytes:      DefaultMaxRetainedBytes,
		PageSize:              DefaultPageSize,
		MaxConcurrentQueries:  DefaultMaxConcurrentQueries,
		MaxConcurrentBackends: DefaultMaxConcurrentBackends,
		CacheTTL:              DefaultCacheTTL,
		CacheMaxBytes:         DefaultCacheMaxBytes,
	}
}

// FillZero replaces unset (zero) limits with defaults. CacheTTL is left alone
// because zero is a meaningful value (cache disabled). It makes programmatic
// configurations without the newer settings usable.
func (l *LookingGlass) FillZero() {
	d := Defaults()
	if l.MaxTotalRetainedBytes == 0 {
		l.MaxTotalRetainedBytes = d.MaxTotalRetainedBytes
	}
	if l.MaxConcurrentWork == 0 {
		l.MaxConcurrentWork = d.MaxConcurrentWork
	}
	if l.MaxQueuedWork == 0 {
		l.MaxQueuedWork = d.MaxQueuedWork
	}
	if l.QueueWait == 0 {
		l.QueueWait = d.QueueWait
	}
	if l.MaxQueuedQueries == 0 {
		l.MaxQueuedQueries = d.MaxQueuedQueries
	}
	if l.QueryTimeout == 0 {
		l.QueryTimeout = d.QueryTimeout
	}
	if l.MaxResponseBytes == 0 {
		l.MaxResponseBytes = d.MaxResponseBytes
	}
	if l.MaxRetainedBytes == 0 {
		l.MaxRetainedBytes = d.MaxRetainedBytes
	}
	if l.PageSize == 0 {
		l.PageSize = d.PageSize
	}
	if l.MaxConcurrentQueries == 0 {
		l.MaxConcurrentQueries = d.MaxConcurrentQueries
	}
	if l.MaxConcurrentBackends == 0 {
		l.MaxConcurrentBackends = d.MaxConcurrentBackends
	}
	if l.CacheMaxBytes == 0 {
		l.CacheMaxBytes = d.CacheMaxBytes
	}
}

// MemoryBudget estimates the heap the configured limits can legitimately need:
// every concurrent large query may retain MaxRetainedBytes of decoded routes and
// the cache may hold CacheMaxBytes, plus 25% estimate slack and 64 MiB for the
// rest of the process. The command uses it as the Go runtime's soft memory
// limit so garbage-collection headroom cannot double the footprint.
func (l LookingGlass) MemoryBudget() int64 {
	budget := int64(l.MaxConcurrentQueries) * l.MaxRetainedBytes
	if l.CacheTTL > 0 {
		budget += l.CacheMaxBytes
	}
	return budget + budget/4 + 64<<20
}

// TimeoutFor returns the effective query timeout for a backend.
func (c Config) TimeoutFor(b Backend) time.Duration {
	if b.QueryTimeout > 0 {
		return b.QueryTimeout
	}
	return c.LookingGlass.QueryTimeout
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()
	return Decode(f)
}

func Decode(r io.Reader) (Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	c := Config{PTREnrichment: PTRDefaults(), ASNEnrichment: ASNDefaults(), Logging: Logging{Level: DefaultLogLevel, Format: DefaultLogFormat}, Metrics: Metrics{Listen: DefaultMetricsListen}, LookingGlass: Defaults(), Server: Server{MaxURLBytes: 8192, MaxQueryBytes: 6144, MaxQueryParams: 64, MaxHeaderBytes: 32768, RatePerSecond: 10, RateBurst: 60, LiveRatePerSecond: 1, LiveRateBurst: 8, ShutdownGrace: 40 * time.Second}}
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("decode config: multiple YAML documents are not supported")
		}
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c *Config) Validate() error {
	p := c.PTREnrichment
	if p.Timeout <= 0 || p.Timeout > time.Minute || p.MaxConcurrent < 1 || p.MaxConcurrent > 64 || p.MaxLookupsPerPage < 1 || p.MaxLookupsPerPage > 256 || p.Cache.PositiveTTL <= 0 || p.Cache.NegativeTTL <= 0 || p.Cache.MaxEntries < 1 || p.Cache.MaxEntries > 10000 {
		return errors.New("invalid ptr_enrichment settings")
	}
	a := c.ASNEnrichment
	if a.Provider != "team_cymru" || a.Timeout <= 0 || a.Timeout > time.Minute || a.MaxConcurrent < 1 || a.MaxConcurrent > 64 || a.MaxLookupsPerPage < 1 || a.MaxLookupsPerPage > 256 || a.Cache.PositiveTTL <= 0 || a.Cache.NegativeTTL <= 0 || a.Cache.MaxEntries < 1 || a.Cache.MaxEntries > 10000 || (a.Cache.Persist && (!filepath.IsAbs(a.Cache.Path) || filepath.Clean(a.Cache.Path) != a.Cache.Path)) {
		return errors.New("invalid asn_enrichment settings")
	}
	if err := c.CompileVisibility(); err != nil {
		return err
	}
	if _, err := theme.Compile(c.Theme); err != nil {
		return err
	}
	if math.IsNaN(c.Server.RatePerSecond) || math.IsInf(c.Server.RatePerSecond, 0) || math.IsNaN(c.Server.LiveRatePerSecond) || math.IsInf(c.Server.LiveRatePerSecond, 0) || c.Server.MaxURLBytes <= 0 || c.Server.MaxQueryBytes <= 0 || c.Server.MaxQueryParams <= 0 || c.Server.MaxHeaderBytes <= 0 || c.Server.RatePerSecond <= 0 || c.Server.RateBurst <= 0 || c.Server.LiveRatePerSecond <= 0 || c.Server.LiveRateBurst <= 0 || c.Server.ShutdownGrace <= 0 || c.LookingGlass.QueueWait <= 0 || c.LookingGlass.MaxQueuedQueries <= 0 {
		return errors.New("server and queue limits must be positive")
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(c.Server.Listen))
	if err != nil || host == "" {
		return errors.New("server.listen must be a host:port address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("server.listen port must be between 1 and 65535")
	}
	if err := c.validateObservability(); err != nil {
		return err
	}
	if c.CommunityDefinitions.Enabled && strings.TrimSpace(c.CommunityDefinitions.Directory) == "" {
		return errors.New("community_definitions.directory is required when community_definitions.enabled is true")
	}
	if strings.TrimSpace(c.Server.Title) == "" {
		return errors.New("server.title is required")
	}
	if c.LookingGlass.QueryTimeout <= 0 {
		return errors.New("looking_glass.query_timeout must be positive")
	}
	if c.LookingGlass.MaxResponseBytes < 1024 {
		return errors.New("looking_glass.max_response_bytes must be at least 1024")
	}
	if c.LookingGlass.MaxTotalRetainedBytes < 1024 || c.LookingGlass.MaxConcurrentWork < 1 || c.LookingGlass.MaxQueuedWork < 1 {
		return errors.New("aggregate resource limits must be positive (memory at least 1024 bytes)")
	}
	if c.LookingGlass.MaxRetainedBytes < 1024 {
		return errors.New("looking_glass.max_retained_bytes must be at least 1024")
	}
	if c.LookingGlass.PageSize < 1 || c.LookingGlass.PageSize > maxPageSize {
		return fmt.Errorf("looking_glass.page_size must be between 1 and %d", maxPageSize)
	}
	if c.LookingGlass.MaxConcurrentQueries < 1 {
		return errors.New("looking_glass.max_concurrent_queries must be at least 1")
	}
	if c.LookingGlass.MaxConcurrentBackends < 1 {
		return errors.New("looking_glass.max_concurrent_backends must be at least 1")
	}
	if c.LookingGlass.CacheTTL < 0 {
		return errors.New("looking_glass.cache_ttl must not be negative (0 disables the cache)")
	}
	if c.LookingGlass.CacheTTL > 0 && c.LookingGlass.CacheMaxBytes < 1024 {
		return errors.New("looking_glass.cache_max_bytes must be at least 1024 when the cache is enabled")
	}
	if len(c.Backends) == 0 {
		return errors.New("at least one backend must be configured")
	}
	ids := map[string]bool{}
	enabled := false
	for i, b := range c.Backends {
		p := fmt.Sprintf("backends[%d]", i)
		if strings.TrimSpace(b.ID) == "" || strings.TrimSpace(b.Name) == "" {
			return fmt.Errorf("%s requires id and name", p)
		}
		if strings.ContainsAny(b.ID, " /?#") {
			return fmt.Errorf("%s.id must be a URL-safe stable identifier", p)
		}
		if ids[b.ID] {
			return fmt.Errorf("duplicate backend id %q", b.ID)
		}
		ids[b.ID] = true
		if b.QueryTimeout < 0 {
			return fmt.Errorf("%s.query_timeout must not be negative", p)
		}
		u, err := url.Parse(b.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%s.url must be an absolute http or https URL", p)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%s.url must not include credentials, query, or fragment", p)
		}
		if b.Enabled {
			enabled = true
		}
	}
	if !enabled {
		return errors.New("at least one backend must be enabled")
	}
	if c.LookingGlass.DefaultBackend != "all" && !ids[c.LookingGlass.DefaultBackend] {
		return fmt.Errorf("looking_glass.default_backend %q does not match a configured backend id or 'all'", c.LookingGlass.DefaultBackend)
	}
	if c.LookingGlass.DefaultBackend != "all" {
		for _, b := range c.Backends {
			if b.ID == c.LookingGlass.DefaultBackend && !b.Enabled {
				return fmt.Errorf("looking_glass.default_backend %q is disabled", b.ID)
			}
		}
	}
	return nil
}

// CompileVisibility validates and freezes the policy for this configuration.
func (c *Config) CompileVisibility() error {
	if c.policyCompiled && reflect.DeepEqual(c.policySource, c.RouteVisibility) {
		return nil
	}
	policy, err := visibility.Compile(c.RouteVisibility)
	if err != nil {
		return err
	}
	c.policy, c.policyCompiled = policy, true
	c.policySource = c.RouteVisibility
	c.policySource.Exclude.IPv4 = slices.Clone(c.RouteVisibility.Exclude.IPv4)
	c.policySource.Exclude.IPv6 = slices.Clone(c.RouteVisibility.Exclude.IPv6)
	return nil
}

// VisibilityPolicy returns the immutable compiled policy.
func (c Config) VisibilityPolicy() visibility.Policy { return c.policy }

func (c Config) validateObservability() error {
	switch strings.ToLower(c.Logging.Level) {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level %q must be debug, info, warn or error", c.Logging.Level)
	}
	switch strings.ToLower(c.Logging.Format) {
	case "", "json", "text":
	default:
		return fmt.Errorf("logging.format %q must be json or text", c.Logging.Format)
	}
	if !c.Metrics.Enabled {
		return nil
	}
	mh, mp, err := net.SplitHostPort(strings.TrimSpace(c.Metrics.Listen))
	if err != nil || mh == "" {
		return errors.New("metrics.listen must be a host:port address with an explicit host (default 127.0.0.1:9090)")
	}
	if n, err := strconv.Atoi(mp); err != nil || n < 1 || n > 65535 {
		return errors.New("metrics.listen port must be between 1 and 65535")
	}
	if sh, sp, err := net.SplitHostPort(strings.TrimSpace(c.Server.Listen)); err == nil && sp == mp {
		wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
		if sh == mh || wild(sh) || wild(mh) {
			return errors.New("metrics.listen must not share a port with server.listen: /metrics must never be served by the public listener")
		}
	}
	return nil
}

// LoopbackOnly reports whether the metrics listener is bound to a
// loopback address (or "localhost"). Other addresses are legal, for container
// monitoring, but the caller should warn: reachability is then the operator's
// network policy, not the application's.
func (m Metrics) LoopbackOnly() bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(m.Listen))
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
