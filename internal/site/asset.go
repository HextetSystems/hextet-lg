package site

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/gif" // register the decoders used by validate
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Size limits. Assets are read once at startup and kept in memory; they are
// small by design (the header is compact).
const (
	maxLogoBytes    = 512 << 10
	maxFaviconBytes = 128 << 10
	maxPixels       = 4096 // per side, for raster images
)

// Asset is a validated image kept in memory and served at a fixed URL.
type Asset struct {
	Data        []byte
	ContentType string
	// ETag is a strong validator and Version a short fingerprint used in the
	// asset URL, so a changed file is never paired with a cached older copy.
	ETag, Version string
	// Width and Height are the intrinsic dimensions when known (0 otherwise).
	// The header uses them to reserve space and avoid layout shift.
	Width, Height int
	// Builtin marks the generated default icon.
	Builtin bool
}

type kind struct{ contentType string }

// Allowed file types by extension. SVG is only ever referenced from <img> and
// is served sandboxed; it is validated, never inlined into a page.
var (
	logoKinds = map[string]kind{
		".png": {"image/png"}, ".jpg": {"image/jpeg"}, ".jpeg": {"image/jpeg"},
		".gif": {"image/gif"}, ".svg": {"image/svg+xml"},
	}
	faviconKinds = map[string]kind{
		".png": {"image/png"}, ".jpg": {"image/jpeg"}, ".jpeg": {"image/jpeg"},
		".gif": {"image/gif"}, ".svg": {"image/svg+xml"}, ".ico": {"image/x-icon"},
	}
)

func kindList(m map[string]kind) string {
	var out []string
	for _, e := range []string{".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico"} {
		if _, ok := m[e]; ok {
			out = append(out, e)
		}
	}
	return strings.Join(out, ", ")
}

// assetName validates a configured file name. It must be a plain base name:
// no directories, separators, parent references or NUL bytes. This is the
// first of two traversal defences; the file is also opened through os.Root.
func assetName(field, name string, kinds map[string]kind) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || filepath.Base(name) != name {
		return fmt.Errorf("%s must be a file name inside site.branding.directory, without a path", field)
	}
	if _, ok := kinds[strings.ToLower(filepath.Ext(name))]; !ok {
		return fmt.Errorf("%s: unsupported file type %q (supported: %s)", field, filepath.Ext(name), kindList(kinds))
	}
	return nil
}

// loadAsset reads one file from the branding directory and validates its
// content. The directory is opened as an os.Root, so neither ".." nor a
// symbolic link can reach outside it.
func loadAsset(field, dir, name string, kinds map[string]kind, max int64) (*Asset, error) {
	if err := assetName(field, name, kinds); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Clean(dir))
	if err != nil {
		return nil, fmt.Errorf("%s: open site.branding.directory: %w", field, err)
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %q is not a regular file", field, name)
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s: %q is %d bytes; the limit is %d", field, name, st.Size(), max)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s: %q is larger than %d bytes", field, name, max)
	}
	want := kinds[strings.ToLower(filepath.Ext(name))].contentType
	a, err := validate(data, want)
	if err != nil {
		return nil, fmt.Errorf("%s: %q: %w", field, name, err)
	}
	return a, nil
}

// validate checks that the bytes really are the type the extension claims.
func validate(data []byte, want string) (*Asset, error) {
	a := &Asset{Data: data, ContentType: want}
	switch want {
	case "image/svg+xml":
		w, h, err := validateSVG(data)
		if err != nil {
			return nil, err
		}
		a.Width, a.Height = w, h
	case "image/x-icon":
		if err := validateICO(data); err != nil {
			return nil, err
		}
	default:
		if got := http.DetectContentType(data); got != want {
			return nil, fmt.Errorf("content is %s, not %s", got, want)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("not a valid image: %v", err)
		}
		if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxPixels || cfg.Height > maxPixels {
			return nil, fmt.Errorf("image is %dx%d; each side must be 1 to %d pixels", cfg.Width, cfg.Height, maxPixels)
		}
		// The header alone says nothing about the rest of the file: decode the
		// pixels once so a truncated or corrupt image fails startup instead of
		// being served as a broken logo. The work is bounded by the file-size
		// and dimension limits checked above, and the decoded image is dropped.
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("image data is truncated or corrupt: %v", err)
		}
		if want == "image/gif" {
			// image.Decode reads the first frame only; walk the remaining
			// blocks (without decompressing them) so a cut-off animation is
			// rejected as well.
			if err := gifComplete(data); err != nil {
				return nil, err
			}
		}
		a.Width, a.Height = cfg.Width, cfg.Height
	}
	a.Version = fingerprint(data)
	a.ETag = `"` + a.Version + `"`
	return a, nil
}

