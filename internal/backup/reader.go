package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Inspect reads an archive's spec back out of r, without ever reading
// image.tar.gz's content into memory: only its size, from the tar header, is
// reported. It is the whole of the spec-only restore path — the text it
// returns is what fills the create form.
func Inspect(r io.Reader) (Inspection, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Inspection{}, fmt.Errorf("read archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var (
		insp         Inspection
		sawManifest  bool
		sawAnyMember bool
	)
	for i := 0; ; i++ {
		if i >= maxMembers {
			return Inspection{}, fmt.Errorf("backup: archive has more than %d entries", maxMembers)
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Inspection{}, fmt.Errorf("read archive: %w", err)
		}
		if err := checkMember(hdr); err != nil {
			return Inspection{}, err
		}

		switch hdr.Name {
		case memberManifest:
			sawAnyMember = true
			data, err := readCapped(tr, MaxSpecMember)
			if err != nil {
				return Inspection{}, fmt.Errorf("read manifest: %w", err)
			}
			if err := json.Unmarshal(data, &insp.Manifest); err != nil {
				return Inspection{}, fmt.Errorf("read manifest: %w", err)
			}
			sawManifest = true
		case memberDockerfile:
			sawAnyMember = true
			data, err := readCapped(tr, MaxSpecMember)
			if err != nil {
				return Inspection{}, fmt.Errorf("read %s: %w", memberDockerfile, err)
			}
			insp.Dockerfile = string(data)
		case memberCompose:
			sawAnyMember = true
			data, err := readCapped(tr, MaxSpecMember)
			if err != nil {
				return Inspection{}, fmt.Errorf("read %s: %w", memberCompose, err)
			}
			insp.Compose = string(data)
		case memberImage:
			sawAnyMember = true
			insp.HasImage = true
			insp.ImageSize = hdr.Size
		}
	}

	if !sawAnyMember {
		return Inspection{}, fmt.Errorf(
			"backup: archive has none of the expected members (%s, %s, %s, %s)",
			memberManifest, memberDockerfile, memberCompose, memberImage)
	}
	if !sawManifest {
		return Inspection{}, fmt.Errorf("backup: archive has no %s", memberManifest)
	}
	if insp.Manifest.FormatVersion > FormatVersion {
		return Inspection{}, fmt.Errorf("%w: this backup is format %d, this server understands up to %d",
			ErrFormatVersion, insp.Manifest.FormatVersion, FormatVersion)
	}
	return insp, nil
}

// imageReader wraps the file and the gzip reader OpenImage opened, so a
// caller reading the member through it closes both with one Close.
type imageReader struct {
	tr *tar.Reader
	gz *gzip.Reader
	f  *os.File
}

func (r *imageReader) Read(p []byte) (int, error) { return r.tr.Read(p) }

func (r *imageReader) Close() error {
	gzErr := r.gz.Close()
	fErr := r.f.Close()
	if gzErr != nil {
		return gzErr
	}
	return fErr
}

// OpenImage opens the archive at path and returns a reader positioned at its
// image.tar.gz member, still gzip-compressed: docker's load accepts a
// compressed stream directly, and this package never decompresses a member it
// is not reading itself. ErrNoImage reports an archive with no image to
// import.
//
// It takes a path rather than an io.Reader because it has to be read fully
// once to find the member, close over that read, and hand back a fresh stream
// for the caller to read from — a plain reader has nothing left to rewind.
func OpenImage(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read archive: %w", err)
	}
	tr := tar.NewReader(gz)

	for i := 0; ; i++ {
		if i >= maxMembers {
			gz.Close()
			f.Close()
			return nil, fmt.Errorf("backup: archive has more than %d entries", maxMembers)
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			gz.Close()
			f.Close()
			return nil, ErrNoImage
		}
		if err != nil {
			gz.Close()
			f.Close()
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if err := checkMember(hdr); err != nil {
			gz.Close()
			f.Close()
			return nil, err
		}
		if hdr.Name == memberImage {
			return &imageReader{tr: tr, gz: gz, f: f}, nil
		}
	}
}

// checkMember refuses every classic tar extraction hazard outright: there is
// no case this format needs an absolute path, a path that escapes the
// archive, a symlink, a hard link, or anything that is not a regular file, so
// none of them get a pass just for carrying a name this package does not
// recognize.
func checkMember(hdr *tar.Header) error {
	if strings.HasPrefix(hdr.Name, "/") {
		return fmt.Errorf("backup: refusing member %q: absolute path", hdr.Name)
	}
	if strings.Contains(hdr.Name, "..") {
		return fmt.Errorf("backup: refusing member %q: path escapes the archive", hdr.Name)
	}
	switch hdr.Typeflag {
	case tar.TypeReg:
		return nil
	default:
		return fmt.Errorf("backup: refusing member %q: not a regular file", hdr.Name)
	}
}

// readCapped reads at most limit bytes from r, refusing anything larger as it
// is read rather than after: LimitReader never pulls more than limit+1 bytes
// through the decompressor behind r, so a member whose declared size lies is
// caught before it can expand further.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("backup: member exceeds %d bytes", limit)
	}
	return data, nil
}
