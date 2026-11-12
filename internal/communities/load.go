package communities

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Fixed resource bounds. They are generous for real-world files (the largest
// NLNOG file is 23 KB and the whole upstream set about 14,500 lines) and exist
// to stop an accidental huge or hostile file from exhausting memory at startup.
const (
	MaxDescriptionBytes = 512
	MaxLineBytes        = 1024
	// WellKnownFile holds definitions that belong to no operator (65535:...).
	WellKnownFile = "well-known.txt"
	maxProblems   = 20
)

// Limits bound what a directory may contain.
type Limits struct {
	MaxFiles       int   // definition files
	MaxFileBytes   int64 // per definition file
	MaxDefinitions int   // across all files
}

// DefaultLimits are used by Load.
var DefaultLimits = Limits{MaxFiles: 4096, MaxFileBytes: 1 << 20, MaxDefinitions: 100000}

// Error lists every problem found while loading, each with its file and line
// where there is one. Nothing is loaded when there is any.
type Error struct {
	Problems []string
	More     int // problems not listed
}

func (e *Error) Error() string {
	var b strings.Builder
	n := len(e.Problems) + e.More
	fmt.Fprintf(&b, "community definitions: %d problem(s):", n)
	for _, p := range e.Problems {
		b.WriteString("\n  ")
		b.WriteString(p)
	}
	if e.More > 0 {
		fmt.Fprintf(&b, "\n  ... and %d more", e.More)
	}
	return b.String()
}

type problems struct {
	list []string
	more int
}

func (p *problems) add(format string, args ...any) {
	if len(p.list) >= maxProblems {
		p.more++
		return
	}
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

func (p *problems) err() error {
	if len(p.list) == 0 {
		return nil
	}
	return &Error{Problems: p.list, More: p.more}
}

// Load reads every definition file in dir with DefaultLimits.
func Load(dir string) (*Set, error) { return LoadDir(dir, DefaultLimits) }

// LoadDir reads the asN.txt files and well-known.txt in dir and compiles them.
// Any problem (missing or unreadable directory or file, bad file name,
// malformed or duplicate definition, exceeded limit) fails the whole load:
// it never returns a partial definition set. An empty directory is valid and
// yields an empty Set.
//
// Files: "asN.txt" (N a canonical ASN) and "well-known.txt" are loaded; any
// other file whose name starts with "as" and ends in ".txt" is an error (it is
// almost certainly a typo); other files are ignored and reported in Stats.
// Files load in a fixed order: well-known first, then ascending ASN.
func LoadDir(dir string, lim Limits) (*Set, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("community definitions: directory is not set")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("community definitions: directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("community definitions: %s is not a directory", dir)
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("community definitions: read directory: %w", err)
	}
	var pr problems
	type file struct {
		name string
		asn  uint64 // 0: well-known
	}
	var files []file
	set := &Set{exact: map[exactKey]*entry{}, patterns: map[bucketKey][]*entry{}}
	for _, de := range des {
		name := de.Name()
		if de.IsDir() {
			continue
		}
		switch {
		case name == WellKnownFile:
			files = append(files, file{name: name})
		case isASFile(name):
			asn, ok := parseASFileName(name)
			if !ok {
				pr.add("%s: invalid file name: expected asN.txt with N a decimal ASN from 1 to 4294967295 without leading zeros (lower case)", name)
				continue
			}
			files = append(files, file{name: name, asn: asn})
		case strings.HasSuffix(name, ".txt"):
			set.stats.Ignored = append(set.stats.Ignored, name)
		}
	}
	if len(files) > lim.MaxFiles {
		pr.add("%s: %d definition files exceed the limit of %d", dir, len(files), lim.MaxFiles)
		return nil, pr.err()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].asn < files[j].asn })
	sort.Strings(set.stats.Ignored)

	seen := map[exactKey]string{} // exact and pattern definitions alike, by canonical text
	order := 0
	for _, f := range files {
		data, err := readBounded(dir+string(os.PathSeparator)+f.name, lim.MaxFileBytes)
		if err != nil {
			pr.add("%s: %v", f.name, err)
			continue
		}
		set.stats.Files++
		source := "Well-known"
		if f.asn != 0 {
			source = "AS" + strconv.FormatUint(f.asn, 10)
		}
		parseFile(set, &pr, seen, &order, lim, f.name, source, f.asn, data)
		if len(set.defs) > lim.MaxDefinitions {
			pr.add("%s: more than %d definitions in total", f.name, lim.MaxDefinitions)
			return nil, pr.err()
		}
	}
	if err := pr.err(); err != nil {
		return nil, err
	}
	for k, list := range set.patterns {
		sort.Slice(list, func(i, j int) bool { return list[i].before(list[j]) })
		set.patterns[k] = list
	}
	return set, nil
}

func isASFile(name string) bool {
	l := strings.ToLower(name)
	return strings.HasPrefix(l, "as") && strings.HasSuffix(l, ".txt")
}

func parseASFileName(name string) (uint64, bool) {
	if !strings.HasPrefix(name, "as") || !strings.HasSuffix(name, ".txt") {
		return 0, false
	}
	digits := name[2 : len(name)-4]
	n, canonical, ok := parseDecimal(digits, maxU32)
	return n, ok && canonical && n != 0
}

// readBounded reads a regular file (symbolic links are followed) of at most
// max bytes.
func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if st.Size() > max {
		return nil, fmt.Errorf("file is %d bytes, the limit is %d", st.Size(), max)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("file is larger than the limit of %d bytes", max)
	}
	return data, nil
}

func parseFile(set *Set, pr *problems, seen map[exactKey]string, order *int, lim Limits, name, source string, asn uint64, data []byte) {
	if !utf8.Valid(data) {
		pr.add("%s: not valid UTF-8", name)
		return
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	asnText := strconv.FormatUint(asn, 10)
	for i, raw := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' {
			continue
		}
		if len(line) > MaxLineBytes {
			pr.add("%s:%d: line is %d bytes, the limit is %d", name, lineNo, len(line), MaxLineBytes)
			continue
		}
		if err := checkText(line); err != nil {
			pr.add("%s:%d: %v", name, lineNo, err)
			continue
		}
		if asn != 0 {
			line = strings.ReplaceAll(line, "<ASN>", asnText)
		}
		pl, err := parseLine(line, *order)
		if err != nil {
			pr.add("%s:%d: %v", name, lineNo, err)
			continue
		}
		k := exactKey{pl.e.kind, pl.key}
		if first, dup := seen[k]; dup {
			pr.add("%s:%d: duplicate definition of %q (first defined at %s)", name, lineNo, pl.key, first)
			continue
		}
		seen[k] = name + ":" + strconv.Itoa(lineNo)
		*order++
		set.register(pl, name, source, lineNo)
		if len(set.defs) > lim.MaxDefinitions {
			return
		}
	}
}

func (s *Set) register(pl parsedLine, file, source string, line int) {
	e := pl.e
	s.defs = append(s.defs, Definition{Kind: e.kind, Key: pl.key, Description: e.desc, File: file, Source: source, Line: line, Exact: pl.exact})
	s.stats.Definitions++
	switch e.kind {
	case Standard:
		s.stats.Standard++
	case Large:
		s.stats.Large++
	case Extended:
		s.stats.Extended++
	}
	if pl.exact {
		s.stats.Exact++
		s.exact[exactKey{e.kind, pl.key}] = e
		return
	}
	s.stats.Patterns++
	bk := bucketKey{kind: e.kind, label: e.label}
	if e.comps[0].kind == compLit {
		bk.first = e.comps[0].lit
	}
	s.patterns[bk] = append(s.patterns[bk], e)
}
