package bgp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc, timeout time.Duration, maxBytes int64) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, timeout, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestClientBuildsCompoundUpstreamRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		q    Query
		path string
		want url.Values
	}{
		"simple prefix": {Query{Prefix: "8.8.8.8"}, "/rib", url.Values{"prefix": {"8.8.8.8"}}},
		"simple asn":    {SimpleQuery(SearchASN, "AS15169"), "/rib", url.Values{"as": {"15169"}}},
		"simple large":  {SimpleQuery(SearchLargeCommunity, "395089:0:6"), "/rib", url.Values{"large-community": {"395089:0:6"}}},
		"compound": {Query{AS: "15169", LargeCommunity: "395089:0:6", Community: "65000:1", ExtCommunity: "rt 65000:1", AF: "ipv6", OVS: "valid", AVS: "unknown", Best: true, RIB: "Loc-RIB", Neighbor: "peer a"}, "/rib",
			url.Values{"as": {"15169"}, "large-community": {"395089:0:6"}, "community": {"65000:1"}, "ext-community": {"rt 65000:1"}, "af": {"ipv6"}, "ovs": {"valid"}, "avs": {"unknown"}, "best": {"1"}, "rib": {"Loc-RIB"}, "neighbor": {"peer a"}}},
		"adj-rib-in":  {Query{Endpoint: EndpointRIBIn, Group: "g", Prefix: "10.0.0.0/8", OrShorter: true}, "/rib/in", url.Values{"group": {"g"}, "prefix": {"10.0.0.0/8"}, "or-shorter": {"1"}}},
		"adj-rib-out": {Query{Endpoint: EndpointRIBOut, Neighbor: "192.0.2.1", Leaked: true}, "/rib/out", url.Values{"neighbor": {"192.0.2.1"}, "leaked": {"1"}}},
	} {
		t.Run(name, func(t *testing.T) {
			var gotPath, gotRaw string
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotRaw = r.URL.Path, r.URL.RawQuery
				_, _ = w.Write([]byte(`{"rib":[{"prefix":"x"}]}`))
			}, time.Second, 1<<20)
			res, err := c.Search(context.Background(), tc.q)
			if err != nil || !res.IsComplete() || len(res.Routes) != 1 {
				t.Fatalf("%v %+v", err, res.Completion)
			}
			got, _ := url.ParseQuery(gotRaw)
			if gotPath != tc.path || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("path=%s query=%v", gotPath, gotRaw)
			}
			// Exactly one occurrence of every key, escaped by net/url.
			if strings.Count(gotRaw, "&")+1 != len(tc.want) {
				t.Fatalf("unexpected repeated parameters: %s", gotRaw)
			}
		})
	}
}

func TestSearchInvalidQueryMakesNoRequest(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }, time.Second, 1<<20)
	for _, q := range []Query{{}, {Neighbor: "a", Group: "b"}, {AS: "x"}, {All: true, AS: "1"}} {
		if _, err := c.Search(context.Background(), q); err == nil {
			t.Errorf("%+v must be rejected", q)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("invalid query reached the backend")
	}
}

