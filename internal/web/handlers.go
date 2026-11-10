package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/asn"
	"github.com/HextetSystems/hextet-lg/internal/bgp"
	"github.com/HextetSystems/hextet-lg/internal/communities"
	"github.com/HextetSystems/hextet-lg/internal/config"
	"github.com/HextetSystems/hextet-lg/internal/ptr"
	"github.com/HextetSystems/hextet-lg/internal/theme"
)

type backendClient struct {
	config.Backend
	client *bgp.Client
}

type Handler struct {
	cfg         config.Config
	asn         *asn.Service
	asnResolver asn.Resolver
	ptr         *ptr.Service
	ptrResolver ptr.Resolver
	backends    []backendClient
	templates   *template.Template
	// defs holds the operator-supplied community definitions, compiled once at
	// startup and immutable afterwards. It is nil when they are disabled.
	defs   *communities.Set
	cache  *resultCache
	budget *resultBudget
	work   *workGate
	// expensive holds the instance-wide slots for large upstream queries.
	expensive chan struct{}
	queue     chan struct{}
	limiter   *rateLimiter
	stopping  atomic.Bool
	stopCh    chan struct{}
	stopOnce  sync.Once

	theme      *theme.Theme
	assetVer   map[string]string
	log        *slog.Logger
	metrics    *metrics
	health     map[string]*backendHealth // fixed at construction: one per enabled backend
	activeHTTP atomic.Int64
}

// Option customizes NewHandler.
type Option func(*Handler)

// WithLogger sets the logger. Without it the handler logs to stderr using the
// configured logging.level and logging.format.
func WithLogger(l *slog.Logger) Option { return func(h *Handler) { h.log = l } }

func WithASNResolver(r asn.Resolver) Option { return func(h *Handler) { h.asnResolver = r } }

func WithPTRResolver(r ptr.Resolver) Option { return func(h *Handler) { h.ptrResolver = r } }

// Form is the search form state: the simple box plus the advanced filters.
type Form struct {
	Type, Q      string
	Values       map[string]string
	Flags        map[string]bool
	AdvancedOpen bool
	ActiveCount  int
}

// Page is the template model for every page.
type Page struct {
	Title, Query, SearchType, SelectedBackend string
	Backends                                  []config.Backend
	Results                                   []BackendResult
	Error                                     string
	Problems                                  []string
	IsHTMX                                    bool
	Active, Category, View                    string
	Communities                               []CommunityRow
	Categories                                []string
	// CommunityDefs reports whether definitions are enabled; CommunitySource
	// is the operator's note on where optional status metadata comes from.
	CommunityDefs   bool
	CommunitySource string
	// CommunityMatched is the number of definitions matching the Explorer
	// filters; Communities is capped at maxExplorerRows.
	CommunityMatched int

	Form                  Form
	Searched              bool
	Filters               []bgp.Filter
	Upstream              string // bgplgd path and query (never the backend address)
	Summary               Summary
	Sections              []Section
	Pager                 Pager
	Text                  string
	PlainURL              string
	CacheTTL              time.Duration
	LargeCommunityEnabled bool
	Groups                []FormGroup
}

