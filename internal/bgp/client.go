package bgp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/visibility"
)

// Route is the normalized, presentation-facing form of one bgplgd rib entry.
// A prefix with several paths yields several Routes.
type Route struct {
	Raw              string // compact raw JSON of this route object
	Best             bool
	Prefix           string
	ASPath           string
	ExitNexthop      string
	TrueNexthop      string
	Neighbor         string
	Valid            *bool
	Source           string
	OVS              string
	AVS              string
	Origin           string
	OriginASN        string
	Metric           *int64
	LocalPref        *int64
	Weight           *int64
	DMetric          *int64
	LastUpdate       string
	LastUpdateSec    *int64
	Communities      []string
	ExtCommunities   []string
	LargeCommunities []string
}

// Family reports the route's address family, "IPv4" or "IPv6", from its prefix.
// It is never inferred from the next hop or the neighbor: an IPv6 route may
// have an IPv4 next hop (RFC 8950) and a neighbor of either family. VPN or
// otherwise unparsable prefixes yield "".
func (r Route) Family() string {
	host, _, _ := strings.Cut(r.Prefix, "/")
	a, err := netip.ParseAddr(host)
	switch {
	case err != nil:
		return ""
	case a.Is4():
		return "IPv4"
	default:
		return "IPv6"
	}
}

// NextHop prefers the resolved (true) next hop. bgpctl prints "???" when the
// true next hop is unresolved; the exit next hop is more useful then.
func (r Route) NextHop() string {
	if r.TrueNexthop != "" && r.TrueNexthop != "???" {
		return r.TrueNexthop
	}
	if r.ExitNexthop != "" {
		return r.ExitNexthop
	}
	return r.TrueNexthop
}

// DefaultMaxRetainedBytes bounds retained decoded routes when a Client does
// not set MaxRetainedBytes.
const DefaultMaxRetainedBytes = 256 << 20

// maxOverviewBytes bounds /neighbors and /summary bodies, which are decoded in
// one piece (they are small compared to a RIB).
const maxOverviewBytes = 1 << 20

// Client queries one configured bgplgd backend.
type Client struct {
	Visibility visibility.Policy
	BaseURL    *url.URL
	HTTP       *http.Client
	// Timeout bounds a whole query including reading the body.
	Timeout time.Duration
	// MaxResponseBytes bounds the upstream response body.
	MaxResponseBytes int64
	// MaxRetainedBytes bounds the estimated memory of decoded routes.
	MaxRetainedBytes int64
}

func NewClient(base string, timeout time.Duration, maxBytes int64) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid backend URL %q", base)
	}
	// No http.Client.Timeout: the per-query context deadline covers headers and
	// the streamed body, and lets us tell a timeout from other failures.
	return &Client{BaseURL: u, HTTP: &http.Client{}, Timeout: timeout, MaxResponseBytes: maxBytes, MaxRetainedBytes: DefaultMaxRetainedBytes}, nil
}

func (c *Client) endpointURL(path string, params url.Values) string {
	u := *c.BaseURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = params.Encode()
	return u.String()
}

// Search runs one compound /rib query and decodes the response as a stream.
// The returned error is non-nil only when the query is invalid and no request
// was made; every backend problem is reported through Result.Completion.
// Complete routes decoded before an interruption are preserved.
func (c *Client) Search(ctx context.Context, q Query) (Result, error) {
	return c.SearchWithReservation(ctx, q, nil)
}