// validateICO checks the ICONDIR header (reserved 0, type 1, at least one
// image) and that every directory entry points at data inside the file, which
// rejects truncated icons. The embedded bitmaps are not decoded.
func validateICO(data []byte) error {
	if len(data) < 6 || data[0] != 0 || data[1] != 0 || data[2] != 1 || data[3] != 0 {
		return fmt.Errorf("content is not an ICO image")
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if count == 0 {
		return fmt.Errorf("content is not an ICO image")
	}
	if len(data) < 6+16*count {
		return fmt.Errorf("ICO image is truncated (directory)")
	}
	for i := 0; i < count; i++ {
		e := data[6+16*i:]
		size, off := uint64(binary.LittleEndian.Uint32(e[8:12])), uint64(binary.LittleEndian.Uint32(e[12:16]))
		if size == 0 || off < uint64(6+16*count) || off+size > uint64(len(data)) {
			return fmt.Errorf("ICO image is truncated or corrupt (image %d)", i+1)
		}
	}
	return nil
}

// gifComplete walks the block structure of a GIF (header, color tables,
// extensions, image descriptors and their data sub-blocks) up to the trailer.
// Nothing is decompressed, so the cost is linear in the file size.
func gifComplete(data []byte) error {
	bad := fmt.Errorf("image data is truncated or corrupt: incomplete GIF")
	if len(data) < 13 {
		return bad
	}
	pos := 13
	if data[10]&0x80 != 0 {
		pos += 3 << (data[10]&7 + 1) // global color table
	}
	subBlocks := func() bool {
		for pos < len(data) {
			n := int(data[pos])
			pos += 1 + n
			if n == 0 {
				return pos <= len(data)
			}
		}
		return false
	}
	for pos < len(data) {
		switch data[pos] {
		case 0x3b: // trailer
			return nil
		case 0x21: // extension: label, then sub-blocks
			pos += 2
			if !subBlocks() {
				return bad
			}
		case 0x2c: // image descriptor, optional local color table, LZW code size, data
			if pos+10 > len(data) {
				return bad
			}
			flags := data[pos+9]
			pos += 10
			if flags&0x80 != 0 {
				pos += 3 << (flags&7 + 1)
			}
			pos++ // LZW minimum code size
			if !subBlocks() {
				return bad
			}
		default:
			return bad
		}
	}
	return bad
}

// Elements that can run code, load content or embed other documents.
var forbiddenSVG = map[string]bool{
	"script": true, "foreignobject": true, "iframe": true, "object": true,
	"embed": true, "audio": true, "video": true, "canvas": true,
	"animate": true, "set": true, "animatetransform": true, "animatemotion": true,
}

// validateSVG accepts a well-formed, self-contained SVG document: svg root in
// the SVG namespace, no scripts, event handlers, external references or
// entity declarations. It returns the intrinsic size when the root declares
// one. Browsers additionally treat <img> SVG as inert, and the asset is served
// with a sandboxing Content-Security-Policy.
func validateSVG(data []byte) (int, int, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	var w, h int
	var vbW, vbH float64
	depth, root := 0, false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, fmt.Errorf("not well-formed SVG: %v", err)
		}
		switch t := tok.(type) {
		case xml.ProcInst:
			// Only the XML declaration: <?xml-stylesheet?> names an external
			// stylesheet, and no other instruction belongs in a logo.
			if t.Target != "xml" {
				return 0, 0, fmt.Errorf("SVG processing instruction <?%s?> is not allowed", t.Target)
			}
		case xml.Directive:
			if bytes.Contains(bytes.ToUpper(t), []byte("ENTITY")) {
				return 0, 0, fmt.Errorf("SVG entity declarations are not allowed")
			}
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if !root {
				if name != "svg" || t.Name.Space != "http://www.w3.org/2000/svg" {
					return 0, 0, fmt.Errorf("root element must be <svg xmlns=\"http://www.w3.org/2000/svg\">")
				}
				root = true
				for _, at := range t.Attr {
					switch strings.ToLower(at.Name.Local) {
					case "width":
						w = int(math.Round(length(at.Value)))
					case "height":
						h = int(math.Round(length(at.Value)))
					case "viewbox":
						f := strings.Fields(strings.ReplaceAll(at.Value, ",", " "))
						if len(f) == 4 {
							vbW, _ = strconv.ParseFloat(f[2], 64)
							vbH, _ = strconv.ParseFloat(f[3], 64)
						}
					}
				}
			}
			depth++
			if forbiddenSVG[name] {
				return 0, 0, fmt.Errorf("SVG <%s> elements are not allowed", name)
			}
			for _, at := range t.Attr {
				an := strings.ToLower(at.Name.Local)
				if strings.HasPrefix(an, "on") {
					return 0, 0, fmt.Errorf("SVG event-handler attribute %q is not allowed", an)
				}
				if an == "href" && !strings.HasPrefix(strings.TrimSpace(at.Value), "#") {
					return 0, 0, fmt.Errorf("SVG external references are not allowed (href must be a #fragment)")
				}
				// Any presentation attribute may carry url(); none of them has a
				// reason to name something outside the document.
				if unsafeCSS(at.Value) {
					return 0, 0, fmt.Errorf("SVG attribute %q references external content", an)
				}
			}
		case xml.CharData:
			if unsafeCSS(string(t)) {
				return 0, 0, fmt.Errorf("SVG text or style references external content")
			}
		case xml.EndElement:
			depth--
		}
	}
	if !root || depth != 0 {
		return 0, 0, fmt.Errorf("not a complete SVG document")
	}
	if w <= 0 || h <= 0 {
		w, h = int(math.Round(vbW)), int(math.Round(vbH))
	}
	if w < 1 || h < 1 || w > maxPixels*4 || h > maxPixels*4 {
		w, h = 0, 0 // unknown: the renderer falls back to CSS sizing
	}
	return w, h, nil
}

