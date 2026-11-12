package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

const maxRouteMapURL = 4096
const routeMapLayoutQueryBytes = len("&layout=horizontal")

type routeMapPayload struct {
	FetchedAt time.Time      `json:"t"`
	Paths     []routeMapPath `json:"p"`
}
type routeMapPath struct {
	ASPath  string `json:"a"`
	Peer    string `json:"n,omitempty"`
	Address string `json:"i,omitempty"`
	Best    bool   `json:"b,omitempty"`
}

type RouteMap struct {
	Prefix         string
	Backend        string
	FetchedAt      time.Time
	Nodes          []RouteMapNode
	Edges          []RouteMapEdge
	PathCount      int
	Width          int
	Height         int
	VerticalWidth  int
	VerticalHeight int
	ASNEnabled     bool
	HasBest        bool
	Horizontal     bool
	LayoutURL      string
	LayoutLabel    string
}
type RouteMapNode struct {
	ID, Label, ASN, Kind string
	X, Y, VX, VY         int
	Best, Enrich         bool
}
type RouteMapEdge struct {
	X1, Y1, X2, Y2     int
	VX1, VY1, VX2, VY2 int
	Best, StartBest    bool
}

type RouteMapView struct {
	Class, Prefix, TitleID   string
	Nodes                    []RouteMapNode
	Edges                    []RouteMapEdge
	PathCount, Width, Height int
	ASNEnabled, HasBest      bool
}

