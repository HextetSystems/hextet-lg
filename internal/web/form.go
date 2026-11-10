package web

import (
	"html/template"

	"github.com/HextetSystems/hextet-lg/internal/bgp"
)

// Field describes one advanced filter control. Fields are data so that the
// form and its hints stay in one place.
type Field struct {
	Key, Label, Placeholder, Hint string
	Options                       []string // non-empty: render a <select>
	Group                         string   // fields in one group are mutually exclusive
}

// FormGroup bundles related controls under a heading.
type FormGroup struct {
	Title  string
	Fields []Field
	Flags  []Field
}

var formGroups = []FormGroup{
	{Title: "Route attributes", Fields: []Field{
		{Key: bgp.KeyPrefix, Label: "Prefix", Placeholder: "192.0.2.0/24 or 2001:db8::/32"},
		{Key: bgp.KeyAS, Label: "Source ASN", Placeholder: "AS15169", Hint: "OpenBGPD's source-AS filter, not AS-path membership."},
		{Key: bgp.KeyAF, Label: "Address family", Options: []string{"ipv4", "ipv6", "vpnv4", "vpnv6"}, Hint: "Leave on any to get IPv4 and IPv6."},
		{Key: bgp.KeyRIB, Label: "RIB table name", Placeholder: "Loc-RIB", Hint: "OpenBGPD routing table name."},
	}},
	{Title: "Communities (kept distinct)", Fields: []Field{
		{Key: bgp.KeyCommunity, Label: "Standard community", Placeholder: "65000:100 or NO_EXPORT", Hint: "RFC 1997, 16-bit fields."},
		{Key: bgp.KeyExtCommunity, Label: "Extended community", Placeholder: "rt 65000:1", Hint: "Grammar is validated by OpenBGPD."},
		{Key: bgp.KeyLargeCommunity, Label: "Large community", Placeholder: "395089:0:6", Hint: "RFC 8092, x:x:x."},
	}},
	{Title: "Peer (choose one)", Fields: []Field{
		{Key: bgp.KeyNeighbor, Label: "Neighbor", Placeholder: "IPv4/IPv6 address or description", Group: "peer"},
		{Key: bgp.KeyGroup, Label: "Peer group", Placeholder: "group name", Group: "peer"},
	}},
	{Title: "Validation state", Fields: []Field{
		{Key: bgp.KeyOVS, Label: "RPKI origin validation (OVS)", Options: []string{"valid", "invalid", "not-found"}},
		{Key: bgp.KeyAVS, Label: "ASPA validation (AVS)", Options: []string{"valid", "invalid", "unknown"}},
	}},
	{Title: "Route state (choose one)", Flags: []Field{
		{Key: bgp.KeyBest, Label: "Best routes only", Group: "state"},
		{Key: bgp.KeyError, Label: "Treated as withdrawn (error)", Group: "state"},
		{Key: bgp.KeyFiltered, Label: "Filtered by input filter", Group: "state"},
		{Key: bgp.KeyInvalid, Label: "Not eligible (invalid)", Group: "state"},
		{Key: bgp.KeyLeaked, Label: "Route leak detected", Group: "state"},
	}},
	{Title: "Prefix mode (CIDR only; leave both unchecked for exact prefix)", Flags: []Field{
		{Key: bgp.KeyAll, Label: "Include more specifics", Group: "scope"},
		{Key: bgp.KeyOrShorter, Label: "Include covering prefixes", Group: "scope"},
	}},
}

func selected(a, b string) template.HTMLAttr {
	if a == b && a != "" {
		return "selected"
	}
	return ""
}

func checked(v bool) template.HTMLAttr {
	if v {
		return "checked"
	}
	return ""
}
