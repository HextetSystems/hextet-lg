package web

import (
	"github.com/HextetSystems/hextet-lg/internal/bgp"
	"github.com/HextetSystems/hextet-lg/internal/config"
)

// Server-side presentation settings (the optional "display" configuration).
// Nothing here touches the backend request, route parsing, cache keys or the
// search filters: the same decoded routes are rendered with more or fewer
// columns, and a hidden column's values remain in the Route details.

// baseRouteColumns is the number of always-present route table columns:
// prefix, AS path, origin ASN, next hop, local preference, MED and age.
const baseRouteColumns = 7

// RouteCols is the resolved column layout of one route table.
type RouteCols struct {
	Neighbor, RPKI, ASPA bool
	// NeighborFormat is ip, name or both.
	NeighborFormat string
	// Count is the number of table columns, for the details row's colspan.
	Count int
}

func (h *Handler) routeCols() RouteCols {
	rc := h.cfg.Display.RouteColumns
	c := RouteCols{Neighbor: rc.Neighbor, RPKI: rc.RPKIColumn(), ASPA: rc.ASPAColumn(), NeighborFormat: rc.Format(), Count: baseRouteColumns}
	for _, on := range []bool{c.Neighbor, c.RPKI, c.ASPA} {
		if on {
			c.Count++
		}
	}
	return c
}

// NeighborCell is the presentation of one route's neighbor: the link to the
// neighbor's routes plus the visible label. The label is display text only;
// the link always targets the backend-supplied address and the source backend.
type NeighborCell struct {
	URL, LinkTitle string
	// Text and Title describe the label. Title is set when a reverse DNS name
	// replaced or accompanies the address, so the numeric address stays
	// reachable as a tooltip.
	Text, Title string
	// PTR, when set, is the address the page asks the cache-only status
	// endpoint about; Format tells the script how to show a name that arrives.
	PTR, Format string
	// Fallback is shown when the backend supplied no usable neighbor address.
	Fallback string
}

// neighborCell builds the neighbor label for a route. Names are looked up in
// the shared PTR cache only (never blocking on DNS) and only while the
// Neighbor column is shown with the name or both format: otherwise the label
// is the plain address.
func (h *Handler) neighborCell(r bgp.Route, backend string) NeighborCell {
	c := NeighborCell{URL: neighborURL(r.NeighborAddress, backend), LinkTitle: neighborTitle(r.NeighborDescription, r.NeighborAddress, ""), Text: r.NeighborAddress, Fallback: r.Neighbor}
	if c.Fallback == "" {
		c.Fallback = "—"
	}
	rc := h.cfg.Display.RouteColumns
	if !rc.NeighborNames() || h.ptr == nil {
		return c
	}
	key := h.ptrKey(r.NeighborAddress)
	if key == "" {
		return c
	}
	c.Format = rc.Format()
	name := h.ptrTitle(key)
	if name == "" {
		c.PTR = key
		return c
	}
	c.Title = name + " (" + key + ")"
	c.Text = name
	if c.Format == config.NeighborFormatBoth {
		c.Text = c.Title
	}
	return c
}
