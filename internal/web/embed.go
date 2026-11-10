package web

import "embed"

//go:embed templates/* static/*
var assets embed.FS
