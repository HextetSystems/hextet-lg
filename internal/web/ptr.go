package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/HextetSystems/hextet-lg/internal/ptr"
)

func (h *Handler) ptrKey(s string) string {
	if h.ptr == nil {
		return ""
	}
	a, ok := ptr.Address(s)
	if !ok {
		return ""
	}
	return a.String()
}
func (h *Handler) ptrTitle(s string) string {
	if h.ptr == nil {
		return ""
	}
	a, ok := ptr.Address(s)
	if !ok {
		return ""
	}
	e, ok := h.ptr.Get(a)
	if ok && !e.Negative {
		return e.Name
	}
	return ""
}
func (h *Handler) schedulePTRs(p *Page) {
	if h.ptr == nil || p.Active != "routes" || p.View != "table" {
		return
	}
	seen := map[string]bool{}
	budget := h.cfg.PTREnrichment.MaxLookupsPerPage
	add := func(s string) {
		key := h.ptrKey(s)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		a, _ := ptr.Address(key)
		if _, ok := h.ptr.Get(a); ok {
			return
		}
		if budget > 0 {
			budget--
			h.ptr.Schedule(a)
		}
	}
	for _, sec := range p.Sections {
		for _, r := range sec.Rows {
			if h.ptrKey(r.NextHop()) != "" {
				add(r.NextHop())
			} else {
				add(r.ExitNexthop)
			}
		}
	}
	for _, sec := range p.Sections {
		for _, r := range sec.Rows {
			add(r.TrueNexthop)
			add(r.ExitNexthop)
		}
	}
}
func (h *Handler) ptrStatus(w http.ResponseWriter, r *http.Request) {
	if h.ptr == nil {
		http.NotFound(w, r)
		return
	}
	vals := r.URL.Query()
	if len(vals) != 1 || len(vals["ip"]) != 1 {
		http.Error(w, "invalid request", 400)
		return
	}
	parts := strings.Split(vals.Get("ip"), ",")
	if len(parts) > 256 || len(parts) == 0 {
		http.Error(w, "invalid request", 400)
		return
	}
	out := map[string]string{}
	for _, v := range parts {
		_, ok := ptr.Address(v)
		if !ok {
			http.Error(w, "invalid request", 400)
			return
		}
		if title := h.ptrTitle(v); title != "" {
			out[v] = title
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
