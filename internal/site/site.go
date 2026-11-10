// Package site holds the deployment identity of a looking glass: who runs it
// and how it presents itself. It is operator data (YAML), distinct from the
// software identity (Hextet LG). The package validates the configuration,
// reads the optional logo and favicon once at startup, and produces an
// immutable Site that the web layer renders. It has no web or BGP
// dependencies, and nothing in it is ever used to build a backend query.
package site

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Generic defaults for an installation without a site section. They name no
// operator and no network.
const (
	DefaultName     = "BGP Looking Glass"
	DefaultSubtitle = "Network Routing Information"
	DefaultFooter   = "Powered by Hextet LG"
)

// Bounds on operator text. They keep the compact header and footer intact and
// bound what reaches the document title.
const (
	MaxNameRunes     = 64
	MaxSubtitleRunes = 96
	MaxOperatorRunes = 64
	MaxFooterRunes   = 160
	maxWebsiteBytes  = 512
	maxContactBytes  = 254
)

// ASN is an operator AS number. In YAML it is a number (64500) or the usual
// notation ("AS64500"); zero means unset.
type ASN uint32

// UnmarshalYAML accepts 64500, "64500" and "AS64500".
func (a *ASN) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if len(s) > 2 && (s[0] == 'A' || s[0] == 'a') && (s[1] == 'S' || s[1] == 's') {
		s = s[2:]
	}
	if s == "" {
		*a = 0
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return fmt.Errorf("site.asn must be an AS number such as 64500 or AS64500")
	}
	*a = ASN(n)
	return nil
}

// Branding configures the optional local assets and footer text.
type Branding struct {
	// Directory holds the asset files named by Logo and Favicon. It is read
	// once at startup; nothing outside it can be served. In Docker, mount it
	// read-only (for example at /etc/hextet-lg/branding).
	Directory string `yaml:"directory"`
	// Logo and Favicon are plain file names inside Directory (no path).
	Logo    string `yaml:"logo"`
	Favicon string `yaml:"favicon"`
	// Footer is plain text. nil selects DefaultFooter; an explicit empty
	// string removes it. It is never interpreted as HTML or Markdown.
	Footer *string `yaml:"footer"`
}

// Config is the optional `site` section. Every field is optional.
type Config struct {
	// Name is the site name: header, browser titles. When empty the legacy
	// server.title is used, then DefaultName.
	Name string `yaml:"name"`
	// Subtitle is secondary header text. nil selects DefaultSubtitle; an
	// explicit empty string removes it.
	Subtitle *string `yaml:"subtitle"`
	// Operator, ASN, Website and Contact describe who runs this looking
	// glass. They are display metadata only: the operator ASN never reaches a
	// query, filter, backend identity or result processing.
	Operator string   `yaml:"operator"`
	ASN      ASN      `yaml:"asn"`
	Website  string   `yaml:"website"`
	Contact  string   `yaml:"contact"`
	Branding Branding `yaml:"branding"`
}

// Part is one footer item: plain text, optionally a link.
type Part struct {
	Text string
	// URL is an http(s) or mailto: link target, or empty.
	URL string
	// External marks an http(s) link (rendered with rel="noopener noreferrer").
	External bool
}

// Site is the validated, immutable deployment identity.
type Site struct {
	Name     string
	Subtitle string
	Operator string
	ASN      uint32
	Website  string
	Contact  string
	// Mark is the letter shown in the header when no logo is configured.
	Mark string
	// Footer lists the operator items in display order: operator (linked to
	// the website), AS number, contact, footer text. Absent fields are absent
	// here, so a renderer never produces empty labels or stray separators.
	Footer []Part
	// Logo is nil unless configured. Favicon is never nil: it is the
	// operator's file or the built-in generic icon.
	Logo, Favicon *Asset
	// Warnings are nonfatal startup notes, in a fixed order.
	Warnings []string
}

// Validate checks every setting that needs no file access. Compile repeats it.
func Validate(c Config) error {
	if _, err := text("site.name", c.Name, MaxNameRunes); err != nil {
		return err
	}
	if c.Subtitle != nil {
		if _, err := text("site.subtitle", *c.Subtitle, MaxSubtitleRunes); err != nil {
			return err
		}
	}
	if _, err := text("site.operator", c.Operator, MaxOperatorRunes); err != nil {
		return err
	}
	if c.Branding.Footer != nil {
		if _, err := text("site.branding.footer", *c.Branding.Footer, MaxFooterRunes); err != nil {
			return err
		}
	}
	if err := validASN(c.ASN); err != nil {
		return err
	}
	if _, _, err := website(c.Website); err != nil {
		return err
	}
	if _, err := contact(c.Contact); err != nil {
		return err
	}
	b := c.Branding
	if (b.Logo != "" || b.Favicon != "") && strings.TrimSpace(b.Directory) == "" {
		return fmt.Errorf("site.branding.directory is required when site.branding.logo or site.branding.favicon is set")
	}
	if b.Logo != "" {
		if err := assetName("site.branding.logo", b.Logo, logoKinds); err != nil {
			return err
		}
	}
	if b.Favicon != "" {
		if err := assetName("site.branding.favicon", b.Favicon, faviconKinds); err != nil {
			return err
		}
	}
	return nil
}