func NewHandler(cfg config.Config, opts ...Option) (*Handler, error) {
	cfg.LookingGlass.FillZero()
	if err := cfg.CompileVisibility(); err != nil {
		return nil, err
	}
	var err error
	h := &Handler{cfg: cfg, stopCh: make(chan struct{}), health: map[string]*backendHealth{}}
	for _, o := range opts {
		o(h)
	}
	if h.log == nil {
		h.log = h.defaultLogger()
	}
	// The stylesheet is generated once from the validated configuration.
	if h.theme, err = theme.Compile(cfg.Theme); err != nil {
		return nil, err
	}
	for _, w := range h.theme.Warnings {
		h.log.Warn("theme contrast below target; colors are used as configured", "detail", w)
	}
	h.assetVer = assetVersions()
	// Definitions are read once, before anything else is started, so a bad
	// configuration fails startup cleanly.
	if err := h.loadCommunityDefinitions(); err != nil {
		return nil, err
	}
	for _, b := range cfg.Backends {
		if !b.Enabled {
			continue
		}
		c, err := bgp.NewClient(b.URL, cfg.TimeoutFor(b), cfg.LookingGlass.MaxResponseBytes)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", b.ID, err)
		}
		c.Visibility = cfg.VisibilityPolicy()
		c.MaxRetainedBytes = cfg.LookingGlass.MaxRetainedBytes
		h.backends = append(h.backends, backendClient{Backend: b, client: c})
		h.health[b.ID] = &backendHealth{}
	}
	ids := make([]string, len(h.backends))
	for i, b := range h.backends {
		ids[i] = b.ID
	}
	if cfg.ASNEnrichment.Enabled {
		a := cfg.ASNEnrichment
		h.asn = asn.New(asn.Config{Timeout: a.Timeout, PositiveTTL: a.Cache.PositiveTTL, NegativeTTL: a.Cache.NegativeTTL, MaxConcurrent: a.MaxConcurrent, MaxEntries: a.Cache.MaxEntries, Persist: a.Cache.Persist, Path: a.Cache.Path}, h.asnResolver)
	}
	if cfg.PTREnrichment.Enabled {
		p := cfg.PTREnrichment
		h.ptr = ptr.New(ptr.Config{Timeout: p.Timeout, PositiveTTL: p.Cache.PositiveTTL, NegativeTTL: p.Cache.NegativeTTL, MaxConcurrent: p.MaxConcurrent, MaxEntries: p.Cache.MaxEntries}, h.ptrResolver)
	}
	h.metrics = newMetrics(ids)
	h.budget = &resultBudget{limit: cfg.LookingGlass.MaxTotalRetainedBytes}
	h.work = &workGate{active: make(chan struct{}, cfg.LookingGlass.MaxConcurrentWork), queue: make(chan struct{}, cfg.LookingGlass.MaxQueuedWork), wait: cfg.LookingGlass.QueueWait, stop: h.stopCh}
	h.cache = newResultCache(cfg.LookingGlass.CacheTTL, cfg.LookingGlass.CacheMaxBytes)
	h.expensive = make(chan struct{}, cfg.LookingGlass.MaxConcurrentQueries)
	h.queue = make(chan struct{}, cfg.LookingGlass.MaxQueuedQueries)
	h.limiter = newRateLimiter(cfg.Server)
	h.templates, err = template.New("root").Funcs(template.FuncMap{
		"themeDefault":  func() string { return h.theme.Default },
		"assetURL":      h.assetURL,
		"asnTitle":      h.asnTitle,
		"ptrKey":        h.ptrKey,
		"ptrTitle":      h.ptrTitle,
		"asnEnabled":    func(s string) bool { return h.asn != nil && parseASN(s) != 0 },
		"asnPath":       h.pathParts,
		"featureBGP":    h.cfg.Features.BGPNeighborsEnabled,
		"featureRTR":    h.cfg.Features.RTRSessionsEnabled,
		"neighborURL":   neighborURL,
		"neighborTitle": neighborTitle,
		"searchURL":     searchURL,
		"age":           FormatAge,
		"formatRoutes":  formatRoutes,
		"human": func(s string) string {
			if s == "" {
				return "—"
			}
			return s
		},
		"number": func(n *int64) string {
			if n == nil {
				return "—"
			}
			return fmt.Sprint(*n)
		},
		"validity": func(v *bool) string {
			if v == nil {
				return "Unknown"
			}
			if *v {
				return "Valid"
			}
			return "Invalid"
		},
		"selected": selected,
		"checked":  checked,
		"commas":   commas,
		"bytes":    bgp.FormatBytes,
		"seconds":  seconds,
		"round":    func(d time.Duration) string { return d.Round(time.Second).String() },
		"join":     strings.Join,
		"rawJSON": func(s string) string {
			var b bytes.Buffer
			if err := json.Indent(&b, []byte(s), "", "  "); err != nil {
				return s
			}
			return b.String()
		},
		"statusClass": func(s bgp.Status) string {
			switch s {
			case bgp.StatusComplete:
				return "good"
			case bgp.StatusPartial:
				return "warn"
			}
			return "bad"
		},
		"statusClassName": func(s string) string {
			switch s {
			case communities.StatusImplemented:
				return "good"
			case communities.StatusDocumented:
				return "warn"
			}
			return "muted"
		},
		// communityTitle is the tooltip text for a displayed community, or "".
		// It is looked up at render time from the immutable definition set so
		// retained route data never carries descriptions.
		"communityTitle": func(kind, value string) string {
			k, ok := communities.ParseKind(kind)
			if !ok {
				return ""
			}
			d, _ := h.defs.Lookup(k, value)
			return d
		},
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse embedded templates: %w", err)
	}
	// Registered last: the collector describes itself by collecting once, which
	// needs the budget, work gate and cache to exist.
	h.metrics.reg.MustRegister(resourceCollector{h})
	go h.limiter.cleanupLoop(h.stopCh)
	return h, nil
}

