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

// Neighbor follows bgpctl's JSON output. Pointers preserve absent counters.
type Neighbor struct {
	RemoteAS    string `json:"remote_as"`
	Address     string `json:"remote_addr"`
	Description string `json:"description"`
	State       string `json:"state"`
	LastUpDown  string `json:"last_updown"`
	Stats       *struct {
		Prefixes *struct {
			Sent     *int64 `json:"sent"`
			Received *int64 `json:"received"`
		} `json:"prefixes"`
		Message *struct {
			Sent *struct {
				Total *int64 `json:"total"`
			} `json:"sent"`
			Received *struct {
				Total *int64 `json:"total"`
			} `json:"received"`
		} `json:"message"`
	} `json:"stats"`
	Raw string `json:"-"`
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
