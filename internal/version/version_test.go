package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	vcs := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
		{Key: "vcs.time", Value: "2026-11-10T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	cases := map[string]struct {
		ver, commit, date string
		bi                *debug.BuildInfo
		want              Info
	}{
		"release ldflags": {"0.1.0", "abc1234", "2026-11-10T01:02:03Z", nil,
			Info{"v0.1.0", "abc1234", "2026-11-10T01:02:03Z", "go1.24.0"}},
		"full revision abbreviated": {"0.1.0", "0123456789abcdef0123456789abcdef01234567", "x", nil, Info{"v0.1.0", "0123456", "x", "go1.24.0"}},
		"tag already prefixed":      {"v1.2.3-rc.1", "abc1234", "x", nil, Info{"v1.2.3-rc.1", "abc1234", "x", "go1.24.0"}},
		"snapshot name":             {"0.0.1-next", "abc1234", "x", nil, Info{"v0.0.1-next", "abc1234", "x", "go1.24.0"}},
		"plain development":         {"dev", "unknown", "unknown", nil, Info{"dev", "unknown", "unknown", "go1.24.0"}},
		"empty values":              {"", "", "", nil, Info{"dev", "unknown", "unknown", "go1.24.0"}},
		"vcs fallback, dirty":       {"dev", "unknown", "unknown", vcs, Info{"dev", "0123456-dirty", "2026-11-10T00:00:00Z", "go1.24.0"}},
		"ldflags beat vcs":          {"0.1.0", "abc1234", "d", vcs, Info{"v0.1.0", "abc1234", "d", "go1.24.0"}},
		"module version":            {"dev", "unknown", "unknown", &debug.BuildInfo{Main: debug.Module{Version: "v0.3.0"}}, Info{"v0.3.0", "unknown", "unknown", "go1.24.0"}},
		"devel module":              {"dev", "unknown", "unknown", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, Info{"dev", "unknown", "unknown", "go1.24.0"}},
	}
	for name, c := range cases {
		if got := resolve(c.ver, c.commit, c.date, "go1.24.0", c.bi); got != c.want {
			t.Errorf("%s: got %+v want %+v", name, got, c.want)
		}
	}
}

func TestString(t *testing.T) {
	got := Info{"v0.1.0", "abc1234", "2026-11-10T00:00:00Z", "go1.24.3"}.String()
	want := "hextet-lg v0.1.0\ncommit: abc1234\nbuilt: 2026-11-10T00:00:00Z\ngo: go1.24.3\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !strings.HasPrefix(Get().String(), "hextet-lg ") {
		t.Fatal("Get().String() must start with the program name")
	}
}
