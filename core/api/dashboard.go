package api

import (
	_ "embed"
)

// dashboardHTML is the built-in single page dashboard. It is served at "/"
// when Config.Dashboard is set, and calls the same API as any other client.
//
//go:embed dashboard.html
var dashboardHTMLBytes []byte

func dashboardHTML() []byte { return dashboardHTMLBytes }