// searchURL is the canonical legacy form: type, q, backend (view is left to
// the target page).
func searchURL(kind, value, backend string) string {
	return "/?type=" + url.QueryEscape(kind) + "&q=" + url.QueryEscape(value) + "&backend=" + url.QueryEscape(backend)
}

func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.index)
	// Disabled session views are not registered at all: their URLs answer 404
	// and no navigation links to them. The two flags are independent.
	if h.cfg.Features.BGPNeighborsEnabled() {
		mux.HandleFunc("GET /neighbors", h.overview)
		mux.HandleFunc("GET /summary", h.overview)
	}
	if h.cfg.Features.RTRSessionsEnabled() {
		mux.HandleFunc("GET /rtr", h.overview)
	}
	mux.HandleFunc("GET /communities", h.communityPage)
	mux.HandleFunc("GET /asn-status", h.asnStatus)
	mux.HandleFunc("GET /ptr-status", h.ptrStatus)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /static/theme.css", h.themeCSS)
	mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if h.stopping.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	return h.protect(mux)
}

func (h *Handler) backendList() []config.Backend {
	var out []config.Backend
	for _, b := range h.backends {
		out = append(out, b.Backend)
	}
	return out
}

// formFor builds the form state for a parsed query.
func formFor(q bgp.Query, raw url.Values) Form {
	f := Form{Values: map[string]string{}, Flags: map[string]bool{}}
	if t, v, ok := q.Simple(); ok {
		f.Type, f.Q = string(t), v
	} else {
		f.Type = string(bgp.SearchPrefix)
		for _, k := range bgp.StringFilterKeys {
			f.Values[k] = q.Get(k)
		}
		for _, k := range bgp.FlagFilterKeys {
			f.Flags[k] = q.Get(k) != ""
		}

	}
	if q.Prefix != "" && len(q.Filters()) == 2 && (q.All || q.OrShorter) {
		f.Type, f.Q = string(bgp.SearchPrefix), q.Prefix
		f.Values[bgp.KeyPrefix] = ""
	}
	if raw.Get("q") != "" {
		f.Type, f.Q = raw.Get("type"), raw.Get("q")
		key := map[string]string{"prefix": bgp.KeyPrefix, "asn": bgp.KeyAS, "community": bgp.KeyCommunity, "ext-community": bgp.KeyExtCommunity, "large-community": bgp.KeyLargeCommunity}[f.Type]
		if key != "" {
			f.Values[key] = raw.Get(key)
		}
	}
	for _, v := range f.Values {
		if v != "" {
			f.ActiveCount++
		}
	}
	for _, v := range f.Flags {
		if v {
			f.ActiveCount++
		}
	}
	if f.Type == "" {
		f.Type = string(bgp.SearchPrefix)
	}
	return f
}

// knownParams are the query parameters the Route Explorer understands. Anything
// else is rejected: an unrecognized name is most likely a typo for a filter,
// and silently ignoring it would run a broader query than intended.
var knownParams = func() map[string]bool {
	m := map[string]bool{"type": true, "q": true, "backend": true, "view": true, "page": true, "output": true, bgp.KeyEndpoint: true}
	for _, k := range bgp.StringFilterKeys {
		m[k] = true
	}
	for _, k := range bgp.FlagFilterKeys {
		m[k] = true
	}
	return m
}()

