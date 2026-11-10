package bgp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Neighbor follows bgpctl's JSON output. Pointers preserve absent counters:
// a missing value is never presented as zero.
//
// /summary carries only the basic fields (remote_as, description, group,
// remote_addr, bgpid, state, last_updown); /neighbors adds stats, the last
// error strings and, for sessions past OpenSent, the session block.
type Neighbor struct {
	RemoteAS      string         `json:"remote_as"`
	Address       string         `json:"remote_addr"`
	Description   string         `json:"description"`
	Group         string         `json:"group"`
	BGPID         string         `json:"bgpid"`
	State         string         `json:"state"`
	LastUpDown    string         `json:"last_updown"`
	LastUpDownSec *int64         `json:"last_updown_sec"`
	Stats         *NeighborStats `json:"stats"`
	// LastShutdownReason is the shutdown communication received from the peer
	// (RFC 8203); LastErrorSent/Received are the last NOTIFICATION codes.
	LastShutdownReason string `json:"last_shutdown_reason"`
	LastErrorSent      string `json:"last_error_sent"`
	LastErrorReceived  string `json:"last_error_received"`
	// Session is decoded best-effort: an unexpected shape there leaves it nil
	// and never fails the whole document.
	Session *NeighborSession `json:"-"`
	Raw     string           `json:"-"`
}

// NeighborStats is the "stats" object of /neighbors.
type NeighborStats struct {
	LastRead  string `json:"last_read"`
	LastWrite string `json:"last_write"`
	Prefixes  *struct {
		Sent     *int64 `json:"sent"`
		Received *int64 `json:"received"`
	} `json:"prefixes"`
	Message *struct {
		Sent     *MessageCounts `json:"sent"`
		Received *MessageCounts `json:"received"`
	} `json:"message"`
	Update *struct {
		Sent     *UpdateCounts `json:"sent"`
		Received *UpdateCounts `json:"received"`
	} `json:"update"`
}

// MessageCounts are bgpctl's per-direction BGP message counters.
type MessageCounts struct {
	Open          *int64 `json:"open"`
	Notifications *int64 `json:"notifications"`
	Updates       *int64 `json:"updates"`
	Keepalives    *int64 `json:"keepalives"`
	RouteRefresh  *int64 `json:"route_refresh"`
	Total         *int64 `json:"total"`
}

// UpdateCounts are the announced/withdrawn prefix counters of one direction.
type UpdateCounts struct {
	Updates   *int64 `json:"updates"`
	Withdraws *int64 `json:"withdraws"`
	EOR       *int64 `json:"eor"`
}

// NeighborSession is the subset of the "session" object that is shown.
// Local addresses, ports and authentication details are deliberately not
// decoded.
type NeighborSession struct {
	HoldTime  *int64 `json:"holdtime"`
	Keepalive *int64 `json:"keepalive"`
	// Capabilities are the negotiated ones.
	Capabilities *struct {
		// Multiprotocol lists the negotiated address families as bgpctl names
		// them ("IPv4 unicast", ...). This, not the neighbor's transport
		// address, says which families the session carries.
		Multiprotocol []string `json:"multiprotocol"`
	} `json:"capabilities"`
}

// UnmarshalJSON decodes the strict fields normally and the session block
// leniently.
func (n *Neighbor) UnmarshalJSON(b []byte) error {
	type plain Neighbor
	var aux struct {
		*plain
		Session json.RawMessage `json:"session"`
	}
	aux.plain = (*plain)(n)
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	n.Session = nil
	if len(aux.Session) > 0 {
		var s NeighborSession
		if json.Unmarshal(aux.Session, &s) == nil {
			n.Session = &s
		}
	}
	return nil
}

type Overview struct {
	Neighbors          []Neighbor
	Raw                string
	Established, Other int
}

func (c *Client) Overview(ctx context.Context, endpoint string) (Overview, error) {
	var out Overview
	if endpoint != "neighbors" && endpoint != "summary" {
		return out, errors.New("unsupported endpoint")
	}
	u := *c.BaseURL
	u.Path = strings.TrimRight(u.Path, "/") + "/" + endpoint
	u.RawQuery = ""
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return out, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	// Overview documents are decoded whole, so they get a tighter bound than RIB streams.
	limit := min(c.MaxResponseBytes, maxOverviewBytes)
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return out, err
	}
	if int64(len(body)) > limit {
		return out, errors.New("response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("backend HTTP %d", resp.StatusCode)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return out, err
	}
	if status, ok := root["status"]; ok && Flexible(status) == "FAILED" {
		return out, errors.New("backend returned failure")
	}
	var items []json.RawMessage
	if raw, ok := root["neighbors"]; ok {
		if err := json.Unmarshal(raw, &items); err != nil {
			return out, err
		}
	}
	for _, raw := range items {
		var n Neighbor
		if err := json.Unmarshal(raw, &n); err != nil {
			return out, err
		}
		n.Raw = string(raw)
		out.Neighbors = append(out.Neighbors, n)
		if strings.EqualFold(n.State, "established") {
			out.Established++
		} else {
			out.Other++
		}
	}
	out.Raw = string(body)
	return out, nil
}
