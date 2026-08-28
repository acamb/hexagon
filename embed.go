// Package hexagon embeds the built frontend so the server ships as one binary.
package hexagon

import (
	"embed"
	"io/fs"
)

// distFS holds web/dist, produced by `npm run build` in web/. The `all:` prefix
// keeps dotfiles such as .gitkeep, so the tree exists even before a build.
//
//go:embed all:web/dist
var distFS embed.FS

// FrontendFS returns the built frontend rooted at web/dist.
func FrontendFS() (fs.FS, error) {
	return fs.Sub(distFS, "web/dist")
}