func unknownParams(v url.Values) []string {
	var out []string
	for k := range v {
		if !knownParams[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func hasEmptyParam(v url.Values) bool {
	for _, vals := range v {
		for _, s := range vals {
			if strings.TrimSpace(s) == "" {
				return true
			}
		}
	}
	return false
}

func wantsPlain(r *http.Request, view string) bool {
	return view != "table" && (r.URL.Query().Get("output") == "plain" || strings.Contains(r.Header.Get("Accept"), "text/plain"))
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	vals := r.URL.Query()
	if len(vals) > 0 {
		if !h.admitWork(w, r) {
			return
		}
		defer h.work.release()
	}
	hx := r.Header.Get("HX-Request") == "true"
	p := Page{Active: "routes", Title: h.cfg.Server.Title, SelectedBackend: vals.Get("backend"), IsHTMX: hx, View: vals.Get("view"), Groups: h.formGroups(), Backends: h.backendList(), CacheTTL: h.cfg.LookingGlass.CacheTTL, LargeCommunityEnabled: h.cfg.Features.LargeCommunityEnabled()}
	if p.View == "" {
		p.View = "table"
	}
	if p.SelectedBackend == "" {
		p.SelectedBackend = h.cfg.LookingGlass.DefaultBackend
	}
	w.Header().Add("Vary", "HX-Request")

	q, perr := bgp.ParseQuery(vals)
	p.Form = formFor(q, vals)
	p.SearchType, p.Query = p.Form.Type, p.Form.Q
	plain := wantsPlain(r, p.View)

	var problems []string
	if p.View != "table" && p.View != "text" && p.View != "detail" {
		problems = append(problems, "Unsupported view format.")
	}
	page, pageErr := parsePage(vals)
	if pageErr != nil {
		problems = append(problems, pageErr.Error())
	}
	for _, k := range unknownParams(vals) {
		problems = append(problems, fmt.Sprintf("Unknown parameter: %s is not supported; check the spelling of the filter name.", k))
	}
	searching := perr != nil || !q.IsEmpty() || len(problems) > 0
	for _, f := range []struct {
		key     string
		enabled bool
	}{{bgp.KeyCommunity, h.cfg.Features.CommunityEnabled()}, {bgp.KeyLargeCommunity, h.cfg.Features.LargeCommunityEnabled()}, {bgp.KeyExtCommunity, h.cfg.Features.ExtendedCommunityEnabled()}} {
		if !f.enabled && (q.Get(f.key) != "" || (vals.Get("type") == f.key && vals.Get("q") != "")) {
			problems = append(problems, f.key+" search is disabled.")
		}
	}
	if perr != nil {
		problems = append(problems, problemList(perr)...)
	} else if searching {
		nq, err := q.Normalize()
		if err != nil {
			problems = append(problems, problemList(err)...)
		} else {
			q = nq
			p.Form = formFor(q, vals)
			p.SearchType, p.Query = p.Form.Type, p.Form.Q
		}
	}
	p.Problems = problems
	if len(problems) > 0 {
		p.Form.AdvancedOpen = true
	}
	p.Searched = searching

	if searching && len(problems) == 0 {
		// Canonicalize form noise (empty fields) with one redirect; HTMX
		// requests instead tell the browser which URL to record in history.
		canonical := publicURL(q, p.SelectedBackend, p.View, page, plain && !hx)
		if !hx && (hasEmptyParam(vals) || vals.Has(bgp.KeyEndpoint)) {
			http.Redirect(w, r, canonical, http.StatusSeeOther)
			return
		}
		if hx {
			w.Header().Set("HX-Push-Url", publicURL(q, p.SelectedBackend, p.View, page, false))
		}
		results, err := h.search(r.Context(), p.SelectedBackend, q)
		defer func() {
			for i := range results {
				results[i].lease.release()
			}
		}()
		if err != nil {
			p.Problems = append(p.Problems, err.Error())
		} else {
			h.fillResults(&p, q, results, page)
			noteSearch(r, p.Summary, results)
		}
	}
	if len(p.Problems) > 0 {
		p.Error = strings.Join(p.Problems, " ")
	}

	if plain || (len(p.Problems) > 0 && (vals.Get("output") == "plain" || strings.Contains(r.Header.Get("Accept"), "text/plain"))) {
		h.servePlain(w, r, &p, q, page, vals)
		return
	}
	h.scheduleASNs(&p)
	h.schedulePTRs(&p)
	name := "index.html"
	if hx && r.Header.Get("HX-History-Restore-Request") != "true" {
		name = "results.html"
	}
	setCompletionHeaders(w, p.Results, p.Summary)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(p.Problems) > 0 {
		w.WriteHeader(http.StatusBadRequest)
	} else if len(p.Results) > 0 && plainStatus(p.Summary, p.Results) == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	if err := h.templates.ExecuteTemplate(w, name, p); err != nil {
		h.renderFailed(w, r, err)
	}
}

// noteSearch records the query outcome for the access-log line.
func noteSearch(r *http.Request, s Summary, results []BackendResult) {
	i := infoFrom(r.Context())
	if i == nil {
		return
	}
	i.searched, i.result, i.backends = true, s.Status, s.Backends
	for _, res := range results {
		if res.Cached || res.Shared {
			i.cacheHits++
		} else {
			i.cacheMiss++
		}
	}
}

func problemList(err error) []string {
	if ve, ok := err.(*bgp.ValidationError); ok {
		out := make([]string, len(ve.Problems))
		for i, s := range ve.Problems {
			out[i] = capitalize(s) + "."
		}
		return out
	}
	return []string{err.Error()}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// fillResults paginates results and prepares everything the templates need.
func (h *Handler) fillResults(p *Page, q bgp.Query, results []BackendResult, page int) {
	p.Results = results
	p.Summary = summarize(results)
	p.Filters = q.Filters()
	if params, err := q.Params(); err == nil {
		p.Upstream = "/rib?" + params.Encode()
	}
	p.Sections, p.Pager = paginate(results, page, h.cfg.LookingGlass.PageSize)
	p.Pager.setLinks(q, p.SelectedBackend, p.View)
	p.PlainURL = publicURL(q, p.SelectedBackend, p.View, 1, true)
	if p.View != "table" {
		p.Text = sectionsText(p.Sections, p.View)
	}
}

// setCompletionHeaders exposes completion metadata to machine clients.
//
//	X-Hextet-LG-Completeness: complete | partial | failed (overall)
//	X-Hextet-LG-Routes:       total decoded routes across backends
//	X-Hextet-LG-Backends:     id=status[;reason=..];routes=N;elapsed=Ns, ...
func setCompletionHeaders(w http.ResponseWriter, results []BackendResult, s Summary) {
	if len(results) == 0 {
		return
	}
	var parts []string
	for _, r := range results {
		part := r.ID + "=" + string(r.Status)
		if r.Reason != "" {
			part += ";reason=" + string(r.Reason)
		}
		part += fmt.Sprintf(";routes=%d;elapsed=%.1fs", len(r.Routes), r.Elapsed.Seconds())
		if r.Cached {
			part += ";cached=1"
		}
		parts = append(parts, part)
	}
	w.Header().Set("X-Hextet-LG-Completeness", string(s.Status))
	w.Header().Set("X-Hextet-LG-Routes", strconv.Itoa(s.Routes))
	w.Header().Set("X-Hextet-LG-Backends", strings.Join(parts, ", "))
}

// plainStatus maps the overall outcome to an HTTP status. Complete and partial
// results are 200 (the completeness headers and the in-body warning carry the
// distinction); only a query that produced nothing usable is an error.
func plainStatus(s Summary, results []BackendResult) int {
	if s.Status != bgp.StatusFailed {
		return http.StatusOK
	}
	allReason := func(reason bgp.Reason) bool {
		for _, r := range results {
			if r.Reason != reason {
				return false
			}
		}
		return len(results) > 0
	}
	switch {
	case allReason(bgp.ReasonTimeout):
		return http.StatusGatewayTimeout
	case allReason(bgp.ReasonBusy), allReason(bgp.ReasonCanceled), allReason(bgp.ReasonCapacity), allReason(bgp.ReasonMemoryLimit):
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func (h *Handler) servePlain(w http.ResponseWriter, r *http.Request, p *Page, q bgp.Query, page int, vals url.Values) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if len(p.Problems) > 0 {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "ERROR: INVALID REQUEST\n%s\n", strings.Join(p.Problems, "\n"))
		return
	}
	if !p.Searched {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "ERROR: INVALID REQUEST\nSpecify at least one filter.\n")
		return
	}
	setCompletionHeaders(w, p.Results, p.Summary)
	sections := allSections(p.Results)
	paged := len(vals["page"]) > 0 && strings.TrimSpace(vals.Get("page")) != ""
	if paged {
		sections = p.Sections
		w.Header().Set("X-Hextet-LG-Page", fmt.Sprintf("%d/%d", p.Pager.Page, p.Pager.Pages))
		if p.Pager.OutOfRange {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "ERROR: PAGE OUT OF RANGE\nPage %d does not exist; the last page is %d.\n", p.Pager.Page, p.Pager.Pages)
			return
		}
	}
	if plainStatus(p.Summary, p.Results) == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "2")
	}
	w.WriteHeader(plainStatus(p.Summary, p.Results))
	if p.Summary.Incomplete() {
		fmt.Fprintf(w, "WARNING: THIS RESULT IS INCOMPLETE (%d of %d backends did not complete)\nOnly routes that were fully received are shown. Absence of a route here is\nnot evidence that it is absent from the routing table.\n\n", p.Summary.Backends-p.Summary.Complete, p.Summary.Backends)
	}
	if paged {
		fmt.Fprintf(w, "Page %d of %d (routes %d–%d of %d decoded)\n\n", p.Pager.Page, p.Pager.Pages, p.Pager.From, p.Pager.To, p.Pager.Total)
	}
	// Errors here mean the client went away; there is nobody left to tell.
	_ = writeText(r.Context(), w, sections, p.View)
}

