package web

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

// Detail is one labelled value in an expandable details grid. Only values the
// backend actually reported become Details; nothing is inferred or zero-filled.
type Detail struct {
	Label, Value, Title string
	Wide                bool
}

// NeighborRow is a bgp.Neighbor plus its presentation.
type NeighborRow struct {
	bgp.Neighbor
	StateClass string
	// UpText is the "last change" cell: bgpctl's own relative time, prefixed
	// "up" only when the session is Established (then it is the uptime).
	UpText  string
	Details []Detail
}

// neighborRows builds the rows for one backend. showCounters reports whether
// any neighbor carries stats, so the prefix and message columns are omitted
// for backends (and for /summary, which never has them) where they would be
// empty everywhere.
func neighborRows(ns []bgp.Neighbor) (rows []NeighborRow, showCounters bool) {
	rows = make([]NeighborRow, len(ns))
	for i, n := range ns {
		if n.Stats != nil {
			showCounters = true
		}
		rows[i] = NeighborRow{Neighbor: n, StateClass: bgpStateClass(n.State), UpText: upText(n), Details: neighborDetails(n)}
	}
	return rows, showCounters
}

// bgpStateClass gives restrained semantic color to bgpd's session states. The
// state text itself is always shown verbatim next to it.
func bgpStateClass(state string) string {
	switch strings.ToLower(state) {
	case "established":
		return "good"
	case "connect", "active", "opensent", "openconfirm":
		return "warn"
	}
	return "muted"
}

func upText(n bgp.Neighbor) string {
	if n.LastUpDown == "" {
		return ""
	}
	if strings.EqualFold(n.State, "established") && n.LastUpDown != "Never" {
		return "up " + n.LastUpDown
	}
	return n.LastUpDown
}

func neighborDetails(n bgp.Neighbor) []Detail {
	var d []Detail
	add := func(label, value, title string, wide bool) {
		if value != "" {
			d = append(d, Detail{Label: label, Value: value, Title: title, Wide: wide})
		}
	}
	add("Group", n.Group, "", false)
	add("BGP ID", n.BGPID, "", false)
	if s := n.Session; s != nil {
		if c := s.Capabilities; c != nil && len(c.Multiprotocol) > 0 {
			add("Address families", strings.Join(c.Multiprotocol, ", "), "Negotiated multiprotocol capabilities of this session, as reported by OpenBGPD", true)
		}
		add("Hold time", secondsValue(s.HoldTime), "", false)
		add("Keepalive", secondsValue(s.Keepalive), "", false)
	}
	if st := n.Stats; st != nil {
		add("Last read", st.LastRead, "Time since OpenBGPD last read from this peer", false)
		add("Last write", st.LastWrite, "Time since OpenBGPD last wrote to this peer", false)
		if m := st.Message; m != nil {
			add("Messages received", messageCounts(m.Received), "", true)
			add("Messages sent", messageCounts(m.Sent), "", true)
		}
		if u := st.Update; u != nil {
			add("Updates received", updateCounts(u.Received), "", true)
			add("Updates sent", updateCounts(u.Sent), "", true)
		}
	}
	add("Last error received", n.LastErrorReceived, "Last NOTIFICATION received from the peer", true)
	add("Last error sent", n.LastErrorSent, "Last NOTIFICATION sent to the peer", true)
	add("Last shutdown reason", n.LastShutdownReason, "Shutdown communication received from the peer (RFC 8203); text supplied by the peer", true)
	return d
}

func secondsValue(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10) + " s"
}

// counts renders "name n · name n" for the pairs whose value was reported.
func counts(pairs ...any) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if v, ok := pairs[i+1].(*int64); ok && v != nil {
			parts = append(parts, fmt.Sprintf("%s %d", pairs[i], *v))
		}
	}
	return strings.Join(parts, " · ")
}

func messageCounts(m *bgp.MessageCounts) string {
	if m == nil {
		return ""
	}
	return counts("open", m.Open, "notifications", m.Notifications, "updates", m.Updates, "keepalives", m.Keepalives, "route refresh", m.RouteRefresh, "total", m.Total)
}

func updateCounts(u *bgp.UpdateCounts) string {
	if u == nil {
		return ""
	}
	return counts("updates", u.Updates, "withdraws", u.Withdraws, "EOR", u.EOR)
}

// RTRRow is one RTR session ready to render.
type RTRRow struct {
	// Label is the bgpd.conf description, or "Validator N" (N is the position
	// in this backend's answer) when there is none.
	Label string
	// Address and Local are empty unless features.rtr_session_addresses is on.
	Address, Local string
	State          string
	StateClass     string
	StateNote      string
	Protocol       string
	SessionID      string
	Serial         string
	Intervals      string
	IntervalsTitle string
	Errors         []Detail
	Timers         []Detail
}

