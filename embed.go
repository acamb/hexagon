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

// BaseDockerfile is the reference session image. The UI offers it as the
// starting point when defining a new image, so the file on disk stays the one
// source of truth for what a session container needs.
//
//go:embed deploy/images/base/Dockerfile
var BaseDockerfile string

// BaseCompose is the reference compose file for an advanced image, offered as
// the starting point beside the Dockerfile. An empty editor with a list of
// rules attached is a worse place to start than two services commented out.
//
//go:embed deploy/images/base/compose.yaml
var BaseCompose string
