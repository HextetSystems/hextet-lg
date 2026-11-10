package bgp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOverviewDecoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"neighbors":[{"remote_as":"64500","remote_addr":"192.0.2.1","state":"Established","stats":{"prefixes":{"received":0}}},{"state":"Idle"}]}`))
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, time.Second, 4096)
	for _, endpoint := range []string{"neighbors", "summary"} {
		o, err := c.Overview(context.Background(), endpoint)
		if err != nil || len(o.Neighbors) != 2 || o.Established != 1 || o.Other != 1 || o.Neighbors[0].Stats.Prefixes.Received == nil || o.Neighbors[1].Stats != nil {
			t.Fatalf("%s: %#v %v", endpoint, o, err)
		}
	}
}

// Shape of `bgpctl -j show neighbor` (OpenBGPD 9.3, output_json.c); config,
// capability details and the local side are trimmed.
const neighborsFull = `{
	"neighbors": [
		{
			"remote_as": "64500",
			"description": "peer-a",
			"group": "transit",
			"remote_addr": "192.0.2.1",
			"bgpid": "192.0.2.1",
			"state": "Established",
			"last_updown": "01:23:45",
			"last_updown_sec": 5025,
			"config": {"passive": false},
			"stats": {
				"last_read": "00:00:12", "last_read_sec": 12,
				"last_write": "00:00:03", "last_write_sec": 3,
				"prefixes": { "sent": 7, "received": 0 },
				"message": {
					"sent": {"open": 1, "notifications": 0, "updates": 9, "keepalives": 40, "route_refresh": 0, "total": 50},
					"received": {"open": 1, "notifications": 0, "updates": 11, "keepalives": 41, "route_refresh": 0, "total": 53}
				},
				"update": {
					"sent": { "updates": 9, "withdraws": 1, "eor": 2 },
					"received": { "updates": 11, "withdraws": 0, "eor": 2 }
				}
			},
			"last_shutdown_reason": "maintenance <b>window</b>",
			"last_error_sent": "Cease (Administrative Reset)",
			"session": {
				"holdtime": 90,
				"keepalive": 30,
				"local": {"address": "192.0.2.2", "port": 179},
				"capabilities": {"as4byte": true, "multiprotocol": ["IPv4 unicast", "IPv6 unicast"]}
			}
		},
		{
			"remote_as": "64501",
			"remote_addr": "2001:db8::2",
			"state": "Active",
			"last_updown": "Never",
			"last_updown_sec": 0,
			"stats": {"prefixes": {"sent": 0, "received": 0}},
			"session": "not-an-object"
		}
	]
}`

func TestNeighborDetailFieldsDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(neighborsFull)) }))
	defer srv.Close()
	c, _ := NewClient(srv.URL, time.Second, 1<<20)
	o, err := c.Overview(context.Background(), "neighbors")
	if err != nil || len(o.Neighbors) != 2 || o.Established != 1 || o.Other != 1 {
		t.Fatalf("%#v %v", o, err)
	}
	a, b := o.Neighbors[0], o.Neighbors[1]
	if a.Group != "transit" || a.BGPID != "192.0.2.1" || *a.LastUpDownSec != 5025 || a.LastShutdownReason == "" || a.LastErrorSent != "Cease (Administrative Reset)" || a.LastErrorReceived != "" {
		t.Fatalf("%+v", a)
	}
	st := a.Stats
	if st.LastRead != "00:00:12" || *st.Prefixes.Sent != 7 || *st.Prefixes.Received != 0 || *st.Message.Received.Total != 53 || *st.Message.Sent.Updates != 9 || *st.Update.Sent.Withdraws != 1 || *st.Update.Received.EOR != 2 {
		t.Fatalf("%+v", st)
	}
	if a.Session == nil || *a.Session.HoldTime != 90 || *a.Session.Keepalive != 30 || len(a.Session.Capabilities.Multiprotocol) != 2 || a.Session.Capabilities.Multiprotocol[1] != "IPv6 unicast" {
		t.Fatalf("%+v", a.Session)
	}
	// An unexpected session shape is dropped; the neighbor still decodes.
	if b.Session != nil || b.Address != "2001:db8::2" || b.State != "Active" || b.Stats.Message != nil || b.Stats.Update != nil || b.Group != "" || b.BGPID != "" {
		t.Fatalf("%+v", b)
	}
	if a.Raw == "" || !strings.Contains(a.Raw, `"192.0.2.2"`) {
		t.Fatal("raw neighbor JSON must still be retained")
	}
}

func TestSummaryHasNoStatsAndStaysAbsent(t *testing.T) {
	// /summary is the basic-only document: absent stats must stay nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"neighbors":[{"remote_as":"64500","description":"x","remote_addr":"192.0.2.1","bgpid":"192.0.2.1","state":"Established","last_updown":"1d02h","last_updown_sec":93600}]}`))
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, time.Second, 4096)
	o, err := c.Overview(context.Background(), "summary")
	if err != nil || o.Neighbors[0].Stats != nil || o.Neighbors[0].Session != nil {
		t.Fatalf("%+v %v", o, err)
	}
}