// unsafeCSS reports css text that loads something: a url() that is not a
// same-document #fragment, an @import, or script-ish constructs.
func unsafeCSS(s string) bool {
	l := strings.ToLower(s)
	// A CSS escape (\75rl, @\69mport) would hide a reference from the checks
	// below, so they look at the unescaped text.
	if strings.Contains(l, "\\") {
		l = cssUnescape(l)
	}
	if strings.Contains(l, "@import") || strings.Contains(l, "javascript:") || strings.Contains(l, "expression(") {
		return true
	}
	for rest := l; ; {
		i := strings.Index(rest, "url(")
		if i < 0 {
			return false
		}
		rest = strings.TrimLeft(rest[i+4:], " '\"")
		if !strings.HasPrefix(rest, "#") {
			return true
		}
	}
}

// cssUnescape resolves CSS backslash escapes: up to six hexadecimal digits
// (with one optional trailing space) name a code point, any other character
// stands for itself.
func cssUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		j := i + 1
		for j < len(s) && j < i+7 && strings.IndexByte("0123456789abcdefABCDEF", s[j]) >= 0 {
			j++
		}
		if j == i+1 {
			b.WriteByte(s[j]) // a literal character
			i = j
			continue
		}
		n, _ := strconv.ParseUint(s[i+1:j], 16, 32)
		b.WriteString(strings.ToLower(string(rune(n))))
		if j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n') {
			j++
		}
		i = j - 1
	}
	return b.String()
}

// length parses the numeric part of an SVG length; percentages and unknown
// units yield 0 (unknown).
func length(v string) float64 {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.TrimSuffix(v, "px")
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return f
}

// builtinFavicon is a small generic network glyph drawn with the theme's
// accent color, so the default installation needs no external or operator
// asset and no second palette.
func builtinFavicon(accent string) *Asset {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32">` +
		`<rect x="2" y="2" width="28" height="28" rx="7" fill="none" stroke="` + accent + `" stroke-width="2.5"/>` +
		`<path d="M9.5 21.5 16 10.5l6.5 11" fill="none" stroke="` + accent + `" stroke-width="2.2" stroke-linejoin="round"/>` +
		`<g fill="` + accent + `"><circle cx="9.5" cy="21.5" r="2.6"/><circle cx="16" cy="10.5" r="2.6"/><circle cx="22.5" cy="21.5" r="2.6"/></g></svg>`
	data := []byte(svg)
	v := fingerprint(data)
	return &Asset{Data: data, ContentType: "image/svg+xml", ETag: `"` + v + `"`, Version: v, Width: 32, Height: 32, Builtin: true}
}
