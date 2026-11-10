package bgp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

// Status says whether a backend query produced the whole answer.
type Status string

const (
	// StatusComplete: the entire JSON document was decoded and validated.
	StatusComplete Status = "complete"
	// StatusPartial: the query was interrupted after at least one complete
	// route was decoded. More matching routes may exist.
	StatusPartial Status = "partial"
	// StatusFailed: no usable routes were obtained.
	StatusFailed Status = "failed"
)

// Reason explains a partial or failed status.
type Reason string

const (
	ReasonNone             Reason = ""
	ReasonTimeout          Reason = "timeout"
	ReasonResponseLimit    Reason = "response_limit"
	ReasonConnectionClosed Reason = "connection_closed"
	ReasonInvalidJSON      Reason = "invalid_json"
	ReasonBackendError     Reason = "backend_error"
	// ReasonMemoryLimit: the per-query retained-memory limit
	// (looking_glass.max_retained_bytes) was reached. Deterministic for a
	// given query and backend data.
	ReasonMemoryLimit Reason = "memory_limit"
	// ReasonCapacity: the instance-wide retained-memory budget
	// (looking_glass.max_total_retained_bytes) was exhausted by other work.
	// Transient: the same query can succeed once capacity recovers.
	ReasonCapacity Reason = "capacity"
	ReasonCanceled Reason = "canceled"
	ReasonBusy     Reason = "busy"
)

// Transient reports reasons that describe this instance's momentary state
// (shared capacity, queueing, a departed client) rather than the query or the
// backend data. Results ending this way must not be replayed from the cache.
func (r Reason) Transient() bool {
	return r == ReasonCapacity || r == ReasonBusy || r == ReasonCanceled
}

// Completion describes how a backend query ended.
type Completion struct {
	Status Status
	Reason Reason
	// Detail is a short, safe explanation (never contains backend addresses).
	Detail string
	// RouteCount is the number of complete routes decoded and retained.
	RouteCount int
	// Bytes is the number of upstream response body bytes read.
	Bytes int64
	// Elapsed is the total query duration; FirstRoute is the time until the
	// first complete route was decoded (zero if none).
	Elapsed    time.Duration
	FirstRoute time.Duration
	// MemBytes is the estimated memory retained for the decoded routes.
	MemBytes int64
}

func (c Completion) IsComplete() bool { return c.Status == StatusComplete }
func (c Completion) IsPartial() bool  { return c.Status == StatusPartial }
func (c Completion) IsFailed() bool   { return c.Status == StatusFailed }

// Usable reports whether the routes can be shown (complete or partial).
func (c Completion) Usable() bool { return c.Status != StatusFailed }

// Label is the upper-case status label shown to users.
func (c Completion) Label() string { return strings.ToUpper(string(c.Status)) }

// Result is the outcome of one backend route query.
type Result struct {
	Completion
	Routes []Route
}

var (
	// ErrResponseLimit is returned when the upstream body exceeds the limit.
	ErrResponseLimit = errors.New("upstream response exceeds the configured limit")
	// errMemoryLimit stops decoding when retained routes reach the memory bound.
	errMemoryLimit = errors.New("retained route memory limit reached")
	// errCapacity stops decoding when the instance-wide budget has no room.
	errCapacity = errors.New("instance-wide retained memory budget exhausted")
	// errTruncated: the document ended before it was complete.
	errTruncated = errors.New("response ended before the JSON document was complete")
	// errStructure: well-formed JSON with an unexpected shape.
	errStructure = errors.New("unexpected response structure")
	errTrailing  = errors.New("unexpected data after the JSON document")
)

