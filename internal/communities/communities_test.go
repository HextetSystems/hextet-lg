package communities

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadFiles writes the files (name -> content) to a fresh directory and loads it.
func loadFiles(t testing.TB, files map[string]string) (*Set, error) {
	t.Helper()
	return loadFilesLimits(t, files, DefaultLimits)
}

func loadFilesLimits(t testing.TB, files map[string]string, lim Limits) (*Set, error) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return LoadDir(dir, lim)
}

func mustLoad(t testing.TB, files map[string]string) *Set {
	t.Helper()
	s, err := loadFiles(t, files)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type lookup struct {
	kind  Kind
	value string
	want  string // "" means no description
}

func check(t *testing.T, s *Set, cases []lookup) {
	t.Helper()
	for _, c := range cases {
		got, ok := s.Lookup(c.kind, c.value)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("Lookup(%s, %q) = %q, %v; want %q", c.kind, c.value, got, ok, c.want)
		}
	}
}

func TestExactMatches(t *testing.T) {
	s := mustLoad(t, map[string]string{"as64500.txt": `
# comment
64500:100,Learned from customer
64500:200,Learned from peer
64500:1:100,Example large community
soo 64500:0,Example site of origin
rt 64500:1,Example route target
`})
	check(t, s, []lookup{
		{Standard, "64500:100", "Learned from customer"},
		{Standard, "64500:200", "Learned from peer"},
		{Standard, "64500:300", ""},
		{Large, "64500:1:100", "Example large community"},
		{Large, "64500:1:101", ""},
		{Extended, "soo 64500:0", "Example site of origin"},
		{Extended, "rt 64500:1", "Example route target"},
		{Extended, "rt 64500:2", ""},
		// Kinds are never conflated.
		{Large, "64500:100", ""},
		{Standard, "64500:1:100", ""},
		{Standard, "rt 64500:1", ""},
		{Extended, "64500:100", ""},
		{Extended, "soo 64500:1", ""},
	})
	st := s.Stats()
	if st.Definitions != 5 || st.Exact != 5 || st.Patterns != 0 || st.Standard != 2 || st.Large != 1 || st.Extended != 2 || st.Files != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestRanges(t *testing.T) {
	s := mustLoad(t, map[string]string{"as64500.txt": `
64500:400-499,Regional policy
65001-65003:0,Prepend to all peers
64500:7:10-20,Large range
rt 65511-65513:nnn,Peer prepend $0
`})
	check(t, s, []lookup{
		{Standard, "64500:400", "Regional policy"},
		{Standard, "64500:450", "Regional policy"},
		{Standard, "64500:499", "Regional policy"},
		{Standard, "64500:399", ""},
		{Standard, "64500:500", ""},
		{Standard, "65001:0", "Prepend to all peers"},
		{Standard, "65003:0", "Prepend to all peers"},
		{Standard, "65004:0", ""},
		{Large, "64500:7:10", "Large range"},
		{Large, "64500:7:21", ""},
		{Extended, "rt 65512:77", "Peer prepend 77"},
		{Extended, "rt 65514:77", ""},
	})
}

func TestSingleDigitWildcard(t *testing.T) {
	s := mustLoad(t, map[string]string{"as65535.txt": `
65535:x0,Tens
64500:1x,Teens $0
64500:2xxx,Local pref $0$1$2
`})
	check(t, s, []lookup{
		{Standard, "65535:10", "Tens"},
		{Standard, "65535:90", "Tens"},
		{Standard, "65535:0", ""},   // x is exactly one digit: "0" has one digit, x0 needs two
		{Standard, "65535:100", ""}, // upstream's prefix-only regex would match this
		{Standard, "65535:15", ""},
		{Standard, "64500:19", "Teens 9"},
		{Standard, "64500:1", ""},
		{Standard, "64500:2345", "Local pref 345"},
		{Standard, "64500:2000", "Local pref 000"},
		{Standard, "64500:23456", ""},
		{Standard, "64500:234", ""},
	})
}

func TestArbitraryNumericWildcard(t *testing.T) {
	s := mustLoad(t, map[string]string{"as10965.txt": `
65535:nnn,Blackhole to peer $0
64500:1nnn,Prefixed $0
64500:nnn:0,Do not announce to AS$0
`})
	// nnn is any number of digits, not a fixed three. The definition lives in
	// as10965.txt but matches by the community value, not the file ASN.
	check(t, s, []lookup{
		{Standard, "65535:0", "Blackhole to peer 0"},
		{Standard, "65535:7", "Blackhole to peer 7"},
		{Standard, "65535:65000", "Blackhole to peer 65000"},
		{Standard, "64500:1", ""}, // 1nnn needs "1" followed by at least one digit
		{Standard, "64500:19", "Prefixed 9"},
		{Standard, "64500:12345", "Prefixed 2345"},
		{Large, "64500:4200000000:0", "Do not announce to AS4200000000"},
		{Large, "64500:12:0", "Do not announce to AS12"},
		{Large, "64500:12:1", ""},
	})
}

func TestMultipleCapturesAndSubstitutions(t *testing.T) {
	s := mustLoad(t, map[string]string{"as64500.txt": `
64500:x:nnn,Prepend $0 times to AS$1
64500:0:nnn,Do not announce to AS$0
64500:nnn:x,Reordered: $1 then $0, $1 again
203038:752113nnn:nnn,Region AS$1 code $0
20473:0:3xxxxxx1xx,region $0$1$2 country $3$4$5 location $6$7
`})
	check(t, s, []lookup{
		{Large, "64500:3:4200000000", "Prepend 3 times to AS4200000000"},
		{Large, "64500:0:123", "Do not announce to AS123"}, // 0 is exact-ish: fewer values than x:nnn
		{Large, "64500:12345:7", "Reordered: 7 then 12345, 7 again"},
		{Large, "203038:752113005:99", "Region AS99 code 005"},
		{Large, "20473:0:3123456178", "region 123 country 456 location 78"},
	})
}

func TestNnnBacktrackingMatchesAnchoredRegexp(t *testing.T) {
	// nnn is greedy with backtracking, like ^64500:(\d+)5(\d)$ would be.
	s := mustLoad(t, map[string]string{"as64500.txt": "64500:nnn5x,A $0/$1\n"})
	check(t, s, []lookup{
		{Standard, "64500:12354", "A 123/4"},
		{Standard, "64500:5555", "A 55/5"},
		{Standard, "64500:12345", ""}, // no 5 followed by exactly one more digit
		{Standard, "64500:55", ""},    // nnn needs at least one digit
		{Standard, "64500:1235", ""},
	})
}

func TestDescriptionsWithCommasAndSpacing(t *testing.T) {
	s := mustLoad(t, map[string]string{"as64500.txt": "\xef\xbb\xbf64500:1,  Learned from A, B, and C  \r\n64500:2,Plain\r\n"})
	check(t, s, []lookup{
		{Standard, "64500:1", "Learned from A, B, and C"},
		{Standard, "64500:2", "Plain"},
	})
}

func TestDollarInExactDescriptionIsLiteral(t *testing.T) {
	// Substitution applies only to wildcard definitions; an exact entry keeps "$0".
	s := mustLoad(t, map[string]string{"as64500.txt": "64500:1,Costs $0 or $9\n"})
	check(t, s, []lookup{{Standard, "64500:1", "Costs $0 or $9"}})
}

func TestWellKnownNamesAndNormalization(t *testing.T) {
	s := mustLoad(t, map[string]string{
		"well-known.txt": "65535:0,Graceful shutdown\n65535:666,Blackhole\n65535:65281,NO_EXPORT: do not export\n65535:65284,NOPEER\n",
		"as64500.txt":    "64500:7,Seven\nsoo 64500:1,SOO\n",
	})
	check(t, s, []lookup{
		{Standard, "GRACEFUL_SHUTDOWN", "Graceful shutdown"},
		{Standard, "BLACKHOLE", "Blackhole"},
		{Standard, "NO_EXPORT", "NO_EXPORT: do not export"},
		{Standard, "NO_PEER", "NOPEER"},
		{Standard, "NO_ADVERTISE", ""}, // no definition, name still understood
		{Standard, "NOT_A_NAME", ""},
		{Standard, " 64500:7 ", "Seven"},
		{Standard, "064500:07", "Seven"}, // leading zeros normalize
		{Extended, "SOO 64500:1", "SOO"},
		{Extended, "soo  64500:01", "SOO"},
		{Standard, "64500:7:", ""},
		{Standard, "64500", ""},
		{Standard, "", ""},
		{Standard, "64500:65536", ""},
		{Large, "1:2:4294967296", ""},
		{Extended, "soo", ""},
		{Extended, "soo 64500:1:2", ""},
		{Extended, "so_o 64500:1", ""},
	})
}

func TestExtendedForms(t *testing.T) {
	s := mustLoad(t, map[string]string{"as65535.txt": `
soo 65535:0,Example site of origin
soo 65535:nnn,Example site of origin $0
rt 65511-65513:nnn,Peer prepend
rt 64500,Single number form
`})
	check(t, s, []lookup{
		{Extended, "soo 65535:0", "Example site of origin"},
		{Extended, "soo 65535:42", "Example site of origin 42"},
		{Extended, "rt 65512:9", "Peer prepend"},
		{Extended, "rt 64500", "Single number form"},
		{Extended, "rt 64501", ""},
		{Extended, "odi 65535:42", ""},
	})
	// Equivalent representations of one definition are the same definition.
	if _, err := loadFiles(t, map[string]string{"as1.txt": "soo 1:2,A\nSOO 1:2,B\n"}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("case-variant duplicate accepted: %v", err)
	}
}

func TestPrecedence(t *testing.T) {
	s := mustLoad(t, map[string]string{"as64500.txt": `
64500:nnn,any
64500:4xx,four hundreds
64500:40x,forties
64500:400-409,range
64500:405,exact
64500:5xx,five hundreds
64500:5x0,five x zero
`})
	check(t, s, []lookup{
		{Standard, "64500:405", "exact"},         // an exact definition beats every pattern
		{Standard, "64500:402", "forties"},       // 40x and 400-409 can match 10 values each: the earlier definition wins
		{Standard, "64500:499", "four hundreds"}, // 100 values beat the unconstrained nnn
		{Standard, "64500:123", "any"},
		{Standard, "64500:510", "five x zero"}, // 10 values beat 100
		{Standard, "64500:511", "five hundreds"},
	})
	// A range is more specific than nnn even when defined later.
	s = mustLoad(t, map[string]string{"as1.txt": "1:nnn,any\n1:0-100,range\n"})
	check(t, s, []lookup{{Standard, "1:50", "range"}, {Standard, "1:500", "any"}})
}

func TestPrecedenceIsDeterministic(t *testing.T) {
	// 65000:1x and 65000:x1 can each match 10 values, so file order decides:
	// ascending ASN (numerically), then line. Repeat to catch map-order effects.
	files := map[string]string{
		"as64501.txt": "65000:x1,B-second-file\n",
		"as64500.txt": "65000:1x,A-first-file\n",
	}
	for i := 0; i < 50; i++ {
		s := mustLoad(t, files)
		check(t, s, []lookup{{Standard, "65000:11", "A-first-file"}, {Standard, "65000:21", "B-second-file"}, {Standard, "65000:15", "A-first-file"}})
	}
	// Line order breaks the tie inside one file.
	s := mustLoad(t, map[string]string{"as64500.txt": "65000:x1,second-defined-first\n65000:1x,first-defined-second\n"})
	check(t, s, []lookup{{Standard, "65000:11", "second-defined-first"}})
	// Numeric, not lexicographic, file order: as9 comes before as10.
	s = mustLoad(t, map[string]string{"as10.txt": "65000:x1,ten\n", "as9.txt": "65000:1x,nine\n"})
	check(t, s, []lookup{{Standard, "65000:11", "nine"}})
	// well-known.txt is first of all.
	s = mustLoad(t, map[string]string{"as1.txt": "65535:x1,as1\n", "well-known.txt": "65535:1x,well-known\n"})
	check(t, s, []lookup{{Standard, "65535:11", "well-known"}})
}

func TestDuplicateDefinitions(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"exact in one file":      {"as1.txt": "1:1,a\n1:1,b\n"},
		"exact across files":     {"as1.txt": "1:1,a\n", "as2.txt": "1:1,b\n"},
		"pattern":                {"as1.txt": "1:x,a\n1:X,b\n"},
		"range":                  {"as1.txt": "1:1-5,a\n1:1-5,b\n"},
		"well-known and as file": {"well-known.txt": "65535:666,a\n", "as65535.txt": "65535:666,b\n"},
	} {
		_, err := loadFiles(t, files)
		if err == nil || !strings.Contains(err.Error(), "duplicate definition") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The same text in different kinds is not a duplicate.
	mustLoad(t, map[string]string{"as1.txt": "1:2,std\n1:2:3,large\nrt 1:2,ext\n"})
}

func TestInvalidSyntax(t *testing.T) {
	cases := []struct{ line, want string }{
		{"just text", "missing ','"},
		{",desc", "empty community"},
		{"64500:1,", "empty description"},
		{"64500,x", "unsupported community"},
		{"1:2:3:4,x", "unsupported community"},
		{"a b c,x", "unsupported community"},
		{"64500:ccc,x", "unsupported character"},
		{"64500:nn,x", "unsupported syntax"},
		{"64500:nnnn,x", "unsupported syntax"}, // nnn followed by a stray n
		{"64500:nnnnnn,x", "adjacent"},
		{"64500:1-,x", "unsupported range"},
		{"64500:-5,x", "unsupported range"},
		{"64500:1-2-3,x", "unsupported range"},
		{"64500:5-1,x", "start is greater than end"},
		{"64500:4x-50,x", "unsupported range"},
		{"64500:65536,x", "not a number up to 65535"},
		{"64500:1-65536,x", "unsupported range"},
		{"65536:1,x", "not a number up to 65535"},
		{"1:2:4294967296,x", "not a number"},
		{"64500:007,x", "leading zeros"},
		{"64500:01-5,x", "leading zeros"},
		{"64500:xxxxxxxxxxx,x", "can never match"},
		{"64500:xxxxxxxxxxxxxxxxx,x", "longer than"},
		{"64500:7xxxxx,x", "smallest value exceeds"},
		{"64500:xxxxxxxxxxnnn,x", "can never match"},
		{"64500::1,x", "empty component"},
		{"rt,x", "unsupported community"},
		{"rt 1:2:3,x", "unsupported community"},
		{"RT_X 1:2,x", "unsupported extended community label"},
		{"x y z 1:2,x", "unsupported community"},
		{"64500:x,Uses $1", "refers to $1"},
		{"64500:1-5,Uses $0", "no wildcard captures"},
		{"64500:nnn,Uses $0 and $1", "only 1 capture"},
		{"64500:nnn:nnn:nnn,x", "unsupported community"},
		{"1:nnn:nnn,ok", ""},
		{"1:nnn:xnnnx,ok", ""},
		{"nnn:nnn:nnn,ok", ""},
		{"nnn:nnn:nnn nnn,bad", "unsupported extended community label"},
		{"64500:1,bad \x01 control", "control"},
		{"64500:1," + strings.Repeat("a", MaxDescriptionBytes+1), "description is"},
	}
	for _, c := range cases {
		_, err := loadFiles(t, map[string]string{"as64500.txt": "# header\n\n" + c.line + "\n"})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%q: unexpected error %v", c.line, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%q: error %v, want %q", c.line, err, c.want)
		case c.want != "" && !strings.Contains(err.Error(), "as64500.txt:3:"):
			t.Errorf("%q: error lacks file and line: %v", c.line, err)
		}
	}
	if _, err := loadFiles(t, map[string]string{"as1.txt": "1:1,\xff\xfe\n"}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("invalid UTF-8: %v", err)
	}
	long := "1:1," + strings.Repeat("a", MaxLineBytes) + "\n"
	if _, err := loadFiles(t, map[string]string{"as1.txt": long}); err == nil || !strings.Contains(err.Error(), "as1.txt:1:") {
		t.Errorf("long line: %v", err)
	}
}

