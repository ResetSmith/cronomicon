// Package web embeds the built frontend assets so the single Go binary serves
// the UI (T2/T3). The built frontend (`web/dist`) is tracked in git so
// `go build` works without Node; rebuild it with `cd frontend && npm run build`
// after any change under frontend/src or documentation/.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// DistFS returns the embedded frontend filesystem rooted at dist/.
func DistFS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// dist is always embedded; a failure here is a build-time bug.
		panic(err)
	}
	return sub
}
