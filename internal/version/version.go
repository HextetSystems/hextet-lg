// Package version reports the identity of the running build.
//
// Release builds receive Version, Commit and Date from GoReleaser through
// linker flags (see .goreleaser.yaml). Anything built another way falls back
// to what the Go toolchain embedded (module version, VCS revision and time)
// and finally to "dev" / "unknown", so a development binary never claims to
// be a release.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set at link time: -X github.com/HextetSystems/hextet-lg/internal/version.Version=...
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

const shortCommit = 7

// Info is the resolved build identity.
type Info struct {
	Version   string // "v0.1.0", a snapshot name, or "dev"
	Commit    string // abbreviated revision, "-dirty" suffix for a modified tree, or "unknown"
	Date      string // RFC 3339 build or commit time, or "unknown"
	GoVersion string // toolchain that built the binary
}

// Get resolves the build identity from the link-time values, falling back to
// the build information embedded by the Go toolchain.
func Get() Info {
	bi, _ := debug.ReadBuildInfo()
	return resolve(Version, Commit, Date, runtime.Version(), bi)
}

func resolve(ver, commit, date, goVersion string, bi *debug.BuildInfo) Info {
	var vcsRev, vcsTime string
	var dirty bool
	if bi != nil {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				vcsRev = s.Value
			case "vcs.time":
				vcsTime = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		// `go install module@version` records the module version.
		if (ver == "" || ver == "dev") && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			ver = bi.Main.Version
		}
	}
	if commit == "" || commit == "unknown" {
		commit = "unknown"
		if vcsRev != "" {
			commit = abbreviate(vcsRev)
			if dirty {
				commit += "-dirty"
			}
		}
	} else {
		// Source-built images receive the full revision (it also feeds the
		// OCI label); show the same short form GoReleaser stamps.
		commit = abbreviate(commit)
	}
	if date == "" || date == "unknown" {
		date = "unknown"
		if vcsTime != "" {
			date = vcsTime
		}
	}
	if ver == "" {
		ver = "dev"
	}
	// GoReleaser's {{.Version}} has no "v" prefix; tags do. Show one form.
	if ver[0] >= '0' && ver[0] <= '9' {
		ver = "v" + ver
	}
	return Info{Version: ver, Commit: commit, Date: date, GoVersion: goVersion}
}

func abbreviate(rev string) string {
	if len(rev) > shortCommit {
		return rev[:shortCommit]
	}
	return rev
}

// String is the output of `hextet-lg version`.
func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hextet-lg %s\ncommit: %s\nbuilt: %s\ngo: %s\n", i.Version, i.Commit, i.Date, i.GoVersion)
	return b.String()
}
