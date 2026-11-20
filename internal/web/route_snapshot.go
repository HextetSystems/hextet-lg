package web

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

const maxRouteSnapshotBytes = 1 << 20
const maxRouteSnapshotEncoded = 64 << 10 // base64 text; far above any permitted URL length

// gunzipBounded inflates one untrusted, plain gzip member. Output is capped at
// max bytes, so a decompression bomb is rejected after at most max+1 bytes of
// work. It rejects trailing bytes, extra members, and header metadata (name,
// comment, extra field), none of which the application ever produces.
func gunzipBounded(compressed []byte, max int) ([]byte, error) {
	src := bytes.NewReader(compressed)
	zr, err := gzip.NewReader(src)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	zr.Multistream(false)
	if zr.Name != "" || zr.Comment != "" || len(zr.Extra) != 0 {
		return nil, errors.New("unexpected gzip header fields")
	}
	out, err := io.ReadAll(io.LimitReader(zr, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > max {
		return nil, errors.New("decompressed data too large")
	}
	// bytes.Reader is an io.ByteReader, so the decoder does not read ahead:
	// anything left over is data after the gzip trailer.
	if src.Len() != 0 {
		return nil, errors.New("trailing data after gzip stream")
	}
	return out, nil
}

type routeSnapshotPayload struct {
	Created  time.Time              `json:"c"`
	LiveURL  string                 `json:"l"`
	Sections []routeSnapshotSection `json:"s"`
}
type routeSnapshotSection struct {
	ID       string      `json:"i"`
	Name     string      `json:"n"`
	Location string      `json:"l,omitempty"`
	Status   bgp.Status  `json:"s"`
	Reason   bgp.Reason  `json:"r,omitempty"`
	Routes   []bgp.Route `json:"p"`
}

func (h *Handler) routeSnapshotURL(p Page) string {
	if p.LiveURL == "" || len(p.Sections) == 0 || p.View != "table" {
		return ""
	}
	payload := routeSnapshotPayload{Created: time.Now().UTC(), LiveURL: p.LiveURL}
	for _, section := range p.Sections {
		s := routeSnapshotSection{ID: section.Result.ID, Name: section.Result.Name, Location: section.Result.Location, Status: section.Result.Status, Reason: section.Result.Reason}
		for _, route := range section.Rows {
			route.Raw = ""
			s.Routes = append(s.Routes, route)
		}
		payload.Sections = append(payload.Sections, s)
	}
	b, err := json.Marshal(payload)
	if err != nil || len(b) > maxRouteSnapshotBytes {
		return ""
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err = zw.Write(b); err != nil {
		return ""
	}
	if err = zw.Close(); err != nil {
		return ""
	}
	data := base64.RawURLEncoding.EncodeToString(compressed.Bytes())
	u := "/snapshot?data=" + url.QueryEscape(data)
	maxURL, maxQuery := h.cfg.Server.MaxURLBytes, h.cfg.Server.MaxQueryBytes
	if maxURL <= 0 {
		maxURL = 8192
	}
	if maxQuery <= 0 {
		maxQuery = 6144
	}
	if len(data) > maxRouteSnapshotEncoded || len(u) > maxURL || len(strings.TrimPrefix(u, "/snapshot?")) > maxQuery {
		return ""
	}
	return u
}

func (h *Handler) routeSnapshot(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	encoded := v.Get("data")
	if len(v) != 1 || len(v["data"]) != 1 || len(encoded) == 0 || len(encoded) > maxRouteSnapshotEncoded {
		requestError(w, r, 400, "Invalid route snapshot.")
		return
	}
	compressed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(compressed) > maxRouteSnapshotEncoded {
		requestError(w, r, 400, "Invalid route snapshot.")
		return
	}
	b, err := gunzipBounded(compressed, maxRouteSnapshotBytes)
	if err != nil {
		requestError(w, r, 400, "Invalid route snapshot.")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var payload routeSnapshotPayload
	if dec.Decode(&payload) != nil || payload.Created.IsZero() || len(payload.Sections) == 0 || len(payload.Sections) > 64 {
		requestError(w, r, 400, "Invalid route snapshot.")
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		requestError(w, r, 400, "Invalid route snapshot.")
		return
	}
	live, err := url.ParseRequestURI(payload.LiveURL)
	if err != nil || !strings.HasPrefix(payload.LiveURL, "/?") || live.Path != "/" {
		requestError(w, r, 400, "Invalid route snapshot.")
		return
	}
	q, qerr := bgp.ParseQuery(live.Query())
	page, pageErr := parsePage(live.Query())
	if qerr != nil || pageErr != nil || len(unknownParams(live.Query())) != 0 {
		requestError(w, r, 400, "Invalid route snapshot.")
		return
	}
	backend := live.Query().Get("backend")
	if backend == "" {
		backend = h.cfg.LookingGlass.DefaultBackend
	}
	if len(h.selectBackends(backend)) == 0 {
		requestError(w, r, 400, "Invalid route snapshot.")
		return
	}
	count := 0
	results := make([]BackendResult, 0, len(payload.Sections))
	sections := make([]Section, 0, len(payload.Sections))
	for _, item := range payload.Sections {
		validID := false
		for _, be := range h.backends {
			if be.ID == item.ID {
				validID = true
				break
			}
		}
		if !validID || len(item.Name) > 256 || len(item.Location) > 256 || len(item.Routes) > 100 || (item.Status != bgp.StatusComplete && item.Status != bgp.StatusPartial && item.Status != bgp.StatusFailed) {
			requestError(w, r, 400, "Invalid route snapshot.")
			return
		}
		for _, route := range item.Routes {
			count++
			if len(route.Prefix) > 64 || len(route.ASPath) > 4096 || len(route.NeighborDescription) > 256 || len(route.NeighborAddress) > 64 || len(route.ExitNexthop) > 128 || len(route.TrueNexthop) > 128 || len(route.Raw) > 0 || len(route.Communities) > 256 || len(route.ExtCommunities) > 256 || len(route.LargeCommunities) > 256 {
				requestError(w, r, 400, "Invalid route snapshot.")
				return
			}
		}
		res := BackendResult{ID: item.ID, Name: item.Name, Location: item.Location, Result: bgp.Result{Routes: item.Routes, Completion: bgp.Completion{Status: item.Status, Reason: item.Reason, RouteCount: len(item.Routes)}}}
		results = append(results, res)
		sections = append(sections, Section{Result: res, Rows: item.Routes})
	}
	if count > 100 {
		requestError(w, r, 400, "Invalid route snapshot.")
		return
	}
	p := h.basePage(r, "routes")
	p.Title = "Route snapshot"
	p.Active = "routes"
	p.View = "table"
	p.Searched = true
	p.Form = formFor(q, live.Query())
	p.SearchType, p.Query = p.Form.Type, p.Form.Q
	p.Results = results
	p.Sections = sections
	p.Summary = summarize(results)
	p.SnapshotAt = payload.Created.UTC()
	p.LiveURL = payload.LiveURL
	p.Pager = Pager{Page: page, Pages: 1, Size: 100, Total: count, From: 1, To: count}
	p.Cols = h.routeCols()
	p.Groups = h.formGroups()
	p.SelectedBackend = backend
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, "index.html", p); err != nil {
		h.renderFailed(w, r, err)
	}
}