// Compile validates the configuration, reads the assets and returns the Site.
// legacyTitle is the old server.title; it names the site only when site.name
// is not set. accent is the theme's accent color, used for the built-in icon.
func Compile(c Config, legacyTitle, accent string) (*Site, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	s := &Site{}
	s.Name, _ = text("site.name", c.Name, MaxNameRunes)
	legacy := sanitize(legacyTitle, MaxNameRunes)
	switch {
	case s.Name != "" && legacy != "" && legacy != s.Name:
		s.Warnings = append(s.Warnings, "server.title is ignored because site.name is set; remove server.title")
	case s.Name == "":
		s.Name = legacy
	}
	if s.Name == "" {
		s.Name = DefaultName
	}
	s.Subtitle = DefaultSubtitle
	if c.Subtitle != nil {
		s.Subtitle, _ = text("site.subtitle", *c.Subtitle, MaxSubtitleRunes)
	}
	s.Operator, _ = text("site.operator", c.Operator, MaxOperatorRunes)
	s.ASN = uint32(c.ASN)
	s.Website, _, _ = website(c.Website)
	s.Contact, _ = contact(c.Contact)
	s.Mark = mark(s.Name)

	footer := DefaultFooter
	if c.Branding.Footer != nil {
		footer, _ = text("site.branding.footer", *c.Branding.Footer, MaxFooterRunes)
	}
	s.Footer = footerParts(s, footer)

	b := c.Branding
	var err error
	if b.Logo != "" {
		if s.Logo, err = loadAsset("site.branding.logo", b.Directory, b.Logo, logoKinds, maxLogoBytes); err != nil {
			return nil, err
		}
	}
	if b.Favicon != "" {
		if s.Favicon, err = loadAsset("site.branding.favicon", b.Directory, b.Favicon, faviconKinds, maxFaviconBytes); err != nil {
			return nil, err
		}
	} else {
		s.Favicon = builtinFavicon(accent)
	}
	return s, nil
}

// ASNLabel is "AS<n>" or "" when no ASN is configured.
func (s *Site) ASNLabel() string {
	if s.ASN == 0 {
		return ""
	}
	return "AS" + strconv.FormatUint(uint64(s.ASN), 10)
}

func footerParts(s *Site, footer string) []Part {
	var parts []Part
	switch {
	case s.Operator != "":
		p := Part{Text: s.Operator}
		if s.Website != "" {
			p.URL, p.External = s.Website, true
		}
		parts = append(parts, p)
	case s.Website != "":
		// A website without an operator name is shown by its host.
		u, _ := url.Parse(s.Website)
		parts = append(parts, Part{Text: u.Host, URL: s.Website, External: true})
	}
	if l := s.ASNLabel(); l != "" {
		parts = append(parts, Part{Text: l})
	}
	if s.Contact != "" {
		parts = append(parts, Part{Text: s.Contact, URL: "mailto:" + s.Contact})
	}
	if footer != "" {
		parts = append(parts, Part{Text: footer})
	}
	return parts
}

// mark is the first letter or digit of the name, upper-cased.
func mark(name string) string {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(unicode.ToUpper(r))
		}
	}
	return "L"
}

// text trims and validates operator text: valid UTF-8, no control or
// non-printing characters, bounded length. Markup is not special; the value is
// plain text and the renderer escapes it.
func text(field, v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if !utf8.ValidString(v) {
		return "", fmt.Errorf("%s is not valid UTF-8", field)
	}
	for _, r := range v {
		if !unicode.IsPrint(r) && r != ' ' {
			return "", fmt.Errorf("%s must not contain control or non-printing characters", field)
		}
	}
	if n := utf8.RuneCountInString(v); n > max {
		return "", fmt.Errorf("%s is %d characters; the limit is %d", field, n, max)
	}
	return v, nil
}

// sanitize is the lenient form used for the legacy server.title: existing
// configurations must keep loading, so it strips instead of rejecting.
func sanitize(v string, max int) string {
	v = strings.ToValidUTF8(v, "")
	v = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || r == ' ' {
			return r
		}
		return -1
	}, v)
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) > max {
		v = strings.TrimSpace(string([]rune(v)[:max]))
	}
	return v
}

func validASN(a ASN) error {
	switch a {
	case 23456, 65535, 4294967295: // AS_TRANS and the last 16/32-bit values
		return fmt.Errorf("site.asn %d is a reserved AS number", a)
	}
	return nil
}

// website validates an absolute http(s) URL and returns its canonical form.
func website(v string) (string, bool, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false, nil
	}
	if len(v) > maxWebsiteBytes {
		return "", false, fmt.Errorf("site.website is longer than %d bytes", maxWebsiteBytes)
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f || r > unicode.MaxASCII {
			return "", false, fmt.Errorf("site.website must be an ASCII URL without spaces or control characters (use punycode for international host names)")
		}
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", false, fmt.Errorf("site.website is not a valid URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", false, fmt.Errorf("site.website must be an absolute http or https URL")
	}
	if u.Hostname() == "" || u.Opaque != "" {
		return "", false, fmt.Errorf("site.website must be an absolute http or https URL with a host")
	}
	if u.User != nil {
		return "", false, fmt.Errorf("site.website must not contain credentials")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String(), true, nil
}

// contact validates a conservative single email address: no display name,
// quoting, comments or lists.
func contact(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	bad := fmt.Errorf("site.contact must be a plain email address such as noc@example.net")
	if len(v) > maxContactBytes || strings.Count(v, "@") != 1 {
		return "", bad
	}
	local, domain, _ := strings.Cut(v, "@")
	if local == "" || len(local) > 64 || local[0] == '.' || local[len(local)-1] == '.' || strings.Contains(local, "..") {
		return "", bad
	}
	for i := 0; i < len(local); i++ {
		c := local[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(".!#$%&'*+/=?^_`{|}~-", c) >= 0) {
			return "", bad
		}
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 || len(domain) > 253 {
		return "", bad
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", bad
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return "", bad
			}
		}
	}
	return v, nil
}

func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
