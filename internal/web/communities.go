package web

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Implementation statuses for the AS395089 Community Explorer. They describe
// presence in the RouterOS routing configuration, not verified behaviour.
const (
	StatusImplemented = "Implemented"
	StatusDocumented  = "Documented/planned"
	StatusUnresolved  = "Unresolved"
)

// Evidence kinds, from strongest to weakest.
const (
	EvidenceFilter  = "routing filter"
	EvidenceList    = "community list"
	EvidencePattern = "configured pattern"
)

// Community is one entry of the human-editable reference.
type Community struct {
	Value, Name, Description, Category, Notes string
	// Status and Evidence are derived from the RouterOS evidence section of the
	// YAML (see classifyCommunities) and are not stored per entry, except that
	// an entry may set `status: unresolved` to record insufficient evidence.
	Status        string
	Evidence      string
	EvidenceNote  string
	statusDeclare string
}

// routerOSEvidence lists what the RouterOS routing configuration contains.
type routerOSEvidence struct {
	Source string `yaml:"source"`
	// Lists are communities present in explicit community lists.
	Lists []string `yaml:"lists"`
	// Filters maps communities that routing filters act on to that behaviour.
	Filters map[string]string `yaml:"filters"`
	// Patterns are regular expressions used by community matchers.
	Patterns []string `yaml:"patterns"`
}

type communityFile struct {
	Evidence    routerOSEvidence `yaml:"routeros"`
	Communities []struct {
		Value       string `yaml:"value"`
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Category    string `yaml:"category"`
		Notes       string `yaml:"notes"`
		Status      string `yaml:"status"`
	} `yaml:"communities"`
}

// loadCommunities parses the embedded reference and classifies every entry.
func loadCommunities(data []byte) ([]Community, error) {
	var f communityFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode communities: %w", err)
	}
	c, err := newClassifier(f.Evidence)
	if err != nil {
		return nil, err
	}
	var out []Community
	seen := map[string]bool{}
	for _, e := range f.Communities {
		if e.Value == "" || seen[e.Value] {
			return nil, fmt.Errorf("communities: empty or duplicate value %q", e.Value)
		}
		seen[e.Value] = true
		com := Community{Value: e.Value, Name: e.Name, Description: e.Description, Category: e.Category, Notes: e.Notes, statusDeclare: e.Status}
		c.classify(&com)
		out = append(out, com)
	}
	return out, nil
}

type classifier struct {
	lists    map[string]bool
	filters  map[string]string
	patterns []*regexp.Regexp
	sources  []string
}

func newClassifier(ev routerOSEvidence) (*classifier, error) {
	c := &classifier{lists: map[string]bool{}, filters: ev.Filters}
	for _, v := range ev.Lists {
		c.lists[v] = true
	}
	for _, p := range ev.Patterns {
		// RouterOS community regular expressions match the whole community string.
		re, err := regexp.Compile("^(?:" + p + ")$")
		if err != nil {
			return nil, fmt.Errorf("communities: bad pattern %q: %w", p, err)
		}
		c.patterns = append(c.patterns, re)
		c.sources = append(c.sources, p)
	}
	return c, nil
}

// classify applies the project's rules:
//
//	Implemented         present in the RouterOS configuration: an explicit
//	                    community list, a routing filter, or a configured
//	                    matching pattern.
//	Documented/planned  described in the README but absent from the configuration.
//	Unresolved          insufficient evidence to classify reliably (declared in YAML).
//
// Implemented never claims that the intended routing behaviour was verified.
func (c *classifier) classify(com *Community) {
	if strings.EqualFold(com.statusDeclare, "unresolved") {
		com.Status = StatusUnresolved
		return
	}
	if behaviour, ok := c.filters[com.Value]; ok {
		com.Status, com.Evidence, com.EvidenceNote = StatusImplemented, EvidenceFilter, behaviour
		return
	}
	if c.lists[com.Value] {
		com.Status, com.Evidence = StatusImplemented, EvidenceList
		return
	}
	for i, re := range c.patterns {
		if re.MatchString(com.Value) {
			com.Status, com.Evidence, com.EvidenceNote = StatusImplemented, EvidencePattern, c.sources[i]
			return
		}
	}
	com.Status = StatusDocumented
}
