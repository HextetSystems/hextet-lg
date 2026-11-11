package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// themeCSS serves the stylesheet generated once at startup from the validated
// theme configuration. It is never regenerated per request. The strong ETag
// is derived from its bytes, so a conditional request after a restart with new
// colors can never be answered "not modified" for the old stylesheet, and
// no-cache makes browsers revalidate rather than reuse a stale copy.
func (h *Handler) themeCSS(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/css; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("ETag", h.theme.ETag)
	// A zero modification time suppresses Last-Modified; ServeContent still
	// answers If-None-Match, HEAD and ranges.
	http.ServeContent(w, r, "theme.css", time.Time{}, bytes.NewReader(h.theme.CSS))
}

// assetVersions fingerprints every embedded static file, so a new build is
// never paired with a cached older stylesheet or script. The key set is also
// the complete list of files the static handler serves.
func assetVersions() map[string]string {
	out := map[string]string{}
	entries, err := fs.ReadDir(assets, "static")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := fs.ReadFile(assets, "static/"+e.Name())
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		out[e.Name()] = hex.EncodeToString(sum[:6])
	}
	return out
}

// staticAsset serves the embedded static files by exact name. Anything else
// under /static/ (the directory itself, a subdirectory, an unknown name) is a
// 404: there is no directory listing. Embedded files have no modification
// time, so the content fingerprint is the validator: a request carrying the
// current fingerprint (the URL templates emit) is immutable, any other URL
// revalidates against the strong ETag instead of being downloaded again.
func (h *Handler) staticAsset(files http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := h.assetVer[strings.TrimPrefix(r.URL.Path, "/static/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		hdr := w.Header()
		hdr.Set("ETag", `"`+v+`"`)
		if r.URL.Query().Get("v") == v {
			hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			hdr.Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}
}

// assetURL is the versioned URL of a static file used by the page templates.
// theme.css is versioned by its ETag content hash.
func (h *Handler) assetURL(name string) string {
	v := h.assetVer[name]
	if name == "theme.css" {
		v = h.theme.ETag[1 : len(h.theme.ETag)-1]
	}
	if v == "" {
		return "/static/" + name
	}
	return "/static/" + name + "?v=" + url.QueryEscape(v)
}
