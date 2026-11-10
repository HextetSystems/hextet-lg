package web

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

// Browser document titles.
//
// Every HTML page gets a title of the form "<subject> · <site>". The subject
// describes what the page shows (the canonical search, or the page name); the
// site name is the configured deployment name (site.name, else the legacy
// server.title, else a generic default; see internal/site). siteName is the
// only place that decides the suffix.
//
// Rules for route searches (routeTitleSubject):
//
//   - No search: "Route Explorer".
//   - Invalid request: "Invalid search". The rejected input is never echoed.
//   - Otherwise the subject is the canonical (normalized) primary filter, the
//     first present of prefix, source AS, large community, community, extended
//     community, neighbor, peer group. Values that identify themselves by shape
//     (prefix, "AS<n>", x:y:z large community) appear bare; the others carry a
//     short kind label ("Community …", "Neighbor …").
//   - Further filters are never listed: they are summarized as "+N filters". The
//     prefix modes read "(more specifics)" / "(covering prefixes)". A search
//     with filters but no primary one is "Route search (N filters)".
//   - A single selected backend is appended as "@ <name>" (neighbor addresses
//     and route views are backend-specific); "all" adds nothing. Page 2 and up
//     add "(page N)".
//   - Every user-derived part is stripped of control characters and bounded, and
//     the result is escaped by html/template like any other text.
const (
	titleSep         = " · "
	maxSubjectRunes  = 64
	maxBackendRunes  = 32
	titleRouteSearch = "Route Explorer"
	titleInvalid     = "Invalid search"
)

// siteName is the title suffix: the resolved, validated site name.
func (h *Handler) siteName() string { return h.site.Name }

// SiteName is the resolved site name, for the startup log line.
func (h *Handler) SiteName() string { return h.site.Name }

// pageTitle composes the document title for a subject.
func (h *Handler) pageTitle(subject string) string {
	site := h.siteName()
	switch {
	case subject == "":
		return site
	case site == "":
		return subject
	}
	return subject + titleSep + site
}

// backendTitleName returns the configured name of a selected backend, or ""
// for "all", an empty selection or an unknown ID (never echoed).
func (h *Handler) backendTitleName(id string) string {
	if id == "" || id == "all" {
		return ""
	}
	for _, b := range h.backends {
		if b.ID == id {
			return titleText(b.Name, maxBackendRunes)
		}
	}
	return ""
}

// routeTitle is the title of a Route Explorer request. invalid means the
// request failed validation; searched means it carried any search input.
func (h *Handler) routeTitle(q bgp.Query, searched, invalid bool, backend string, page int) string {
	switch {
	case invalid:
		return h.pageTitle(titleInvalid)
	case !searched:
		return h.pageTitle(titleRouteSearch)
	}
	subject := routeTitleSubject(q)
	if name := h.backendTitleName(backend); name != "" {
		subject += " @ " + name
	}
	if page > 1 {
		subject += " (page " + strconv.Itoa(page) + ")"
	}
	return h.pageTitle(subject)
}

// sessionTitle is the title of a backend-scoped session page.
func (h *Handler) sessionTitle(name, backend string) string {
	if b := h.backendTitleName(backend); b != "" {
		name += " @ " + b
	}
	return h.pageTitle(name)
}

// routeTitleSubject summarizes a normalized query. See the rules above.
func routeTitleSubject(q bgp.Query) string {
	filters := q.Filters()
	var primary, label string
	for _, k := range []struct{ key, label string }{
		{bgp.KeyPrefix, ""}, {bgp.KeyAS, "AS"}, {bgp.KeyLargeCommunity, ""},
		{bgp.KeyCommunity, "Community "}, {bgp.KeyExtCommunity, "Extended community "},
		{bgp.KeyNeighbor, "Neighbor "}, {bgp.KeyGroup, "Peer group "},
	} {
		if v := q.Get(k.key); v != "" {
			primary, label = k.key, k.label+v
			break
		}
	}
	if primary == "" {
		return "Route search (" + countNoun(len(filters), "filter") + ")"
	}
	extra := len(filters) - 1
	subject := titleText(label, maxSubjectRunes)
	if primary == bgp.KeyPrefix {
		switch {
		case q.All:
			subject += " (more specifics)"
			extra--
		case q.OrShorter:
			subject += " (covering prefixes)"
			extra--
		}
	}
	if extra > 0 {
		subject += " +" + countNoun(extra, "filter")
	}
	return subject
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// titleText makes user-derived text safe and bounded for a title: control and
// invisible format characters are dropped, runs of white space collapse to one
// space, and anything longer than max runes ends in an ellipsis.
func titleText(s string, max int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError:
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	out := b.String()
	if utf8.RuneCountInString(out) <= max {
		return out
	}
	return string([]rune(out)[:max-1]) + "…"
}