func TestNumericBoundaries(t *testing.T) {
	s := mustLoad(t, map[string]string{"as1.txt": "0:0,zero\n65535:65535,max\n0:0:0,lzero\n4294967295:4294967295:4294967295,lmax\n1:0-65535,full\n1:2:0-4294967295,lfull\n"})
	check(t, s, []lookup{
		{Standard, "0:0", "zero"}, {Standard, "65535:65535", "max"}, {Standard, "1:65535", "full"},
		{Large, "0:0:0", "lzero"}, {Large, "4294967295:4294967295:4294967295", "lmax"}, {Large, "1:2:4294967295", "lfull"},
	})
}

func TestLoadErrors(t *testing.T) {
	good := "64500:1,ok\n"
	for name, files := range map[string]map[string]string{
		"as0":              {"as0.txt": good},
		"leading zero":     {"as064500.txt": good},
		"non numeric":      {"asx.txt": good},
		"suffix":           {"as64500b.txt": good},
		"upper case":       {"AS64500.txt": good},
		"asn too large":    {"as4294967296.txt": good},
		"empty asn":        {"as.txt": good},
		"malformed second": {"as64500.txt": good, "as64501.txt": "bogus\n"},
	} {
		if _, err := loadFiles(t, files); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	// Other .txt files and non-.txt files are ignored, and reported.
	s := mustLoad(t, map[string]string{"as64500.txt": good, "src_decix.txt": "garbage\n", "README.md": "garbage", "notes.txt": "x"})
	if got := s.Stats().Ignored; len(got) != 2 || got[0] != "notes.txt" || got[1] != "src_decix.txt" {
		t.Fatalf("ignored %v", got)
	}
	if s.Stats().Files != 1 || s.Len() != 1 {
		t.Fatalf("%+v", s.Stats())
	}
}

func TestErrorsAreAggregatedAndBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "bad%d,x\n", i)
	}
	_, err := loadFiles(t, map[string]string{"as1.txt": b.String()})
	if err == nil {
		t.Fatal("accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "as1.txt:1:") || !strings.Contains(msg, "40 problem") || !strings.Contains(msg, "and 20 more") || strings.Count(msg, "as1.txt:") != maxProblems {
		t.Fatalf("%s", msg)
	}
}