func FormatAge(sec *int64) string {
	if sec == nil {
		return "—"
	}
	d := time.Duration(*sec) * time.Second
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

func (h *Handler) basePage(r *http.Request, active string) Page {
	p := Page{Title: h.cfg.Server.Title, Active: active, SelectedBackend: r.URL.Query().Get("backend"), Query: r.URL.Query().Get("q"), Category: r.URL.Query().Get("category"), Backends: h.backendList()}
	if p.SelectedBackend == "" {
		p.SelectedBackend = h.cfg.LookingGlass.DefaultBackend
	}
	return p
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	if !h.admitWork(w, r) {
		return
	}
	defer h.work.release()
	kind := strings.TrimPrefix(r.URL.Path, "/")
	p := h.basePage(r, kind)
	targets := h.selectBackends(p.SelectedBackend)
	// Overview decoding retains both raw and decoded forms. Reserve a generous
	// allowance per backend until rendering completes, including slow writers.
	overviewLease := h.budget.newLease()
	defer overviewLease.release()
	// /rtr answers are bounded at 1 MiB and are never kept raw, so they need
	// far less than the neighbor documents.
	reservation := int64(128 << 20)
	if kind == "rtr" {
		reservation = 4 << 20
	}
	for range targets {
		if !overviewLease.reserve(reservation) {
			w.Header().Set("Retry-After", "2")
			requestError(w, r, http.StatusServiceUnavailable, "Server memory capacity is busy; please retry shortly.")
			return
		}
	}
	if len(targets) == 0 {
		p.Error = "Select an enabled backend."
	}
	sem := make(chan struct{}, h.cfg.LookingGlass.MaxConcurrentBackends)
	results := make(chan BackendResult, len(targets))
	var wg sync.WaitGroup
	for _, b := range targets {
		wg.Add(1)
		go func(b backendClient) {
			defer wg.Done()
			res := BackendResult{ID: b.ID, Name: b.Name, Location: b.Location}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				res.Error = "Request cancelled."
				results <- res
				return
			}
			start := time.Now()
			h.metrics.backendActive.WithLabelValues(b.ID).Inc()
			var status bgp.Status
			var reason bgp.Reason
			var rtrState bgp.RTRState
			if kind == "rtr" {
				status, reason, rtrState = h.queryRTR(r.Context(), b, &res)
			} else {
				status, reason = h.queryOverview(r.Context(), b, kind, &res)
			}
			h.metrics.backendActive.WithLabelValues(b.ID).Dec()
			elapsed := time.Since(start)
			res.Duration = elapsed.Round(time.Millisecond).String()
			if status == bgp.StatusFailed && reason != bgp.ReasonNone {
				lvl := slog.LevelWarn
				if reason == bgp.ReasonCanceled {
					lvl = slog.LevelInfo
				}
				attrs := []slog.Attr{slog.String("request_id", requestID(r.Context())), slog.String("backend", b.ID), slog.String("operation", kind), slog.String("status", string(status)), slog.String("reason", string(reason)), slog.Float64("backend_duration_ms", float64(elapsed.Microseconds())/1000)}
				if rtrState != "" {
					attrs = append(attrs, slog.String("rtr_state", string(rtrState)))
				}
				h.log.LogAttrs(r.Context(), lvl, "backend query", attrs...)
			}
			h.recordBackend(b, kind, status, reason, elapsed)
			results <- res
		}(b)
	}
	wg.Wait()
	close(results)
	for result := range results {
		p.Results = append(p.Results, result)
	}
	sort.Slice(p.Results, func(i, j int) bool { return p.Results[i].Name < p.Results[j].Name })
	h.scheduleASNs(&p)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := "overview.html"
	if kind == "rtr" {
		page = "rtr.html"
	}
	if err := h.templates.ExecuteTemplate(w, page, p); err != nil {
		h.renderFailed(w, r, err)
	}
}