// SearchWithReservation reserves estimated route memory before retention.
func (c *Client) SearchWithReservation(ctx context.Context, q Query, reserve func(int64) bool) (Result, error) {
	n, err := q.Normalize()
	if err != nil {
		return Result{}, err
	}
	params, _ := n.Params()
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	start := time.Now()
	var res Result
	decoded := false
	finish := func() Result {
		res.Elapsed = time.Since(start)
		res.RouteCount = len(res.Routes)
		return res
	}
	fail := func(reason Reason, err error) Result {
		res.Reason = reason
		res.Detail = c.describe(reason, err)
		if decoded {
			res.Status = StatusPartial
		} else {
			res.Status = StatusFailed
		}
		return finish()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpointURL("/rib", params), nil)
	if err != nil {
		return fail(ReasonBackendError, err), nil
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fail(c.classify(parent, ctx, err, true), err), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Status, res.Reason = StatusFailed, ReasonBackendError
		switch {
		case resp.StatusCode == http.StatusNotFound:
			res.Detail = fmt.Sprintf("Backend does not provide %s (HTTP 404); this bgplgd version may not support the endpoint", "/rib")
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			res.Detail = fmt.Sprintf("Backend rejected the query (HTTP %d)", resp.StatusCode)
		default:
			res.Detail = fmt.Sprintf("Backend returned HTTP %d", resp.StatusCode)
		}
		return finish(), nil
	}

	body := &boundedReader{r: resp.Body, max: c.MaxResponseBytes}
	maxRetained := c.MaxRetainedBytes
	if maxRetained <= 0 {
		maxRetained = DefaultMaxRetainedBytes
	}
	sink := func(r Route) error {
		if c.Visibility.Excludes(r.Prefix) {
			decoded = true
			return nil
		}
		size := r.MemSize()
		if res.MemBytes+size > maxRetained {
			return errMemoryLimit
		}
		if reserve != nil && !reserve(size) {
			return errCapacity
		}
		decoded = true
		res.MemBytes += size
		if len(res.Routes) == 0 {
			res.FirstRoute = time.Since(start)
		}
		res.Routes = append(res.Routes, r)
		return nil
	}
	failed, derr := decodeStream(body, sink)
	res.Bytes = body.n
	if derr == nil && body.err == nil {
		if failed {
			res.Status, res.Reason, res.Detail = StatusFailed, ReasonBackendError, "backend reported a failure"
			return finish(), nil
		}
		res.Status = StatusComplete
		return finish(), nil
	}
	cause := derr
	if body.err != nil {
		cause = body.err // the real reason the body ended early
	}
	if body.n == 0 && body.err == nil && errors.Is(derr, errTruncated) {
		// bgplgd answers 200 with an empty body when bgpctl rejects the query
		// (for example an invalid ext-community or an unknown neighbor or
		// group). A real "no matches" answer is a small JSON document.
		res.Status, res.Reason = StatusFailed, ReasonBackendError
		res.Detail = "Backend returned an empty response; OpenBGPD probably rejected the query (check ext-community syntax and neighbor or group names)"
		return finish(), nil
	}
	return fail(c.classify(parent, ctx, cause, false), cause), nil
}

// classify maps an error to a Reason. A response limit or memory limit wins
// over context errors because the limit is what ended the read.
func (c *Client) classify(parent, ctx context.Context, err error, request bool) Reason {
	switch {
	case errors.Is(err, ErrResponseLimit):
		return ReasonResponseLimit
	case errors.Is(err, errMemoryLimit):
		return ReasonMemoryLimit
	case errors.Is(err, errCapacity):
		return ReasonCapacity
	case errors.Is(parent.Err(), context.Canceled):
		return ReasonCanceled
	case ctx.Err() != nil, errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ReasonTimeout
	}
	switch {
	case errors.Is(err, errStructure), errors.Is(err, errTrailing), isSyntaxError(err):
		return ReasonInvalidJSON
	case errors.Is(err, errTruncated), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return ReasonConnectionClosed
	}
	if request {
		return ReasonBackendError
	}
	return ReasonConnectionClosed
}

func (c *Client) describe(reason Reason, err error) string {
	switch reason {
	case ReasonTimeout:
		return fmt.Sprintf("Backend timeout after %s", c.Timeout.Round(time.Millisecond))
	case ReasonResponseLimit:
		return fmt.Sprintf("Response exceeded the configured limit of %s", FormatBytes(c.MaxResponseBytes))
	case ReasonMemoryLimit:
		max := c.MaxRetainedBytes
		if max <= 0 {
			max = DefaultMaxRetainedBytes
		}
		return fmt.Sprintf("Retained-result memory limit of %s reached", FormatBytes(max))
	case ReasonCapacity:
		return "Server memory capacity is shared by many queries and is exhausted right now; retry shortly (this is not the per-query limit)"
	case ReasonConnectionClosed:
		return "Connection closed before the response was complete"
	case ReasonInvalidJSON:
		if errors.Is(err, errTrailing) {
			return "Backend sent unexpected data after the JSON document"
		}
		return "Backend returned malformed or unexpected JSON"
	case ReasonCanceled:
		return "Request cancelled"
	case ReasonBackendError:
		return "Could not query the backend"
	}
	return string(reason)
}

// FormatBytes renders a byte count with binary units.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func isSyntaxError(err error) bool {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	return errors.As(err, &syn) || errors.As(err, &typ)
}
