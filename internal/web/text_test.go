package web

import (
	"github.com/HextetSystems/hextet-lg/internal/bgp"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTextGolden(t *testing.T) {
	n := int64(100)
	routes := []BackendResult{{Name: "Alpha", Location: "West", Result: complete(1200*time.Millisecond, []bgp.Route{{Prefix: "192.0.2.0/24", ASPath: "64500 15169", OriginASN: "15169", TrueNexthop: "192.0.2.1", LocalPref: &n, Best: true, OVS: "valid", AVS: "unknown", LargeCommunities: []string{"395089:0:6"}}, {Prefix: "192.0.2.0/24", ASPath: "64501 15169"}, {Prefix: "2001:db8::/32", ASPath: "64500 64501 64502 15169", ExitNexthop: "2001:db8::1"}})}, {Name: "Beta", Result: complete(300*time.Millisecond, []bgp.Route{{Prefix: "2001:db8::/32", ASPath: "15169"}})}}
	for _, view := range []string{"text", "detail"} {
		t.Run(view, func(t *testing.T) {
			got := formatRoutes(routes, view)
			path := filepath.Join("testdata", view+".golden")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(path, []byte(got), 0644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("output differs from %s\n%s", path, got)
			}
		})
	}
}

func complete(elapsed time.Duration, routes []bgp.Route) bgp.Result {
	return bgp.Result{Routes: routes, Completion: bgp.Completion{Status: bgp.StatusComplete, RouteCount: len(routes), Elapsed: elapsed}}
}

func TestPartialAndFailedTextGolden(t *testing.T) {
	n := int64(100)
	routes := []bgp.Route{
		{Prefix: "192.0.2.0/24", ASPath: "64500 15169", OriginASN: "15169", TrueNexthop: "192.0.2.1", LocalPref: &n, Best: true, OVS: "valid", AVS: "unknown", LargeCommunities: []string{"395089:0:6", "395089:5:2"}, Communities: []string{"64500:1"}, ExtCommunities: []string{"rt 64500:1"}},
		{Prefix: "2001:db8::/32", ASPath: "64500 15169", ExitNexthop: "2001:db8::1"},
	}
	part := func(reason bgp.Reason, detail string) bgp.Result {
		return bgp.Result{Routes: routes, Completion: bgp.Completion{Status: bgp.StatusPartial, Reason: reason, Detail: detail, RouteCount: 2, Elapsed: 30 * time.Second}}
	}
	results := []BackendResult{
		{Name: "Alpha", Location: "West", Result: complete(2*time.Second, routes[:1])},
		{Name: "Winnipeg", Location: "Home", Result: part(bgp.ReasonTimeout, "Backend timeout after 30s")},
		{Name: "Gamma", Result: part(bgp.ReasonResponseLimit, "Response exceeded the configured limit of 128.0 MiB")},
		{Name: "Delta", Result: part(bgp.ReasonMemoryLimit, "Retained-result memory limit of 256.0 MiB reached")},
		{Name: "Epsilon", Result: bgp.Result{Completion: bgp.Completion{Status: bgp.StatusFailed, Reason: bgp.ReasonBackendError, Detail: "Could not query the backend", Elapsed: 120 * time.Millisecond}}},
		{Name: "Zeta", Result: complete(time.Second, nil)},
	}
	for _, view := range []string{"text", "detail"} {
		t.Run(view, func(t *testing.T) {
			got := formatRoutes(results, view)
			path := filepath.Join("testdata", "partial-"+view+".golden")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(path, []byte(got), 0644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("output differs from %s\n%s", path, got)
			}
		})
	}
}