// queryOverview fetches /neighbors or /summary into res.
func (h *Handler) queryOverview(ctx context.Context, b backendClient, kind string, res *BackendResult) (bgp.Status, bgp.Reason) {
	data, err := b.client.Overview(ctx, kind)
	res.Overview = data
	if err != nil {
		res.Error = "Request failed or returned an invalid response."
		return bgp.StatusFailed, overviewReason(err)
	}
	res.Neighbors, res.ShowCounters = neighborRows(data.Neighbors)
	return bgp.StatusComplete, bgp.ReasonNone
}

// queryRTR fetches /rtr into res. Metrics and logs see the outcome as a
// bounded status and reason; the page sees the full five-way RTRState. An
// unsupported backend is not a backend fault, so it is recorded as a failed
// request without a failure reason and does not mark the backend unhealthy.
func (h *Handler) queryRTR(ctx context.Context, b backendClient, res *BackendResult) (bgp.Status, bgp.Reason, bgp.RTRState) {
	rtr := b.client.RTR(ctx)
	res.RTR = rtr
	res.RTRBadge, res.RTRBadgeClass = rtrBadge(rtr)
	switch rtr.State {
	case bgp.RTRSessions:
		res.RTRRows, res.RTRAnyError = rtrRows(rtr.Sessions, h.cfg.Features.RTRSessionAddressesEnabled())
		fallthrough
	case bgp.RTRNoSessions:
		return bgp.StatusComplete, bgp.ReasonNone, rtr.State
	}
	return bgp.StatusFailed, rtr.Reason, rtr.State
}

