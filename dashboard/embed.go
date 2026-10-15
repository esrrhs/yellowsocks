// Package dashboard carries the statistics page.
//
// The page is deliberately standalone: one HTML file, no build step and no
// external requests (no CDN, no web fonts, no chart library), so it can be
// opened from disk or dropped on any static host and pointed at an API
// address in its settings panel. It lives here so the relay can still serve
// it with no files on disk next to the binary.
package dashboard

import _ "embed"

//go:embed index.html
var page []byte

// HTML returns the dashboard page.
func HTML() []byte { return page }