// boundedReader enforces the response-size limit while reading and remembers
// the first non-EOF read error so that the real cause (timeout, reset, limit)
// is not hidden behind a JSON error.
type boundedReader struct {
	r   io.Reader
	max int64
	n   int64
	err error // sticky non-EOF error
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	remaining := b.max - b.n
	if remaining <= 0 {
		// Exactly at the limit: only a clean EOF is acceptable.
		var one [1]byte
		n, err := b.r.Read(one[:])
		if n > 0 {
			b.err = ErrResponseLimit
			return 0, b.err
		}
		if err != nil && err != io.EOF {
			b.err = err
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}

// decodeStream incrementally decodes a bgplgd /rib document, calling sink for
// each complete route. It returns nil only when the whole document (including
// the enclosing object or array and the absence of trailing data) was valid.
//
// Accepted shapes: {"rib":[...]} (bgpctl -j), {} (no routes: bgpctl emits no
// rib array then), and, for compatibility, a bare array or an object whose
// array is named "routes" or "data". An optional top-level status of FAILED is
// reported through failed.
func decodeStream(r io.Reader, sink func(Route) error) (failed bool, err error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return false, eofToTruncated(err)
	}
	switch tok {
	case json.Delim('['):
		if err := readRoutes(dec, sink); err != nil {
			return false, err
		}
	case json.Delim('{'):
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return false, eofToTruncated(err)
			}
			key, _ := kt.(string)
			switch key {
			case "rib", "routes", "data":
				t, err := dec.Token()
				if err != nil {
					return false, eofToTruncated(err)
				}
				switch t {
				case nil: // null: no routes
				case json.Delim('['):
					if err := readRoutes(dec, sink); err != nil {
						return false, err
					}
				default:
					return false, fmt.Errorf("%w: %q is not an array", errStructure, key)
				}
			case "status":
				var s string
				if err := dec.Decode(&s); err != nil {
					return false, eofToTruncated(err)
				}
				failed = strings.EqualFold(s, "FAILED")
			case "error":
				// Free-form detail that accompanies status FAILED.
				if err := skipValue(dec); err != nil {
					return false, err
				}
			default:
				// Never report an unrecognized document as a complete (empty) result.
				return false, fmt.Errorf("%w: unexpected key %q", errStructure, key)
			}
		}
		if _, err := dec.Token(); err != nil { // closing }
			return false, eofToTruncated(err)
		}
	default:
		return false, fmt.Errorf("%w: expected an object or array", errStructure)
	}
	// Reject anything after the document; a clean EOF proves the whole body was read.
	if t, err := dec.Token(); err != io.EOF {
		if err == nil {
			_ = t
			return false, errTrailing
		}
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return false, errTrailing
		}
		return false, err
	}
	return failed, nil
}

// readRoutes decodes array elements until the closing bracket. The opening
// bracket has already been consumed. An element that is cut off or malformed
// is never passed to sink.
func readRoutes(dec *json.Decoder, sink func(Route) error) error {
	var raw json.RawMessage
	var buf bytes.Buffer
	for dec.More() {
		raw = raw[:0]
		if err := dec.Decode(&raw); err != nil {
			return eofToTruncated(err)
		}
		route, err := parseRoute(raw, &buf)
		if err != nil {
			return err
		}
		if err := sink(route); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // closing ]
		return eofToTruncated(err)
	}
	return nil
}

// skipValue consumes one JSON value without buffering it.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return eofToTruncated(err)
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '[', '{':
				depth++
			default:
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// eofToTruncated maps every "input ended early" signal to errTruncated.
// encoding/json reports EOF inside an open array or object as a SyntaxError
// with this message rather than as io.EOF; it is a connection problem, not
// malformed data, so it must not be classified as invalid JSON.
func eofToTruncated(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return errTruncated
	}
	var syn *json.SyntaxError
	if errors.As(err, &syn) && syn.Error() == "unexpected end of JSON input" {
		return errTruncated
	}
	return err
}

// routeWire mirrors a bgplgd record; Route is the normalized web-facing model.
type routeWire struct {
	Best             json.RawMessage `json:"best"`
	Prefix           json.RawMessage `json:"prefix"`
	ASPath           json.RawMessage `json:"aspath"`
	ExitNexthop      json.RawMessage `json:"exit_nexthop"`
	TrueNexthop      json.RawMessage `json:"true_nexthop"`
	Neighbor         json.RawMessage `json:"neighbor"`
	Valid            json.RawMessage `json:"valid"`
	Source           json.RawMessage `json:"source"`
	OVS              json.RawMessage `json:"ovs"`
	AVS              json.RawMessage `json:"avs"`
	Origin           json.RawMessage `json:"origin"`
	Metric           json.RawMessage `json:"metric"`
	LocalPref        json.RawMessage `json:"localpref"`
	Weight           json.RawMessage `json:"weight"`
	DMetric          json.RawMessage `json:"dmetric"`
	LastUpdate       json.RawMessage `json:"last_update"`
	LastUpdateSec    json.RawMessage `json:"last_update_sec"`
	Communities      json.RawMessage `json:"communities"`
	ExtCommunities   json.RawMessage `json:"extended_communities"`
	LargeCommunities json.RawMessage `json:"large_communities"`
}