// maxExplorerRows bounds one Community Explorer page. Real definition sets run
// to thousands of entries; the text and category filters narrow them.
const maxExplorerRows = 1000

// CommunityRow is one Community Explorer row: a definition plus a search link.
type CommunityRow struct {
	communities.Definition
	// Link is a one-click search for an exact definition whose search type is
	// enabled; empty for patterns and disabled search types.
	Link string
}

func (h *Handler) communityPage(w http.ResponseWriter, r *http.Request) {
	p := h.basePage(r, "communities")
	p.CommunityDefs = h.cfg.CommunityDefinitions.Enabled
	if h.defs != nil {
		p.CommunitySource = h.defs.MetadataSource
	}
	cats := map[string]bool{}
	query := strings.ToLower(strings.TrimSpace(p.Query))
	for _, d := range h.defs.Definitions() {
		if d.Category == "" {
			d.Category = d.Source
		}
		cats[d.Category] = true
		if p.Category != "" && p.Category != d.Category {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(d.Key+" "+d.Description+" "+d.Status), query) {
			continue
		}
		p.CommunityMatched++
		if len(p.Communities) < maxExplorerRows {
			p.Communities = append(p.Communities, CommunityRow{Definition: d, Link: h.communityLink(d, p.SelectedBackend)})
		}
	}
	for c := range cats {
		p.Categories = append(p.Categories, c)
	}
	sort.Strings(p.Categories)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, "communities.html", p); err != nil {
		h.renderFailed(w, r, err)
	}
}

// communityLink builds the search link for an exact definition. Patterns have
// no single value to search for. Links follow the M4.2 search flags, so a
// disabled search type never produces a link that would be rejected.
func (h *Handler) communityLink(d communities.Definition, backend string) string {
	if !d.Exact {
		return ""
	}
	switch d.Kind {
	case communities.Large:
		if h.cfg.Features.LargeCommunityEnabled() {
			return searchURL("large-community", d.Key, backend)
		}
	case communities.Standard:
		if h.cfg.Features.CommunityEnabled() {
			return publicURL(bgp.Query{Community: d.Key}, backend, "table", 1, false)
		}
	case communities.Extended:
		if h.cfg.Features.ExtendedCommunityEnabled() {
			return publicURL(bgp.Query{ExtCommunity: d.Key}, backend, "table", 1, false)
		}
	}
	return ""
}

// loadCommunityDefinitions compiles the configured definitions once. An enabled
// but unloadable configuration fails startup; nothing is loaded when disabled.
func (h *Handler) loadCommunityDefinitions() error {
	cd := h.cfg.CommunityDefinitions
	if !cd.Enabled {
		h.log.Debug("community definitions disabled")
		return nil
	}
	set, err := communities.Load(cd.Directory)
	if err != nil {
		return err
	}
	h.defs = set
	st := set.Stats()
	h.log.Info("community definitions loaded", "directory", cd.Directory, "files", st.Files, "definitions", st.Definitions, "exact", st.Exact, "patterns", st.Patterns,
		"standard", st.Standard, "large", st.Large, "extended", st.Extended, "metadata_entries", st.MetadataEntries)
	if st.Files == 0 {
		h.log.Warn("community definitions enabled but the directory contains no asN.txt or well-known.txt files", "directory", cd.Directory)
	}
	if len(st.Ignored) > 0 {
		h.log.Warn("community definition directory contains .txt files that are not asN.txt or well-known.txt and were not loaded", "ignored", st.Ignored)
	}
	return nil
}

