package bgp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Shapes below are what bgpctl -j show rtr prints (OpenBGPD 9.3, output_json.c).
const rtrConnected = `{
	"rtrs": [
		{
			"descr": "routinator-a",
			"remote_addr": "192.0.2.100",
			"remote_port": 323,
			"local_addr": "192.0.2.1",
			"state": "established",
			"version": 2,
			"minimal_version": 1,
			"session_id": 7,
			"serial": 4242,
			"refresh": 3600,
			"retry": 600,
			"expire": 7200,
			"timers": [
				{ "name": "RTR RefreshTimer", "due": 3100 },
				{ "name": "RTR ExpireTimer", "due": 7000 }
			]
		},
		{
			"remote_addr": "2001:db8::100",
			"remote_port": 3323,
			"state": "closed",
			"refresh": 3600,
			"retry": 600,
			"expire": 7200,
			"last_recv_error": "Transport Error",
			"last_recv_msg": "connection refused"
		}
	]
}
`

func rtrClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, time.Second, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func doc(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rtr" || r.URL.RawQuery != "" || r.Method != http.MethodGet {
			http.Error(w, "unexpected", 500)
			return
		}
		_, _ = w.Write([]byte(body))
	}
}

func TestRTRSessionsDecoded(t *testing.T) {
	res := rtrClient(t, doc(rtrConnected)).RTR(context.Background())
	if res.State != RTRSessions || len(res.Sessions) != 2 || res.Reason != ReasonNone {
		t.Fatalf("%+v", res)
	}
	a, b := res.Sessions[0], res.Sessions[1]
	if a.Description != "routinator-a" || a.State != "established" || *a.Version != 2 || *a.MinimalVersion != 1 || *a.SessionID != 7 || *a.Serial != 4242 || *a.Refresh != 3600 || *a.Retry != 600 || *a.Expire != 7200 || *a.RemotePort != 323 || len(a.Timers) != 2 || *a.Timers[0].Due != 3100 || a.HasError() {
		t.Fatalf("%+v", a)
	}
	// A session that never exchanged data has no version/session/serial: they
	// must stay absent rather than become zero.
	if b.State != "closed" || b.Version != nil || b.SessionID != nil || b.Serial != nil || !b.HasError() || b.LastRecvError != "Transport Error" || b.LastRecvMsg != "connection refused" || b.RemoteAddr != "2001:db8::100" {
		t.Fatalf("%+v", b)
	}
}

func TestRTRZeroSessionsIsNotAnError(t *testing.T) {
	// bgpctl prints an empty object when no rtr directive is configured.
	for _, body := range []string{"{\n\n}\n", `{}`, `{"status":"OK"}`, `{"rtrs":[]}`} {
		res := rtrClient(t, doc(body)).RTR(context.Background())
		if res.State != RTRNoSessions || len(res.Sessions) != 0 {
			t.Errorf("%q: %+v", body, res)
		}
	}
}

func TestRTRMissingOptionalFields(t *testing.T) {
	res := rtrClient(t, doc(`{"rtrs":[{"remote_addr":"192.0.2.9"},{"descr":"only-a-label"}]}`)).RTR(context.Background())
	if res.State != RTRSessions || len(res.Sessions) != 2 {
		t.Fatalf("%+v", res)
	}
	for _, s := range res.Sessions {
		if s.State != "" || s.Serial != nil || s.Refresh != nil || s.Timers != nil {
			t.Fatalf("absent fields invented: %+v", s)
		}
	}
}

