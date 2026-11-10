package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/url"
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

// assetVersions fingerprints the embedded static files that templates link, so
// a new build is never paired with a cached older stylesheet or script.
func assetVersions() map[string]string {
	out := map[string]string{}
	for _, name := range []string{"app.css", "app.js", "theme-init.js"} {
		b, err := fs.ReadFile(assets, "static/"+name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		out[name] = hex.EncodeToString(sum[:6])
	}
	return out
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
