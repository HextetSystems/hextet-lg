package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/HextetSystems/hextet-lg/internal/asn"
)

type pathPart struct{ Text, ASN, Title string }

func parseASN(s string) uint32 {
	n, e := strconv.ParseUint(s, 10, 32)
	if e != nil || !asn.Eligible(uint32(n)) {
		return 0
	}
	return uint32(n)
}
func (h *Handler) asnTitle(s string) string {
	if h.asn == nil {
		return ""
	}
	n := parseASN(s)
	if n == 0 {
		return ""
	}
	e, ok := h.asn.Get(n)
	if ok && !e.Negative {
		return e.Description
	}
	return ""
}
func (h *Handler) pathParts(s string) []pathPart {
	if s == "" {
		return []pathPart{{Text: "—"}}
	}
	var out []pathPart
	start := 0
	for i := 0; i < len(s); {
		if s[i] < '0' || s[i] > '9' {
			i++
			continue
		}
		if start < i {
			out = append(out, pathPart{Text: s[start:i]})
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		v := s[i:j]
		part := pathPart{Text: v}
		if h.asn != nil && parseASN(v) != 0 {
			part.ASN = v
			part.Title = h.asnTitle(v)
		}
		out = append(out, part)
		i = j
		start = j
	}
	if start < len(s) {
		out = append(out, pathPart{Text: s[start:]})
	}
	return out
}
func (h *Handler) scheduleASNs(p *Page) {
	if h.asn == nil {
		return
	}
	budget := h.cfg.ASNEnrichment.MaxLookupsPerPage
	seen := map[uint32]bool{}
	add := func(s string) {
		n := parseASN(s)
		if n == 0 || seen[n] {
			return
		}
		seen[n] = true
		if _, ok := h.asn.Get(n); ok {
			return
		}
		if budget > 0 {
			budget--
			h.asn.Schedule(n)
		}
	}
	if p.Active == "routes" && p.View == "table" {
		for _, sec := range p.Sections {
			for _, r := range sec.Rows {
				add(r.OriginASN)
			}
		}
		for _, sec := range p.Sections {
			for _, r := range sec.Rows {
				for _, v := range strings.FieldsFunc(r.ASPath, func(r rune) bool { return !unicode.IsDigit(r) }) {
					add(v)
				}
			}
		}
	} else if p.Active == "neighbors" || p.Active == "summary" {
		for _, result := range p.Results {
			for _, n := range result.Overview.Neighbors {
				add(n.RemoteAS)
			}
		}
	}
}

// asnStatus reads only already cached results; clients cannot trigger DNS here.
func (h *Handler) asnStatus(w http.ResponseWriter, r *http.Request) {
	if h.asn == nil {
		http.NotFound(w, r)
		return
	}
	vals := r.URL.Query()
	if len(vals) != 1 || len(vals["asn"]) != 1 {
		http.Error(w, "invalid request", 400)
		return
	}
	parts := strings.Split(vals.Get("asn"), ",")
	if len(parts) > 256 || len(parts) == 0 {
		http.Error(w, "invalid request", 400)
		return
	}
	out := map[string]string{}
	for _, v := range parts {
		if parseASN(v) == 0 {
			http.Error(w, "invalid request", 400)
			return
		}
		if title := h.asnTitle(v); title != "" {
			out[v] = title
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