func TestDirectoryProblems(t *testing.T) {
	if _, err := LoadDir(filepath.Join(t.TempDir(), "missing"), DefaultLimits); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("missing: %v", err)
	}
	if _, err := LoadDir("", DefaultLimits); err == nil {
		t.Error("empty path accepted")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(file, DefaultLimits); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file as directory: %v", err)
	}
	// An empty directory is a valid, empty definition set.
	s := mustLoad(t, nil)
	if s.Len() != 0 {
		t.Fatal("not empty")
	}
	check(t, s, []lookup{{Standard, "64500:1", ""}})
	// A directory named like a definition file is skipped.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "as1.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadDir(dir, DefaultLimits); err != nil || s.Len() != 0 {
		t.Errorf("directory entries are skipped: %v", err)
	}
}

func TestUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "as64500.txt")
	if err := os.WriteFile(path, []byte("64500:1,x\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir, DefaultLimits); err == nil || !strings.Contains(err.Error(), "as64500.txt") {
		t.Fatalf("unreadable file accepted: %v", err)
	}
}

func TestDanglingSymlinkAndSymlinkTemplates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "src_tmpl.txt"), []byte("<ASN>:1912:0,Metro FRA\n<ASN>:1913:nnn,Country $0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"as6695.txt", "as43252.txt"} {
		if err := os.Symlink("src_tmpl.txt", filepath.Join(dir, n)); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
	}
	s, err := LoadDir(dir, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	check(t, s, []lookup{
		{Large, "6695:1912:0", "Metro FRA"}, {Large, "43252:1913:276", "Country 276"},
		{Large, "6695:1913:5", "Country 5"},
	})
	if err := os.Symlink("missing.txt", filepath.Join(dir, "as7.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir, DefaultLimits); err == nil || !strings.Contains(err.Error(), "as7.txt") {
		t.Fatalf("dangling symlink: %v", err)
	}
	// <ASN> is not expanded in well-known.txt (it has no ASN).
	if _, err := loadFiles(t, map[string]string{"well-known.txt": "<ASN>:1,x\n"}); err == nil {
		t.Fatal("<ASN> accepted without an ASN")
	}
}

func TestLimits(t *testing.T) {
	lim := Limits{MaxFiles: 2, MaxFileBytes: 64, MaxDefinitions: 3}
	for name, files := range map[string]map[string]string{
		"files":       {"as1.txt": "1:1,a\n", "as2.txt": "2:1,a\n", "as3.txt": "3:1,a\n"},
		"file size":   {"as1.txt": "1:1," + strings.Repeat("a", 100) + "\n"},
		"definitions": {"as1.txt": "1:1,a\n1:2,a\n", "as2.txt": "2:1,a\n2:2,a\n"},
		"metadata":    {"metadata.yaml": "source: " + strings.Repeat("a", 100) + "\n"},
	} {
		if _, err := loadFilesLimits(t, files, lim); err == nil {
			t.Errorf("%s: limit not enforced", name)
		}
	}
	if _, err := loadFilesLimits(t, map[string]string{"as1.txt": "1:1,a\n1:2,b\n1:3,c\n"}, lim); err != nil {
		t.Errorf("exactly at the limit: %v", err)
	}
}

func TestMetadata(t *testing.T) {
	files := map[string]string{
		"as64500.txt": "64500:100,Customer\n64500:4xx,Regional $0\nsoo 64500:nnn,Origin\n",
		"metadata.yaml": `
source: "Operator configuration export"
communities:
  "64500:100":
    category: Route classification
    status: implemented
    evidence: community list
    notes: "Reviewed"
  "64500:4xx":
    status: Documented/planned
    evidence_note: "pattern"
  "soo 64500:nnn":
    status: unresolved
`}
	s := mustLoad(t, files)
	if s.MetadataSource != "Operator configuration export" || s.Stats().MetadataEntries != 3 {
		t.Fatalf("%q %+v", s.MetadataSource, s.Stats())
	}
	d := s.Definitions()
	if d[0].Status != StatusImplemented || d[0].Category != "Route classification" || d[0].Evidence != "community list" || d[0].Notes != "Reviewed" || d[0].Source != "AS64500" || d[0].File != "as64500.txt" || d[0].Line != 1 {
		t.Errorf("%+v", d[0])
	}
	if d[1].Status != StatusDocumented || d[1].EvidenceNote != "pattern" || d[1].Exact || d[1].Key != "64500:4xx" || d[1].Description != "Regional $0" {
		t.Errorf("%+v", d[1])
	}
	if d[2].Status != StatusUnresolved || d[2].Kind != Extended {
		t.Errorf("%+v", d[2])
	}
	// Metadata never changes lookups.
	check(t, s, []lookup{{Standard, "64500:123", ""}, {Standard, "64500:412", "Regional 1"}})

	for name, meta := range map[string]string{
		"unknown field": "communities:\n  \"64500:100\": {bogus: 1}\n",
		"unknown top":   "bogus: 1\n",
		"undefined":     "communities:\n  \"64500:999\": {status: implemented}\n",
		"non-canonical": "communities:\n  \"64500:4XX\": {status: implemented}\n",
		"bad status":    "communities:\n  \"64500:100\": {status: verified}\n",
		"bad community": "communities:\n  \"nonsense\": {status: implemented}\n",
		"comma key":     "communities:\n  \"64500:100,x\": {status: implemented}\n",
		"two documents": "source: a\n---\nsource: b\n",
		"not yaml":      "communities: [\n",
		"control char":  "source: \"a\\u0001b\"\n",
		"duplicate key": "communities:\n  \"64500:100\": {status: implemented}\n  \"64500:100\": {status: unresolved}\n",
	} {
		_, err := loadFiles(t, map[string]string{"as64500.txt": files["as64500.txt"], "metadata.yaml": meta})
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "metadata.yaml") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An empty metadata file is fine.
	mustLoad(t, map[string]string{"as64500.txt": files["as64500.txt"], "metadata.yaml": ""})
}

func TestNilSet(t *testing.T) {
	var s *Set
	if d, ok := s.Lookup(Standard, "1:1"); ok || d != "" || s.Len() != 0 || s.Definitions() != nil || s.Stats().Definitions != 0 {
		t.Fatal("nil set must be empty")
	}
	if k, ok := ParseKind("large"); !ok || k != Large || k.String() != "large" {
		t.Fatal("kind")
	}
	if _, ok := ParseKind("nope"); ok {
		t.Fatal("kind")
	}
	check(t, mustLoad(t, map[string]string{"as1.txt": "1:1,x\n"}), []lookup{{Kind(9), "1:1", ""}})
}

// TestUpstreamCorpus loads a checkout of the NLNOG communities directory when
// NLNOG_COMMUNITIES_DIR is set. It reports, but does not assert, how the
// upstream files fare under this implementation's stricter validation.
func TestUpstreamCorpus(t *testing.T) {
	dir := os.Getenv("NLNOG_COMMUNITIES_DIR")
	if dir == "" {
		t.Skip("NLNOG_COMMUNITIES_DIR not set")
	}
	s, err := Load(dir)
	if err != nil {
		t.Logf("%v", err)
		return
	}
	t.Logf("loaded %+v", s.Stats())
}

func BenchmarkLookup(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&sb, "%d:%d,Exact %d\n", 1+i%2000, i%60000, i)
	}
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&sb, "%d:%dxx,Pattern $0$1\n", 1+i, i%600)
		fmt.Fprintf(&sb, "%d:nnn:%d,Wild AS$0\n", 1+i, i)
	}
	sb.WriteString("65535:nnn,catch all $0\n65000-65100:x1,range $0\n")
	s, err := loadFiles(b, map[string]string{"as1.txt": sb.String()})
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name string
		kind Kind
		v    string
	}{
		{"exact", Standard, "7:5006"},
		{"pattern", Standard, "100:1234"},
		{"wild-large", Large, "100:4200000000:99"},
		{"generic-bucket", Standard, "65050:51"},
		{"miss", Standard, "64999:64999"},
		{"well-known-name", Standard, "NO_EXPORT"},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s.Lookup(c.kind, c.v)
			}
		})
	}
}
