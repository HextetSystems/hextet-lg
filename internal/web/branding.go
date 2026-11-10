package web

import (
	"bytes"
	"net/http"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/site"
)

// siteAssetURL is the stable, versioned URL of a branding asset. The version
// is the content fingerprint, so replacing a file (and restarting) changes the
// URL and a browser never keeps showing an older logo.
func siteAssetURL(path string, a *site.Asset) string {
	if a == nil {
		return ""
	}
	return path + "?v=" + a.Version
}

// brandingAsset serves one validated, in-memory asset. A request carrying the
// current fingerprint is immutable and cached for a year; any other URL (for
// example /favicon.ico) must revalidate against the strong ETag.
func (h *Handler) brandingAsset(a *site.Asset) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Type", a.ContentType)
		hdr.Set("ETag", a.ETag)
		if r.URL.Query().Get("v") == a.Version {
			hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			hdr.Set("Cache-Control", "no-cache")
		}
		if a.ContentType == "image/svg+xml" {
			// Opened directly, an SVG must be inert: no script, no loads, and
			// a unique opaque origin.
			hdr.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.Data))
	}
}
