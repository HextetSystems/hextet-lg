package web

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

// Section is one backend's slice of the current page. Rows holds only the
// routes shown on this page; First and Last are 1-based positions within the
// backend's decoded routes (zero when the backend has no rows on this page).
type Section struct {
	Result      BackendResult
	Rows        []bgp.Route
	First, Last int
}

// Pager describes the position within the concatenation of all backends'
// decoded routes (backends in display order, routes in upstream order).
type Pager struct {
	Page, Pages, Size int
	Total             int
	From, To          int // 1-based inclusive range shown; zero when none
	OutOfRange        bool
	PrevURL, NextURL  string
	FirstURL, LastURL string
	HasPrev, HasNext  bool
}

// parsePage reads the page number. An absent value is page 1; anything that is
// not a positive integer is an error rather than silently becoming page 1.
func parsePage(vals url.Values) (int, error) {
	v := vals["page"]
	if len(v) == 0 || strings.TrimSpace(v[0]) == "" {
		return 1, nil
	}
	if len(v) > 1 {
		return 0, fmt.Errorf("page was given more than once")
	}
	n, err := strconv.Atoi(strings.TrimSpace(v[0]))
	if err != nil || n < 1 || n > 1_000_000_000 {
		return 0, fmt.Errorf("invalid page number %q: use a positive whole number", v[0])
	}
	return n, nil
}

// paginate selects the routes for one page without copying route data: Rows
// are sub-slices of the decoded routes.
func paginate(results []BackendResult, page, size int) ([]Section, Pager) {
	total := 0
	for _, r := range results {
		total += len(r.Routes)
	}
	pg := Pager{Page: page, Size: size, Total: total, Pages: max(1, (total+size-1)/size)}
	pg.OutOfRange = page > pg.Pages
	start := (page - 1) * size
	end := start + size
	sections := make([]Section, len(results))
	offset := 0
	for i, r := range results {
		s := Section{Result: r}
		lo, hi := max(start-offset, 0), min(end-offset, len(r.Routes))
		if lo < hi && !pg.OutOfRange {
			s.Rows = r.Routes[lo:hi]
			s.First, s.Last = lo+1, hi
		}
		offset += len(r.Routes)
		sections[i] = s
	}
	if total > 0 && !pg.OutOfRange {
		pg.From, pg.To = start+1, min(end, total)
	}
	pg.HasPrev = page > 1
	pg.HasNext = page < pg.Pages
	return sections, pg
}

// allSections returns every route of every backend as a single page.
func allSections(results []BackendResult) []Section {
	out := make([]Section, len(results))
	for i, r := range results {
		out[i] = Section{Result: r, Rows: r.Routes}
		if len(r.Routes) > 0 {
			out[i].First, out[i].Last = 1, len(r.Routes)
		}
	}
	return out
}

// publicQuery builds the canonical, bookmarkable application query string for
// a route search. Simple prefix, source-AS and Large Community searches keep
// the original type/q form; compound searches use the named filters in a fixed
// order. backend and view are always present, page only beyond the first page.
func publicQuery(q bgp.Query, backend, view string, page int, plain bool) string {
	var pairs [][2]string
	pairs = append(pairs, q.PublicPairs()...)
	pairs = append(pairs, [2]string{"backend", backend}, [2]string{"view", view})
	if page > 1 {
		pairs = append(pairs, [2]string{"page", strconv.Itoa(page)})
	}
	if plain {
		pairs = append(pairs, [2]string{"output", "plain"})
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = url.QueryEscape(p[0]) + "=" + url.QueryEscape(p[1])
	}
	return strings.Join(parts, "&")
}

func publicURL(q bgp.Query, backend, view string, page int, plain bool) string {
	return "/?" + publicQuery(q, backend, view, page, plain)
}

// neighborURL uses only an address supplied by the backend, never a label.
func neighborURL(address, backend string) string {
	a, err := netip.ParseAddr(address)
	if err != nil || backend == "" || backend == "all" {
		return ""
	}
	return publicURL(bgp.Query{Neighbor: a.String()}, backend, "table", 1, false)
}

func neighborTitle(description, address, remoteAS string) string {
	var parts []string
	for _, s := range []string{description, address} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if remoteAS != "" {
		parts = append(parts, "AS"+remoteAS)
	}
	return strings.Join(parts, " — ")
}

func (p *Pager) setLinks(q bgp.Query, backend, view string) {
	p.FirstURL = publicURL(q, backend, view, 1, false)
	p.LastURL = publicURL(q, backend, view, p.Pages, false)
	p.PrevURL = publicURL(q, backend, view, max(1, min(p.Page-1, p.Pages)), false)
	p.NextURL = publicURL(q, backend, view, p.Page+1, false)
}