// StopAdmission marks the instance unready and rejects new route work.
func (h *Handler) StopAdmission() { h.stopping.Store(true); h.stopOnce.Do(func() { close(h.stopCh) }) }

// Close cancels shared work and releases cached results.
func (h *Handler) Close() {
	h.StopAdmission()
	h.cache.Close()
	if h.ptr != nil {
		h.ptr.Close()
	}
	if h.asn != nil {
		h.asn.Close()
	}
}

// Shutdown is the process shutdown sequence: stop admitting new work, let
// in-flight requests (including slow writers) finish for at most grace, then
// close the remaining connections, which cancels their request contexts and
// with them every shared backend query nobody is waiting for any more, and
// finally release cached results. It returns the graceful-shutdown error
// (context.DeadlineExceeded when requests were still active at the deadline).
// Each phase is logged with the outstanding-work counters.
func (h *Handler) Shutdown(srv *http.Server, grace time.Duration) error {
	start := time.Now()
	h.log.Info("shutdown initiated", "grace", grace.String())
	h.StopAdmission()
	h.log.Info("admission disabled", append([]any{"readyz", "unavailable"}, h.outstanding()...)...)
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err != nil {
		h.log.Warn("shutdown grace expired; closing remaining connections", append([]any{"error", err.Error()}, h.outstanding()...)...)
		_ = srv.Close()
	}
	h.Close()
	// Closed connections cancel their handlers, which unwind asynchronously;
	// give them a moment so the final counts are final. Bounded, well inside
	// the margin between shutdown_grace and the container stop grace period.
	settled := h.settle(500 * time.Millisecond)
	attrs := append([]any{"graceful", err == nil, "settled", settled, "duration_ms", time.Since(start).Milliseconds()}, h.outstanding()...)
	if err != nil {
		h.log.Warn("shutdown finished after deadline", attrs...)
	} else {
		h.log.Info("shutdown complete", attrs...)
	}
	return err
}

// settle waits up to max for in-flight handlers and admitted work to finish.
func (h *Handler) settle(max time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		if h.activeHTTP.Load() == 0 && len(h.work.active) == 0 && len(h.expensive) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// outstanding reports the live work counters for shutdown logs.
func (h *Handler) outstanding() []any {
	used, _ := h.budget.usage()
	_, _, flights := h.cache.snapshot()
	return []any{
		"active_http", h.activeHTTP.Load(),
		"active_work", len(h.work.active),
		"admission_waiters", len(h.work.queue),
		"active_decodes", len(h.expensive),
		"active_flights", flights,
		"retained_bytes", used,
	}
}

// admitWork bounds backend-driven decoding and rendering, including cache hits.
func (h *Handler) admitWork(w http.ResponseWriter, r *http.Request) bool {
	if h.stopping.Load() {
		h.work.reject(gateShutdown)
		w.Header().Set("Retry-After", "2")
		requestError(w, r, http.StatusServiceUnavailable, "Server shutting down.")
		return false
	}
	if !h.work.acquire(r.Context()) {
		w.Header().Set("Retry-After", "2")
		requestError(w, r, http.StatusServiceUnavailable, "Server busy; please retry shortly.")
		return false
	}
	if h.stopping.Load() {
		h.work.release()
		h.work.reject(gateShutdown)
		w.Header().Set("Retry-After", "2")
		requestError(w, r, http.StatusServiceUnavailable, "Server shutting down.")
		return false
	}
	return true
}

func (h *Handler) formGroups() []FormGroup {
	var groups []FormGroup
	for _, group := range formGroups {
		g := FormGroup{Title: group.Title, Flags: group.Flags}
		for _, f := range group.Fields {
			if f.Key == bgp.KeyCommunity && !h.cfg.Features.CommunityEnabled() || f.Key == bgp.KeyLargeCommunity && !h.cfg.Features.LargeCommunityEnabled() || f.Key == bgp.KeyExtCommunity && !h.cfg.Features.ExtendedCommunityEnabled() {
				continue
			}
			g.Fields = append(g.Fields, f)
		}
		if len(g.Fields)+len(g.Flags) > 0 {
			groups = append(groups, g)
		}
	}
	return groups
}