func TestRTRUnsupportedUnavailableAndMalformedAreDistinct(t *testing.T) {
	status := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code); _, _ = w.Write([]byte(body)) }
	}
	cases := []struct {
		name  string
		h     http.HandlerFunc
		state RTRState
		why   Reason
	}{
		{"404 means unsupported", status(404, "not found"), RTRUnsupported, ReasonNone},
		{"500 is unavailable", status(500, "boom"), RTRUnavailable, ReasonBackendError},
		{"503 is unavailable", status(503, ""), RTRUnavailable, ReasonBackendError},
		{"empty 200 is bgpctl rejection", status(200, ""), RTRMalformed, ReasonInvalidJSON},
		{"whitespace 200", status(200, " \n"), RTRMalformed, ReasonInvalidJSON},
		{"html from a proxy", status(200, "<html>hi</html>"), RTRMalformed, ReasonInvalidJSON},
		{"truncated json", status(200, `{"rtrs":[{"remote_addr":"192.0.2.1"`), RTRMalformed, ReasonInvalidJSON},
		{"trailing data", status(200, `{"rtrs":[]} {}`), RTRMalformed, ReasonInvalidJSON},
		{"json null", status(200, `null`), RTRMalformed, ReasonInvalidJSON},
		{"array root", status(200, `[]`), RTRMalformed, ReasonInvalidJSON},
		{"rtrs null", status(200, `{"rtrs":null}`), RTRMalformed, ReasonInvalidJSON},
		{"rtrs not array", status(200, `{"rtrs":{}}`), RTRMalformed, ReasonInvalidJSON},
		{"session not object", status(200, `{"rtrs":[1]}`), RTRMalformed, ReasonInvalidJSON},
		{"wrong field type", status(200, `{"rtrs":[{"serial":"x"}]}`), RTRMalformed, ReasonInvalidJSON},
		{"negative serial", status(200, `{"rtrs":[{"serial":-1}]}`), RTRMalformed, ReasonInvalidJSON},
		{"bad address", status(200, `{"rtrs":[{"remote_addr":"not-an-ip"}]}`), RTRMalformed, ReasonInvalidJSON},
		{"bad port", status(200, `{"rtrs":[{"remote_port":70000}]}`), RTRMalformed, ReasonInvalidJSON},
		{"reported failure", status(200, `{"status":"FAILED","error":"x"}`), RTRMalformed, ReasonInvalidJSON},
		{"some other document", status(200, `{"neighbors":[]}`), RTRMalformed, ReasonInvalidJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := rtrClient(t, tc.h).RTR(context.Background())
			if res.State != tc.state || res.Reason != tc.why || len(res.Sessions) != 0 || res.Detail == "" {
				t.Fatalf("%+v", res)
			}
		})
	}
}

func TestRTRUnreachableAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c, _ := NewClient(srv.URL, time.Second, 1<<20)
	srv.Close()
	if res := c.RTR(context.Background()); res.State != RTRUnavailable || res.Reason == ReasonNone {
		t.Fatalf("closed server: %+v", res)
	}

	slow := rtrClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	slow.Timeout = 50 * time.Millisecond
	if res := slow.RTR(context.Background()); res.State != RTRUnavailable || res.Reason != ReasonTimeout {
		t.Fatalf("timeout: %+v", res)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := rtrClient(t, doc(rtrConnected)).RTR(ctx); res.State != RTRUnavailable || res.Reason != ReasonCanceled {
		t.Fatalf("cancel: %+v", res)
	}
}

func TestRTRBounds(t *testing.T) {
	big := `{"rtrs":[{"descr":"` + strings.Repeat("a", 2<<20) + `"}]}`
	if res := rtrClient(t, doc(big)).RTR(context.Background()); res.State != RTRMalformed || res.Reason != ReasonResponseLimit {
		t.Fatalf("oversize: %+v", res)
	}
	var many strings.Builder
	many.WriteString(`{"rtrs":[`)
	for i := 0; i <= maxRTRSessions; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"remote_addr":"192.0.2.1"}`)
	}
	many.WriteString(`]}`)
	if res := rtrClient(t, doc(many.String())).RTR(context.Background()); res.State != RTRMalformed {
		t.Fatalf("too many sessions: %+v", res)
	}
}

func TestRTRTextIsCleaned(t *testing.T) {
	long := strings.Repeat("é", 400)
	res := rtrClient(t, doc(`{"rtrs":[{"descr":"a\u0000b\u001b[31m<script>","last_recv_msg":"`+long+`","state":"ok�"}]}`)).RTR(context.Background())
	if res.State != RTRSessions {
		t.Fatalf("%+v", res)
	}
	s := res.Sessions[0]
	if strings.ContainsAny(s.Description, "\x00\x1b") || !strings.Contains(s.Description, "<script>") {
		// Markup is kept (html/template escapes it); control bytes are not.
		t.Fatalf("description %q", s.Description)
	}
	if len(s.LastRecvMsg) > maxRTRText+len("…") || !strings.HasSuffix(s.LastRecvMsg, "…") {
		t.Fatalf("not bounded: %d", len(s.LastRecvMsg))
	}
}

func TestRTRRequestsOnlyTheFixedEndpoint(t *testing.T) {
	var path, query string
	c := rtrClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	})
	c.RTR(context.Background())
	if path != "/rtr" || query != "" {
		t.Fatalf("%s?%s", path, query)
	}
}
