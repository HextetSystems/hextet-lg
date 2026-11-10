package web

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

func val(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func num(n *int64) string {
	if n == nil {
		return "—"
	}
	return fmt.Sprint(*n)
}

// textWidth counts display columns (runes), since "—" is multi-byte.
func textWidth(s string) int { return utf8.RuneCountInString(s) }

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }

// writeText renders sections in the traditional looking-glass layout directly
// from decoded route records. It streams through a buffered writer instead of
// building one large string, and stops when ctx (the client request) ends.
// Incomplete backends get a conspicuous warning; a partial or failed backend
// is never presented as an authoritative answer.
func writeText(ctx context.Context, w io.Writer, sections []Section, view string) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	for _, s := range sections {
		if err := writeSection(ctx, bw, s, view); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func backendLabel(r BackendResult) string {
	if r.Location != "" {
		return fmt.Sprintf("%s (%s)", r.Name, r.Location)
	}
	return r.Name
}

func writeSection(ctx context.Context, w *bufio.Writer, s Section, view string) error {
	r := s.Result
	switch r.Status {
	case bgp.StatusPartial:
		fmt.Fprintf(w, "WARNING: PARTIAL RESULTS\nBackend: %s\nReason: %s\nRoutes decoded: %d\nElapsed: %s\n", backendLabel(r), val(r.Detail), len(r.Routes), seconds(r.Elapsed))
		fmt.Fprint(w, "\nThe following routes are only a subset of the\nmatching routing information. More matching routes may exist.\nDo not conclude that a prefix is absent from the routing table.\n\n")
	case bgp.StatusFailed:
		fmt.Fprintf(w, "ERROR: QUERY FAILED\nBackend: %s\nReason: %s\nRoutes decoded: %d\nElapsed: %s\n", backendLabel(r), val(r.Detail), len(r.Routes), seconds(r.Elapsed))
		fmt.Fprint(w, "\nNo routes were received from this backend. This is NOT\nevidence that no matching routes exist.\n\n")
		return nil
	default:
		fmt.Fprintf(w, "Backend: %s\nStatus: COMPLETE — %d %s decoded in %s", backendLabel(r), len(r.Routes), plural(len(r.Routes), "route"), seconds(r.Elapsed))
		if r.Cached {
			fmt.Fprintf(w, " (cached, %s old)", r.Age().Round(time.Second))
		}
		fmt.Fprintln(w)
	}
	if len(r.Routes) == 0 {
		if r.IsComplete() {
			fmt.Fprint(w, "No matching routes.\n\n")
		}
		return nil
	}
	if len(s.Rows) == 0 {
		fmt.Fprintf(w, "No routes from this backend on this page (%d decoded in total).\n\n", len(r.Routes))
		return nil
	}
	if s.First != 1 || s.Last != len(r.Routes) {
		fmt.Fprintf(w, "Showing routes %d–%d of %d decoded from this backend\n", s.First, s.Last, len(r.Routes))
	}
	// Columns grow to fit the longest value so that long IPv6 prefixes and next
	// hops are never truncated or misaligned; IPv4-only output keeps the
	// traditional 39-column layout.
	pw, nw := 39, 39
	for _, rt := range s.Rows {
		pw, nw = max(pw, textWidth(val(rt.Prefix))), max(nw, textWidth(val(rt.NextHop())))
	}
	if view == "text" {
		fmt.Fprintln(w, "Flags: * = selected best (when reported); eligibility and validation are separate")
		fmt.Fprintln(w, "OVS: origin validation state; AVS: ASPA validation state")
		fmt.Fprintf(w, "%-1s %-*s %-*s %8s %8s %-12s %-12s %s\n", " ", pw, "Prefix", nw, "Next hop", "LocalPref", "MED", "OVS", "AVS", "AS path")
	}
	for i, rt := range s.Rows {
		if i&511 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if view == "text" {
			flag := " "
			if rt.Best {
				flag = "*"
			}
			fmt.Fprintf(w, "%s %-*s %-*s %8s %8s %-12s %-12s %s\n", flag, pw, val(rt.Prefix), nw, val(rt.NextHop()), num(rt.LocalPref), num(rt.Metric), val(rt.OVS), val(rt.AVS), val(rt.ASPath))
			continue
		}
		writeDetail(w, rt)
	}
	if r.IsPartial() {
		fmt.Fprintln(w, "-- end of partial results: additional matching routes may exist --")
	}
	fmt.Fprintln(w)
	return nil
}

func writeDetail(w io.Writer, r bgp.Route) {
	fmt.Fprintf(w, "Prefix: %s\n  Address family: %s\n  AS path: %s\n  Origin ASN: %s\n  Next hop: %s\n  Exit next hop: %s\n  True next hop: %s\n  Neighbor: %s\n  Source: %s\n  Origin: %s\n  Local preference: %s\n  MED: %s\n  Weight: %s\n  DMetric: %s\n  RPKI OVS: %s\n  ASPA AVS: %s\n  Last update: %s\n", val(r.Prefix), val(r.Family()), val(r.ASPath), val(r.OriginASN), val(r.NextHop()), val(r.ExitNexthop), val(r.TrueNexthop), val(r.Neighbor), val(r.Source), val(r.Origin), num(r.LocalPref), num(r.Metric), num(r.Weight), num(r.DMetric), val(r.OVS), val(r.AVS), val(r.LastUpdate))
	if r.Valid != nil {
		fmt.Fprintf(w, "  Eligible: %t\n", *r.Valid)
	}
	if r.Best {
		fmt.Fprintln(w, "  Selected: best")
	}
	if len(r.Communities) > 0 {
		fmt.Fprintf(w, "  Communities: %s\n", strings.Join(r.Communities, " "))
	}
	if len(r.ExtCommunities) > 0 {
		fmt.Fprintf(w, "  Extended communities: %s\n", strings.Join(r.ExtCommunities, " "))
	}
	if len(r.LargeCommunities) > 0 {
		fmt.Fprintf(w, "  Large communities: %s\n", strings.Join(r.LargeCommunities, " "))
	}
	fmt.Fprintln(w)
}

// formatRoutes renders every decoded route of every backend as one string.
// It is meant for tests and small pages; large responses use writeText.
func formatRoutes(results []BackendResult, view string) string {
	var b strings.Builder
	_ = writeText(context.Background(), &b, allSections(results), view)
	return b.String()
}

// sectionsText renders the given sections (one page) as a string for inline HTML.
func sectionsText(sections []Section, view string) string {
	var b strings.Builder
	_ = writeText(context.Background(), &b, sections, view)
	return b.String()
}
