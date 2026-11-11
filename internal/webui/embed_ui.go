//go:build embedui

// Package webui holds the built React app. Build the server with
// -tags embedui after `npm run build` in web/ to include it.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var files embed.FS

// FS returns the built app, rooted at its index.html.
func FS() fs.FS {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
