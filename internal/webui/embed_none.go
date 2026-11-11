//go:build !embedui

// Package webui holds the built React app. Build the server with
// -tags embedui after `npm run build` in web/ to include it.
package webui

import "io/fs"

// FS returns nil: this binary was built without the UI. In development the
// Vite dev server serves the UI and proxies the API.
func FS() fs.FS { return nil }
