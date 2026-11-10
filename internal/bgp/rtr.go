package bgp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"unicode/utf8"
)

// RTRState is the outcome of one /rtr query against one backend. The five
// states are deliberately distinct: "no sessions" must never be shown when the
// application simply could not retrieve RTR information.
type RTRState string

const (
	// RTRSessions: the backend answered and reported at least one session.
	RTRSessions RTRState = "sessions"
	// RTRNoSessions: the backend answered with a valid document that lists no
	// session (bgpd has no rtr directive configured).
	RTRNoSessions RTRState = "none"
	// RTRUnsupported: the backend's bgplgd has no /rtr endpoint (HTTP 404).
	RTRUnsupported RTRState = "unsupported"
	// RTRUnavailable: the backend could not be queried (transport error,
	// timeout, cancellation or a non-404 HTTP error).
	RTRUnavailable RTRState = "unavailable"
	// RTRMalformed: the backend answered, but not with a usable document
	// (empty body, invalid JSON, unexpected structure, reported failure or
	// oversize).
	RTRMalformed RTRState = "malformed"
)

// Bounds for /rtr. A real answer is a few hundred bytes per validator.
const (
	maxRTRBytes    = 1 << 20
	maxRTRSessions = 256
	maxRTRTimers   = 16
	maxRTRText     = 256
)

// RTRTimer is one running timer of an RTR session as reported by bgpctl
// ("RTR RefreshTimer", ...). Due is seconds until it fires, as bgpctl reports.
type RTRTimer struct {
	Name string `json:"name"`
	Due  *int64 `json:"due"`
}

// RTRSession follows `bgpctl -j show rtr`. Every field is optional: bgpctl
// omits version, minimal_version, session_id and serial until the session has
// exchanged data, and older OpenBGPD releases lack some fields (for example
// state before 8.5). Pointers keep "absent" distinct from zero.
type RTRSession struct {
	Description    string     `json:"descr"`
	RemoteAddr     string     `json:"remote_addr"`
	RemotePort     *int64     `json:"remote_port"`
	LocalAddr      string     `json:"local_addr"`
	State          string     `json:"state"`
	Version        *int64     `json:"version"`
	MinimalVersion *int64     `json:"minimal_version"`
	SessionID      *int64     `json:"session_id"`
	Serial         *int64     `json:"serial"`
	Refresh        *int64     `json:"refresh"`
	Retry          *int64     `json:"retry"`
	Expire         *int64     `json:"expire"`
	LastSentError  string     `json:"last_sent_error"`
	LastSentMsg    string     `json:"last_sent_msg"`
	LastRecvError  string     `json:"last_recv_error"`
	LastRecvMsg    string     `json:"last_recv_msg"`
	Timers         []RTRTimer `json:"timers"`
}

// HasError reports whether the backend reported an RTR error in either
// direction.
func (s RTRSession) HasError() bool {
	return s.LastSentError != "" || s.LastRecvError != ""
}

// RTRResult is one backend's RTR answer. Sessions is empty unless State is
// RTRSessions.
type RTRResult struct {
	State    RTRState
	Detail   string // fixed, operator-safe explanation for non-success states
	Reason   Reason // bounded reason for metrics and logs; empty on success
	Sessions []RTRSession
	Bytes    int64
}