func TestSearchComplete(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, newGen(2000, false)) }, 5*time.Second, 64<<20)
	res, err := c.Search(context.Background(), Query{LargeCommunity: "395089:0:6"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsComplete() || res.Reason != ReasonNone || res.RouteCount != 2000 || len(res.Routes) != 2000 || res.Bytes < 1<<20 || res.FirstRoute <= 0 || res.Elapsed < res.FirstRoute || res.MemBytes <= 0 {
		t.Fatalf("%+v", res.Completion)
	}
}

func TestSearchTimeoutAfterSomeRoutesKeepsThem(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{\"rib\":[\n"+genRoute(1, false)+",\n"+genRoute(2, false)+",\n"+genRoute(3, false)+",\n"+`{"prefix":"10.9.9.0/24","aspa`)
		flush(w)
		<-r.Context().Done()
	}, 150*time.Millisecond, 1<<20)
	res, err := c.Search(context.Background(), Query{AS: "64500"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusPartial || res.Reason != ReasonTimeout || len(res.Routes) != 3 || res.RouteCount != 3 {
		t.Fatalf("%+v", res.Completion)
	}
	for _, r := range res.Routes {
		if r.Prefix == "10.9.9.0/24" {
			t.Fatal("incomplete route object was kept")
		}
	}
	if !strings.Contains(res.Detail, "timeout") {
		t.Fatal(res.Detail)
	}
}

func TestSearchTimeoutBeforeAnyRoute(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"no headers": func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
		"headers only": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"rib":[`)
			flush(w)
			<-r.Context().Done()
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, h, 100*time.Millisecond, 1<<20)
			start := time.Now()
			res, _ := c.Search(context.Background(), Query{AS: "64500"})
			if res.Status != StatusFailed || res.Reason != ReasonTimeout || len(res.Routes) != 0 || time.Since(start) > 2*time.Second {
				t.Fatalf("%+v", res.Completion)
			}
		})
	}
}

func TestSearchResponseLimitKeepsCompleteRoutes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, newGen(100000, false)) }, 5*time.Second, 20_000)
	res, _ := c.Search(context.Background(), Query{LargeCommunity: "395089:0:6"})
	if res.Status != StatusPartial || res.Reason != ReasonResponseLimit || res.Bytes != 20_000 || len(res.Routes) == 0 || len(res.Routes) >= 100 {
		t.Fatalf("%+v", res.Completion)
	}
	if !strings.Contains(res.Detail, "limit") {
		t.Fatal(res.Detail)
	}
	// Document that ends exactly at the limit is still complete.
	doc := `{"rib":[{"prefix":"a"}]}`
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, doc) }, time.Second, int64(len(doc)))
	if res, _ = c.Search(context.Background(), Query{AS: "1"}); !res.IsComplete() {
		t.Fatalf("exact-fit response: %+v", res.Completion)
	}
}

func TestSearchResponseLimitBeforeFirstRoute(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, newGen(10, false)) }, time.Second, 100)
	res, _ := c.Search(context.Background(), Query{AS: "1"})
	if res.Status != StatusFailed || res.Reason != ReasonResponseLimit {
		t.Fatalf("%+v", res.Completion)
	}
}

func TestSearchMemoryLimitReturnsPartial(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, newGen(10000, false)) }, 5*time.Second, 64<<20)
	c.MaxRetainedBytes = 6000
	res, _ := c.Search(context.Background(), Query{LargeCommunity: "395089:0:6"})
	if res.Status != StatusPartial || res.Reason != ReasonMemoryLimit || len(res.Routes) == 0 || len(res.Routes) >= 20 || res.MemBytes > 6000 {
		t.Fatalf("%+v", res.Completion)
	}
	if res.Bytes > 100_000 {
		t.Fatalf("reading must stop at the memory limit, read %d bytes", res.Bytes)
	}
}

func TestSearchConnectionTerminated(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = io.WriteString(w, "{\"rib\":[\n"+genRoute(1, false)+",\n"+genRoute(2, false)+",\n{\"prefix\":\"half")
		flush(w)
		panic(http.ErrAbortHandler)
	}, 5*time.Second, 1<<20)
	res, _ := c.Search(context.Background(), Query{AS: "64500"})
	if res.Status != StatusPartial || res.Reason != ReasonConnectionClosed || len(res.Routes) != 2 {
		t.Fatalf("%+v", res.Completion)
	}
}

func TestSearchTruncatedWithoutLengthIsNotComplete(t *testing.T) {
	// A server closing cleanly in the middle of the document (no Content-Length
	// check possible) must still be detected by JSON validation.
	for name, tc := range map[string]struct {
		body   string
		routes int
	}{
		"missing closing array": {`{"rib":[{"prefix":"a"},{"prefix":"b"}`, 2},
		"cut mid route":         {`{"rib":[{"prefix":"a"},{"prefix":"b","aspath":`, 1},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }, time.Second, 1<<20)
			res, _ := c.Search(context.Background(), Query{AS: "1"})
			if res.IsComplete() || res.Status != StatusPartial || res.Reason != ReasonConnectionClosed || len(res.Routes) != tc.routes {
				t.Fatalf("%+v", res.Completion)
			}
		})
	}
}

func TestSearchMalformedResponses(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		status Status
		reason Reason
		routes int
	}{
		"invalid after routes": {`{"rib":[{"prefix":"a"},{"prefix":}]}`, StatusPartial, ReasonInvalidJSON, 1},
		"invalid at start":     {`<html>not json</html>`, StatusFailed, ReasonInvalidJSON, 0},
		"wrong document":       {`{"neighbors":[]}`, StatusFailed, ReasonInvalidJSON, 0},
		"trailing data":        {`{"rib":[{"prefix":"a"}]} junk`, StatusPartial, ReasonInvalidJSON, 1},
		"empty body":           {``, StatusFailed, ReasonBackendError, 0},
		"backend failed":       {`{"status":"FAILED","error":"x"}`, StatusFailed, ReasonBackendError, 0},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }, time.Second, 1<<20)
			res, _ := c.Search(context.Background(), Query{AS: "1"})
			if res.IsComplete() || res.Status != tc.status || res.Reason != tc.reason || len(res.Routes) != tc.routes {
				t.Fatalf("%+v", res.Completion)
			}
		})
	}
}

func TestSearchHTTPErrorsAreFailures(t *testing.T) {
	for code, want := range map[int]string{400: "rejected the query", 404: "does not provide /rib (HTTP 404)", 503: "HTTP 503"} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret backend detail 10.1.2.3", code) }, time.Second, 1<<20)
		res, _ := c.Search(context.Background(), Query{AS: "1"})
		if res.Status != StatusFailed || res.Reason != ReasonBackendError || !strings.Contains(res.Detail, want) || strings.Contains(res.Detail, "secret") {
			t.Fatalf("%d: %+v", code, res.Completion)
		}
	}
}

func TestSearchConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c, _ := NewClient(srv.URL, time.Second, 1<<20)
	srv.Close()
	res, _ := c.Search(context.Background(), Query{AS: "1"})
	if res.Status != StatusFailed || res.Reason != ReasonBackendError || strings.Contains(res.Detail, "127.0.0.1") {
		t.Fatalf("%+v", res.Completion)
	}
}

func TestSearchCancellation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{\"rib\":[\n"+genRoute(1, false)+",\n")
		flush(w)
		<-r.Context().Done()
	}, 10*time.Second, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	res, _ := c.Search(ctx, Query{AS: "1"})
	if res.Reason != ReasonCanceled || res.Status != StatusPartial || len(res.Routes) != 1 || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v", res.Completion)
	}
	pre, precancel := context.WithCancel(context.Background())
	precancel()
	if res, _ = c.Search(pre, Query{AS: "1"}); res.Reason != ReasonCanceled || res.Status != StatusFailed {
		t.Fatalf("%+v", res.Completion)
	}
}

func TestNoFalseCompleteStatus(t *testing.T) {
	// Every interrupted scenario must be non-complete.
	bodies := []string{`{"rib":[{"prefix":"a"}`, `{"rib":[{"prefix":"a"},{"pre`, `{"rib":[{"prefix":"a"}]`, `[{"prefix":"a"}`}
	for i, b := range bodies {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, b) }, time.Second, 1<<20)
		if res, _ := c.Search(context.Background(), Query{AS: "1"}); res.IsComplete() {
			t.Errorf("body %d reported complete", i)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{10: "10 B", 1536: "1.5 KiB", 128 << 20: "128.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("%d => %s", n, got)
		}
	}
	_ = fmt.Sprint
}

// bgplgd answers 200 with an empty body when bgpctl rejects the query; that
// must never look like "no matching routes" or like a dropped connection.
func TestSearchEmptyBodyMeansRejectedQuery(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "application/json") }, time.Second, 1<<20)
	res, _ := c.Search(context.Background(), Query{ExtCommunity: "bogus"})
	if res.Status != StatusFailed || res.Reason != ReasonBackendError || !strings.Contains(res.Detail, "empty response") || !strings.Contains(res.Detail, "rejected the query") {
		t.Fatalf("%+v", res.Completion)
	}
	// ...whereas bgpctl's real empty answer is a complete, empty result.
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{\n  }\n") }, time.Second, 1<<20)
	if res, _ = c.Search(context.Background(), Query{AS: "1"}); !res.IsComplete() || len(res.Routes) != 0 {
		t.Fatalf("%+v", res.Completion)
	}
}

func TestSearchAdjRIBNotFoundIsExplained(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rib/in" {
			t.Errorf("path %s", r.URL.Path)
		}
		http.NotFound(w, r)
	}, time.Second, 1<<20)
	res, _ := c.Search(context.Background(), Query{Endpoint: EndpointRIBIn, Neighbor: "x"})
	if res.Status != StatusFailed || !strings.Contains(res.Detail, "/rib/in") || !strings.Contains(res.Detail, "may not support") {
		t.Fatalf("%+v", res.Completion)
	}
}

// Aggregate-budget exhaustion is a different condition from the per-query
// limit: it is transient, has its own typed reason, and its message must not
// claim a per-query limit was reached.
func TestAggregateBudgetExhaustionIsCapacityNotMemoryLimit(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, newGen(10000, false)) }, 5*time.Second, 64<<20)
	var granted int64
	reserve := func(n int64) bool {
		if granted+n > 6000 {
			return false
		}
		granted += n
		return true
	}
	res, err := c.SearchWithReservation(context.Background(), Query{LargeCommunity: "395089:0:6"}, reserve)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusPartial || res.Reason != ReasonCapacity || len(res.Routes) == 0 {
		t.Fatalf("%+v", res.Completion)
	}
	if strings.Contains(res.Detail, "limit of") || !res.Reason.Transient() {
		t.Fatalf("aggregate exhaustion described as a per-query limit: %q", res.Detail)
	}
	// The per-query limit stays deterministic and wins when both would apply.
	c.MaxRetainedBytes = 3000
	granted = 0
	res, _ = c.SearchWithReservation(context.Background(), Query{LargeCommunity: "395089:0:6"}, reserve)
	if res.Reason != ReasonMemoryLimit || res.Reason.Transient() {
		t.Fatalf("%+v", res.Completion)
	}
}

func TestReasonTransient(t *testing.T) {
	for r, want := range map[Reason]bool{
		ReasonCapacity: true, ReasonBusy: true, ReasonCanceled: true,
		ReasonNone: false, ReasonTimeout: false, ReasonResponseLimit: false, ReasonMemoryLimit: false,
		ReasonConnectionClosed: false, ReasonInvalidJSON: false, ReasonBackendError: false,
	} {
		if r.Transient() != want {
			t.Errorf("%q.Transient() = %v, want %v", r, !want, want)
		}
	}
}
