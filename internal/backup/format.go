// Package backup owns the on-disk format of a Hexagon image backup: a
// gzip-compressed tar holding spec/image.json, spec/Dockerfile and
// spec/compose.yaml, and — when the image itself was asked to be included —
// image.tar.gz at the root, exactly as `docker save | gzip` would write it.
//
// It knows nothing about the store, Docker or HTTP: writing an archive from a
// spec and an image stream, reading the spec back out of one, and the
// refusals a hostile archive earns are all there is to it, which is what
// makes the refusals testable as a table. See
// plans/M3/02-image-backup-restore.md.
package backup

import "errors"

// FormatVersion is written into every manifest this package produces, and
// checked on the way back in: a future change to the layout is a refusal
// naming the mismatch rather than a confusing failure partway through an
// import.
const FormatVersion = 1

// The three files under spec/, and the image export at the archive root. These
// are the only member names this format ever writes, and the only ones a
// restore looks for; everything else in an archive is ignored.
const (
	memberManifest   = "spec/image.json"
	memberDockerfile = "spec/Dockerfile"
	memberCompose    = "spec/compose.yaml"
	memberImage      = "image.tar.gz"
)

// MaxSpecMember bounds spec/image.json, spec/Dockerfile and spec/compose.yaml,
// each read in full: the same limit a typed Dockerfile is held to elsewhere,
// and enforced as the bytes are decompressed rather than after, which is the
// only defence against a small file that expands forever.
const MaxSpecMember = 256 << 10

// maxMembers bounds how many entries a reader will look at before giving up.
// Nothing this format writes needs more than four; a hundred is already a
// sign the archive is not one of ours.
const maxMembers = 100

// ErrNoImage reports an archive that carries no image.tar.gz, from a caller
// that asked to import one.
var ErrNoImage = errors.New("backup: archive has no image.tar.gz")

// ErrFormatVersion reports a manifest whose format version this build does
// not understand.
var ErrFormatVersion = errors.New("backup: unsupported format version")

// Manifest is spec/image.json: everything a restore needs to know about the
// image that a Dockerfile's or a compose file's content alone cannot say —
// guessing the source type from which files are present would work until the
// first compose image whose compose file happens to be empty.
type Manifest struct {
	FormatVersion int    `json:"formatVersion"`
	Name          string `json:"name"`
	SourceType    string `json:"sourceType"`
	RegistryRef   string `json:"registryRef,omitempty"`
	// ImageRef is the tag `docker save` wrote the layers under: what the loaded
	// reference has to be retagged from before it is retagged again as the
	// row's own local tag.
	ImageRef string `json:"imageRef"`
}

// Spec is what CreateArchive writes into spec/: the manifest plus the two
// files a Dockerfile or compose image carries. Empty means "this image has
// none" — a registry image has neither.
type Spec struct {
	Manifest
	Dockerfile string
	Compose    string
}

// Inspection is what reading an archive's spec answers, without ever touching
// image.tar.gz's content: enough to fill the create form, and to say whether
// there is more to import.
type Inspection struct {
	Manifest
	Dockerfile string
	Compose    string
	HasImage   bool
	ImageSize  int64
}