// parseRoute converts one complete route object. The raw JSON is compacted so
// that the retained copy is as small as possible (bgpctl indents with tabs).
func parseRoute(raw []byte, buf *bytes.Buffer) (Route, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return Route{}, fmt.Errorf("%w: route is not an object", errStructure)
	}
	var w routeWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Route{}, fmt.Errorf("%w: %v", errStructure, err)
	}
	buf.Reset()
	rawText := ""
	if json.Compact(buf, raw) == nil {
		rawText = buf.String()
	} else {
		rawText = string(raw)
	}
	path := Flexible(w.ASPath)
	r := Route{
		Raw: rawText, Best: string(w.Best) == "true", Prefix: Flexible(w.Prefix), ASPath: path,
		ExitNexthop: Flexible(w.ExitNexthop), TrueNexthop: Flexible(w.TrueNexthop), Neighbor: neighborName(w.Neighbor),
		Source: Flexible(w.Source), OVS: Flexible(w.OVS), AVS: Flexible(w.AVS), Origin: Flexible(w.Origin),
		OriginASN: originASN(path), Metric: optionalInt(w.Metric), LocalPref: optionalInt(w.LocalPref),
		Weight: optionalInt(w.Weight), DMetric: optionalInt(w.DMetric), LastUpdate: Flexible(w.LastUpdate),
		LastUpdateSec: optionalInt(w.LastUpdateSec), Communities: optionalStrings(w.Communities),
		ExtCommunities: optionalStrings(w.ExtCommunities), LargeCommunities: optionalStrings(w.LargeCommunities),
	}
	if len(w.Valid) > 0 && string(w.Valid) != "null" {
		var valid bool
		if json.Unmarshal(w.Valid, &valid) == nil {
			r.Valid = &valid
		} else {
			state := strings.ToLower(Flexible(w.Valid))
			valid = state == "valid" || state == "true" || state == "yes"
			r.Valid = &valid
		}
	}
	return r, nil
}

// neighborName reads bgpctl's neighbor object ({description, remote_addr, bgp_id})
// or a plain string.
func neighborName(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] != '{' {
		return Flexible(raw)
	}
	var n struct {
		Description string `json:"description"`
		RemoteAddr  string `json:"remote_addr"`
	}
	if json.Unmarshal(raw, &n) != nil {
		return Flexible(raw)
	}
	switch {
	case n.Description != "" && n.RemoteAddr != "":
		return n.Description + " (" + n.RemoteAddr + ")"
	case n.Description != "":
		return n.Description
	default:
		return n.RemoteAddr
	}
}

// MemSize estimates the memory retained by a route: the struct, its strings
// and slices, plus allocator and slice-growth overhead. It is an estimate used
// only to bound retention.
func (r *Route) MemSize() int64 {
	n := int64(unsafe.Sizeof(*r)) * 3 / 2
	n += int64(len(r.Raw) + len(r.Prefix) + len(r.ASPath) + len(r.ExitNexthop) + len(r.TrueNexthop) + len(r.Neighbor) +
		len(r.Source) + len(r.OVS) + len(r.AVS) + len(r.Origin) + len(r.OriginASN) + len(r.LastUpdate))
	for _, list := range [][]string{r.Communities, r.ExtCommunities, r.LargeCommunities} {
		for _, s := range list {
			n += int64(len(s)) + 16
		}
	}
	for _, p := range []*int64{r.Metric, r.LocalPref, r.Weight, r.DMetric, r.LastUpdateSec} {
		if p != nil {
			n += 8
		}
	}
	return n + n/8
}

func optionalInt(raw json.RawMessage) *int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	n, err := strconv.ParseInt(Flexible(raw), 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func optionalStrings(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) == nil {
		result := make([]string, 0, len(values))
		for _, v := range values {
			result = append(result, Flexible(v))
		}
		return result
	}
	return []string{Flexible(raw)}
}

func originASN(path string) string {
	fields := strings.Fields(path)
	for i := len(fields) - 1; i >= 0; i-- {
		field := strings.Trim(fields[i], "{}()[]<>[],")
		if strings.Contains(field, ",") {
			parts := strings.Split(field, ",")
			field = parts[len(parts)-1]
		}
		if _, err := strconv.ParseUint(field, 10, 32); err == nil {
			return field
		}
	}
	return ""
}

// Flexible returns an API value as text while retaining scalar and array forms.
func Flexible(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}
