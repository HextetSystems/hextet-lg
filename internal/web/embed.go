package web

import "embed"

//go:embed templates/* static/* communities.yaml
var assets embed.FS
