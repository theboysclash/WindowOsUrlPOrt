// Package web holds the browser-side assets (templates, scripts, noVNC) that
// are compiled into the executable.
package web

import "embed"

//go:embed static templates
var FS embed.FS