func (h *Handler) routeMapURL(rows []bgp.Route, backend, prefix string, fetchedAt time.Time) string {
	if _, err := netip.ParsePrefix(prefix); err != nil || !h.cfg.Features.BGPMapsEnabled() || backend == "" || fetchedAt.IsZero() {
		return ""
	}
	payload := routeMapPayload{FetchedAt: fetchedAt.UTC()}
	count := 0
	for _, r := range rows {
		if r.Prefix != prefix {
			continue
		}
		count++
		if count > 128 || len(r.ASPath) > 512 || len(r.NeighborDescription) > 256 || len(r.NeighborAddress) > 64 || len(mapASPathTokens(r.ASPath)) > 64 {
			return ""
		}
		payload.Paths = append(payload.Paths, routeMapPath{ASPath: r.ASPath, Peer: r.NeighborDescription, Address: r.NeighborAddress, Best: r.Best})
	}
	if count < 2 {
		return ""
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	data := base64.RawURLEncoding.EncodeToString(b)
	values := url.Values{"prefix": {prefix}, "backend": {backend}, "data": {data}}
	u := "/map?" + values.Encode()
	maxURL, maxQuery := h.cfg.Server.MaxURLBytes, h.cfg.Server.MaxQueryBytes
	if maxURL <= 0 {
		maxURL = 8192
	}
	if maxQuery <= 0 {
		maxQuery = 6144
	}
	if len(u)+routeMapLayoutQueryBytes > maxRouteMapURL || len(u)+routeMapLayoutQueryBytes > maxURL || len(strings.TrimPrefix(u, "/map?"))+routeMapLayoutQueryBytes > maxQuery {
		return ""
	}
	return u
}

func (h *Handler) routeMap(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	if (len(values) != 3 && len(values) != 4) || len(values["prefix"]) != 1 || len(values["backend"]) != 1 || len(values["data"]) != 1 {
		requestError(w, r, 400, "Invalid map request.")
		return
	}
	layout := values.Get("layout")
	if len(values["layout"]) > 1 || (layout != "" && layout != "horizontal") || (layout == "" && len(values) == 4) {
		requestError(w, r, 400, "Invalid map request.")
		return
	}
	prefix, err := netip.ParsePrefix(values.Get("prefix"))
	if err != nil || prefix.String() != values.Get("prefix") {
		requestError(w, r, 400, "Invalid map request.")
		return
	}
	backend := values.Get("backend")
	knownBackend := false
	for _, b := range h.backends {
		if b.ID == backend {
			knownBackend = true
			break
		}
	}
	if !knownBackend {
		requestError(w, r, 400, "Invalid map request.")
		return
	}
	encoded := values.Get("data")
	if len(encoded) == 0 || len(encoded) > maxRouteMapURL {
		requestError(w, r, 400, "Invalid map data.")
		return
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) > 3072 {
		requestError(w, r, 400, "Invalid map data.")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.DisallowUnknownFields()
	var payload routeMapPayload
	if err := dec.Decode(&payload); err != nil || len(payload.Paths) < 2 || len(payload.Paths) > 128 {
		requestError(w, r, 400, "Invalid map data.")
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		requestError(w, r, 400, "Invalid map data.")
		return
	}
	if payload.FetchedAt.IsZero() {
		requestError(w, r, 400, "Invalid map data.")
		return
	}
	model, valid := buildRouteMap(prefix.String(), backend, payload.Paths, h.asn != nil)
	if !valid {
		requestError(w, r, 400, "Invalid map data.")
		return
	}
	model.FetchedAt = payload.FetchedAt.UTC()
	model.Horizontal = layout == "horizontal"
	model.LayoutLabel = "Show horizontal map"
	if model.Horizontal {
		model.LayoutLabel = "Show vertical map"
		values.Del("layout")
	} else {
		values.Set("layout", "horizontal")
	}
	model.LayoutURL = "/map?" + values.Encode()
	h.scheduleASNs(&Page{Active: "map", Map: model})
	page := Page{Active: "map", Title: h.sessionTitle("BGP path map", backend), Map: model, Started: requestStart(r)}
	if name := h.backendTitleName(backend); name != "" {
		page.Title = h.sessionTitle("BGP path map @ "+name, "")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, "route_map.html", page); err != nil {
		h.renderFailed(w, r, err)
	}
}

func routeMapTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func routeMapDisplayTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

func routeMapView(model RouteMap, vertical bool) RouteMapView {
	view := RouteMapView{
		Class: "horizontal", Prefix: model.Prefix, TitleID: "map-horizontal",
		PathCount: model.PathCount, Width: model.Width, Height: model.Height,
		ASNEnabled: model.ASNEnabled, HasBest: model.HasBest,
		Nodes: append([]RouteMapNode(nil), model.Nodes...),
		Edges: append([]RouteMapEdge(nil), model.Edges...),
	}
	if !vertical {
		return view
	}
	view.Class, view.TitleID = "vertical", "map-vertical"
	view.Width, view.Height = model.VerticalWidth, model.VerticalHeight
	for i := range view.Nodes {
		view.Nodes[i].X, view.Nodes[i].Y = view.Nodes[i].VX, view.Nodes[i].VY
	}
	for i := range view.Edges {
		view.Edges[i].X1, view.Edges[i].Y1 = view.Edges[i].VX1, view.Edges[i].VY1
		view.Edges[i].X2, view.Edges[i].Y2 = view.Edges[i].VX2, view.Edges[i].VY2
	}
	return view
}

func buildRouteMap(prefix, backend string, paths []routeMapPath, asnEnabled bool) (RouteMap, bool) {
	model := RouteMap{Prefix: prefix, Backend: backend, ASNEnabled: asnEnabled, PathCount: len(paths)}
	if len(paths) < 2 || len(paths) > 128 {
		return model, false
	}
	tokens := make([][]string, len(paths))
	for i, p := range paths {
		if len(p.ASPath) > 512 || len(p.Peer) > 256 || len(p.Address) > 64 {
			return model, false
		}
		tokens[i] = mapASPathTokens(p.ASPath)
		if len(tokens[i]) > 64 {
			return model, false
		}
	}
	nodes := map[string]*RouteMapNode{}
	lanes := map[string][]int{}
	addNode := func(key, label, asn, kind string, lane int, best, enrich bool) {
		n := nodes[key]
		if n == nil {
			n = &RouteMapNode{ID: key, Label: label, ASN: asn, Kind: kind}
			nodes[key] = n
		}
		lanes[key] = append(lanes[key], lane)
		n.Best = n.Best || best
		n.Enrich = n.Enrich || enrich
	}
	type edgeKey struct{ a, b string }
	type edgeFlags struct{ best, startBest bool }
	edges := map[edgeKey]edgeFlags{}
	for i, p := range paths {
		peer := strings.TrimSpace(p.Peer)
		if peer == "" {
			peer = strings.TrimSpace(p.Address)
		}
		if peer == "" {
			peer = "Peer"
		}
		peerKey := "peer:" + p.Address + ":" + p.Peer
		keys := []string{peerKey}
		addNode(peerKey, peer, "", "peer", i, p.Best, false)
		for _, token := range tokens[i] {
			// Repeated adjacent ASNs are AS prepends. Keep them as one graph
			// vertex so they do not become a self-loop in the network map.
			if len(keys) > 1 && nodes[keys[len(keys)-1]].ASN == token && token != "" {
				continue
			}
			asn := ""
			if n, err := strconv.ParseUint(token, 10, 32); err == nil && n > 0 {
				asn = token
			}
			key := "asn:" + token
			keys = append(keys, key)
			addNode(key, token, asn, "asn", i, p.Best, parseASN(token) != 0)
		}
		destKey := "destination:" + prefix
		keys = append(keys, destKey)
		addNode(destKey, prefix, "", "destination", i, p.Best, false)
		for j := 0; j < len(keys)-1; j++ {
			k := edgeKey{keys[j], keys[j+1]}
			f := edges[k]
			f.best = f.best || p.Best
			f.startBest = f.startBest || (j == 0 && p.Best)
			edges[k] = f
		}
	}
	// Give each vertex a topological layer. This keeps a shared ASN in one
	// place even when routes reach it after different numbers of AS hops.
	indegree := make(map[string]int, len(nodes))
	outgoing := make(map[string][]string, len(nodes))
	for edge := range edges {
		indegree[edge.b]++
		outgoing[edge.a] = append(outgoing[edge.a], edge.b)
	}
	ready := make([]string, 0, len(nodes))
	layersByNode := make(map[string]int, len(nodes))
	for key := range nodes {
		if indegree[key] == 0 {
			ready = append(ready, key)
		}
	}
	sort.Strings(ready)
	visited := 0
	maxLayer := 0
	for len(ready) > 0 {
		key := ready[0]
		ready = ready[1:]
		visited++
		for _, next := range outgoing[key] {
			if layersByNode[next] < layersByNode[key]+1 {
				layersByNode[next] = layersByNode[key] + 1
			}
			if layersByNode[next] > maxLayer {
				maxLayer = layersByNode[next]
			}
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
				sort.Strings(ready)
			}
		}
	}
	if visited != len(nodes) {
		return model, false
	}
	model.Width = 240 + (maxLayer+1)*230
	model.VerticalHeight = 140 + maxLayer*230
	for key, n := range nodes {
		layer := layersByNode[key]
		n.X = 120 + layer*230
	}
	model.Height = max(180, (len(paths)+1)*100)
	model.VerticalWidth = model.Height + 120
	model.HasBest = false
	for key, n := range nodes {
		model.HasBest = model.HasBest || n.Best
		avg := 0
		for _, y := range lanes[key] {
			avg += y
		}
		n.Y = avg*100/len(lanes[key]) + 60
		n.VX, n.VY = n.Y+120, n.X-60
	}
	// Keep a node near the lanes of the paths that use it. Shared tails meet
	// at the same ASN without flattening distinct branches into one row.
	for _, n := range nodes {
		model.Nodes = append(model.Nodes, *n)
	}
	sort.Slice(model.Nodes, func(i, j int) bool {
		if model.Nodes[i].X == model.Nodes[j].X {
			return model.Nodes[i].Y < model.Nodes[j].Y
		}
		return model.Nodes[i].X < model.Nodes[j].X
	})
	for key, flags := range edges {
		a, b := nodes[key.a], nodes[key.b]
		endInset := 95
		if b.Kind == "destination" {
			endInset = 105 // stop before the wider destination diamond
		}
		edge := RouteMapEdge{X1: a.X + 95, Y1: a.Y, X2: b.X - endInset, Y2: b.Y, Best: flags.best, StartBest: flags.startBest}
		edge.VX1, edge.VY1 = edge.Y1+120, edge.X1-60
		edge.VX2, edge.VY2 = edge.Y2+120, edge.X2-60
		model.Edges = append(model.Edges, edge)
	}
	sort.Slice(model.Edges, func(i, j int) bool {
		if model.Edges[i].X1 == model.Edges[j].X1 {
			return model.Edges[i].Y1 < model.Edges[j].Y1
		}
		return model.Edges[i].X1 < model.Edges[j].X1
	})
	return model, true
}

func mapASPathTokens(path string) []string {
	var out []string
	for i := 0; i < len(path); {
		for i < len(path) && strings.ContainsRune(" \t\r\n", rune(path[i])) {
			i++
		}
		if i >= len(path) {
			break
		}
		if path[i] == '{' {
			j := strings.IndexByte(path[i:], '}')
			if j < 0 {
				return []string{strings.TrimSpace(path)}
			}
			j += i + 1
			out = append(out, strings.TrimSpace(path[i:j]))
			i = j
			continue
		}
		j := i
		for j < len(path) && !strings.ContainsRune(" \t\r\n", rune(path[j])) {
			j++
		}
		token := strings.Trim(path[i:j], "(),")
		if token != "" {
			out = append(out, token)
		}
		i = j
	}
	return out
}

func routeMapAdd(a, b int) int { return a + b }

func routeMapLabel(label string) string {
	runes := []rune(label)
	if len(runes) > 24 {
		return string(runes[:21]) + "…"
	}
	return label
}
