package web

import (
	"fmt"
	"math"
	"net/http"
	"time"
)

// Page generation timing.
//
// The footer shows how long the server took to generate the page. The clock
// starts when protect() begins handling the request (the same instant the HTTP
// duration metric and the access log use, see reqInfo.start) and is read
// exactly once, by the footer template (the pageTiming template function) at
// the moment the footer is executed. HTML responses are streamed straight from
// the template into the response writer, so the footer is the last part to be
// produced: everything before it (parsing, validation, admission, cache lookup
// or shared-flight wait, backend queries, visibility, pagination, enrichment
// scheduling and all earlier template output) is included. Nothing is buffered
// to make that possible.
//
// What it is not: browser or network time, the footer's own few bytes, or
// background ASN/PTR lookups, which never block the response.

// requestStart returns the instant the current request began (monotonic), or
// the zero time when the request did not pass through protect.
func requestStart(r *http.Request) time.Time {
	if i := infoFrom(r.Context()); i != nil {
		return i.start
	}
	return time.Time{}
}

// pageTiming is the template function behind the footer. It returns the
// formatted elapsed time since started, or "" when there is nothing accurate to
// show (no start recorded), in which case the footer omits the indicator.
func pageTiming(started time.Time) string {
	if started.IsZero() {
		return ""
	}
	return FormatGenerated(time.Since(started))
}

// FormatGenerated renders a duration with a fixed policy:
//
//	under 0.05 ms     "<0.1 ms"
//	under 100 ms      one decimal    "8.4 ms", "42.7 ms"
//	under 1 s         whole ms       "836 ms"
//	under 10 s        two decimals   "1.24 s"
//	under 100 s       one decimal    "12.3 s"
//	otherwise         whole seconds  "134 s"
//
// A value is rounded before its unit is chosen, so 99.96 ms is "100 ms" and
// 999.6 ms is "1.00 s", never "100.0 ms" or "1000 ms". Negative durations (which
// the monotonic clock cannot produce) give "".
func FormatGenerated(d time.Duration) string {
	if d < 0 {
		return ""
	}
	ms := float64(d) / float64(time.Millisecond)
	if ms < 0.05 {
		return "<0.1 ms"
	}
	if r := round(ms, 1); r < 100 {
		return fmt.Sprintf("%.1f ms", r)
	}
	if r := round(ms, 0); r < 1000 {
		return fmt.Sprintf("%.0f ms", r)
	}
	s := ms / 1000
	if r := round(s, 2); r < 10 {
		return fmt.Sprintf("%.2f s", r)
	}
	if r := round(s, 1); r < 100 {
		return fmt.Sprintf("%.1f s", r)
	}
	return fmt.Sprintf("%.0f s", round(s, 0))
}

func round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}