// rtrRows converts a backend's sessions. Addresses are included only when the
// operator enabled them.
func rtrRows(sessions []bgp.RTRSession, addresses bool) (rows []RTRRow, anyError bool) {
	rows = make([]RTRRow, len(sessions))
	for i, s := range sessions {
		r := RTRRow{Label: s.Description, State: s.State}
		if r.Label == "" {
			r.Label = fmt.Sprintf("Validator %d", i+1)
		}
		if addresses {
			r.Address = hostPort(s.RemoteAddr, s.RemotePort)
			r.Local = s.LocalAddr
		}
		r.StateClass, r.StateNote = rtrStateClass(s)
		if r.State == "" {
			r.State = "unknown"
		}
		r.Protocol = rtrProtocol(s)
		r.SessionID, r.Serial = numValue(s.SessionID), numValue(s.Serial)
		r.Intervals, r.IntervalsTitle = rtrIntervals(s)
		if s.LastRecvError != "" {
			r.Errors = append(r.Errors, Detail{Label: "Last error received", Value: errorText(s.LastRecvError, s.LastRecvMsg)})
		}
		if s.LastSentError != "" {
			r.Errors = append(r.Errors, Detail{Label: "Last error sent", Value: errorText(s.LastSentError, s.LastSentMsg)})
		}
		anyError = anyError || len(r.Errors) > 0
		for _, t := range s.Timers {
			if t.Name != "" && t.Due != nil {
				v := "due"
				if *t.Due > 0 {
					v = "in " + humanSeconds(*t.Due)
				}
				r.Timers = append(r.Timers, Detail{Label: t.Name, Value: v})
			}
		}
		rows[i] = r
	}
	return rows, anyError
}

// rtrStateClass colors bgpd's RTR session state. A session is only "good"
// when it is established and has reported the identity (session ID and serial)
// that an exchange produces; an established session without them is flagged
// rather than presented as healthy, and an unknown state stays neutral.
func rtrStateClass(s bgp.RTRSession) (class, note string) {
	switch strings.ToLower(s.State) {
	case "established":
		if s.SessionID == nil || s.Serial == nil {
			return "warn", "Established, but the backend did not report a session ID and serial, so this is not confirmed as synchronized"
		}
		return "good", ""
	case "exchange", "negotiation":
		return "warn", "The session is still being set up or is exchanging data"
	case "closed", "error":
		return "bad", ""
	case "":
		return "muted", "The backend did not report a state (older OpenBGPD releases omit it)"
	}
	return "muted", "State value not recognized by this application"
}

func rtrProtocol(s bgp.RTRSession) string {
	switch {
	case s.Version == nil:
		return ""
	case s.MinimalVersion != nil:
		return fmt.Sprintf("v%d (min v%d)", *s.Version, *s.MinimalVersion)
	}
	return fmt.Sprintf("v%d", *s.Version)
}

// rtrIntervals renders refresh / retry / expire; absent values show a dash.
func rtrIntervals(s bgp.RTRSession) (text, title string) {
	if s.Refresh == nil && s.Retry == nil && s.Expire == nil {
		return "", ""
	}
	part := func(p *int64) string {
		if p == nil {
			return "—"
		}
		return humanSeconds(*p)
	}
	raw := func(p *int64) string {
		if p == nil {
			return "—"
		}
		return strconv.FormatInt(*p, 10) + " s"
	}
	return part(s.Refresh) + " / " + part(s.Retry) + " / " + part(s.Expire),
		"Refresh / retry / expire interval: " + raw(s.Refresh) + " / " + raw(s.Retry) + " / " + raw(s.Expire)
}

func numValue(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

func errorText(kind, msg string) string {
	if msg == "" {
		return kind
	}
	return kind + " — " + msg
}

func hostPort(addr string, port *int64) string {
	if addr == "" {
		return ""
	}
	if port == nil {
		return addr
	}
	return net.JoinHostPort(addr, strconv.FormatInt(*port, 10))
}

// humanSeconds renders a duration compactly: 45s, 10m, 1h30m, 2d3h.
func humanSeconds(n int64) string {
	if n < 0 {
		n = 0
	}
	d, h, m, s := n/86400, n%86400/3600, n%3600/60, n%60
	switch {
	case d > 0:
		if h > 0 {
			return fmt.Sprintf("%dd%dh", d, h)
		}
		return fmt.Sprintf("%dd", d)
	case h > 0:
		if m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	case m > 0:
		if s > 0 {
			return fmt.Sprintf("%dm%ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", s)
}

// rtrBadge is the short capability label shown in a backend's panel header.
func rtrBadge(r bgp.RTRResult) (text, class string) {
	switch r.State {
	case bgp.RTRSessions:
		if len(r.Sessions) == 1 {
			return "1 session", "muted"
		}
		return fmt.Sprintf("%d sessions", len(r.Sessions)), "muted"
	case bgp.RTRNoSessions:
		return "no sessions", "muted"
	case bgp.RTRUnsupported:
		return "unsupported", "warn"
	case bgp.RTRUnavailable:
		return "unavailable", "bad"
	}
	return "malformed response", "bad"
}