// RTR queries the backend's /rtr endpoint. It never returns an error: every
// outcome is expressed as an RTRState so callers cannot conflate them. The
// request is bounded by the client timeout and a 1 MiB body limit.
func (c *Client) RTR(ctx context.Context) RTRResult {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	fail := func(state RTRState, reason Reason, detail string) RTRResult {
		return RTRResult{State: state, Reason: reason, Detail: detail}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpointURL("/rtr", nil), nil)
	if err != nil {
		return fail(RTRUnavailable, ReasonBackendError, "Could not query the backend")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		r := c.classify(parent, ctx, err, true)
		return fail(RTRUnavailable, r, c.describe(r, err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fail(RTRUnsupported, ReasonNone, "This backend's bgplgd does not provide /rtr (HTTP 404)")
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fail(RTRUnavailable, ReasonBackendError, fmt.Sprintf("Backend returned HTTP %d", resp.StatusCode))
	}

	limit := min(c.MaxResponseBytes, maxRTRBytes)
	if limit <= 0 {
		limit = maxRTRBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		r := c.classify(parent, ctx, err, false)
		return fail(RTRUnavailable, r, c.describe(r, err))
	}
	if int64(len(body)) > limit {
		return fail(RTRMalformed, ReasonResponseLimit, "RTR response exceeded the size limit")
	}
	sessions, err := parseRTR(body)
	if err != nil {
		var pe rtrParseError
		detail := "Backend returned malformed or unexpected JSON"
		if errors.As(err, &pe) {
			detail = pe.Error()
		}
		res := fail(RTRMalformed, ReasonInvalidJSON, detail)
		res.Bytes = int64(len(body))
		return res
	}
	res := RTRResult{State: RTRNoSessions, Sessions: sessions, Bytes: int64(len(body))}
	if len(sessions) > 0 {
		res.State = RTRSessions
	}
	return res
}

// rtrParseError carries a fixed message that is safe to show: it never
// includes any part of the backend's body.
type rtrParseError string

func (e rtrParseError) Error() string { return string(e) }

// parseRTR validates a `bgpctl -j show rtr` document. bgpctl prints an object
// whose only key is "rtrs" (an array); with no sessions configured the object
// is empty. An empty body is how bgplgd reports a rejected command and is an
// error, not "no sessions".
func parseRTR(body []byte) ([]RTRSession, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, rtrParseError("Backend returned an empty response; OpenBGPD may have rejected the command")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return nil, rtrParseError("Backend returned malformed or unexpected JSON")
	}
	if status, ok := root["status"]; ok && Flexible(status) == "FAILED" {
		return nil, rtrParseError("Backend reported a failure")
	}
	raw, ok := root["rtrs"]
	if !ok {
		for k := range root {
			if k != "status" {
				return nil, rtrParseError("Backend returned a document that is not an RTR session list")
			}
		}
		return nil, nil
	}
	var items []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &items) != nil {
		return nil, rtrParseError("Backend returned malformed or unexpected JSON")
	}
	if len(items) > maxRTRSessions {
		return nil, rtrParseError("Backend reported more RTR sessions than this application will display")
	}
	out := make([]RTRSession, 0, len(items))
	for _, it := range items {
		var s RTRSession
		if err := json.Unmarshal(it, &s); err != nil {
			return nil, rtrParseError("Backend returned malformed or unexpected JSON")
		}
		if err := s.validate(); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// validate normalizes text and rejects values bgpctl cannot have produced.
func (s *RTRSession) validate() error {
	for _, a := range []string{s.RemoteAddr, s.LocalAddr} {
		if a != "" {
			if _, err := netip.ParseAddr(a); err != nil {
				return rtrParseError("Backend returned an RTR session with an invalid address")
			}
		}
	}
	for _, p := range []*int64{s.RemotePort, s.Version, s.MinimalVersion, s.SessionID, s.Serial, s.Refresh, s.Retry, s.Expire} {
		if p != nil && *p < 0 {
			return rtrParseError("Backend returned an RTR session with a negative value")
		}
	}
	if s.RemotePort != nil && *s.RemotePort > 65535 {
		return rtrParseError("Backend returned an RTR session with an invalid port")
	}
	for _, f := range []*string{&s.Description, &s.State, &s.LastSentError, &s.LastSentMsg, &s.LastRecvError, &s.LastRecvMsg} {
		*f = cleanText(*f, maxRTRText)
	}
	if len(s.Timers) > maxRTRTimers {
		s.Timers = s.Timers[:maxRTRTimers]
	}
	for i := range s.Timers {
		s.Timers[i].Name = cleanText(s.Timers[i].Name, 64)
	}
	return nil
}

// cleanText makes backend text safe to show anywhere: valid UTF-8, control
// characters replaced by spaces, bounded length. Escaping for HTML is still
// done by html/template at render time.
func cleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return s
}
