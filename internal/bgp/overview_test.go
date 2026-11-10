package bgp

import (
	"context"
	"net/http"
	"net/http/httptest"
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
