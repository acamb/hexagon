package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// WriteArchive writes the backup format to w: spec/image.json always, the two
// source files when spec carries them, and image.tar.gz when image is not
// nil, read from it up to imageSize bytes.
//
// The outer gzip runs at BestSpeed. Almost all of an archive's bytes are
// image.tar.gz, which is already compressed; spending the CPU to compress it a
// second time gains nothing and would double the time on the one part that is
// slow. The spec files still compress, which is all the outer gzip is there
// for.
//
// imageSize is required rather than discovered by copying to a buffer first:
// docker save writes gigabytes, and this is the one call in the package that
// would otherwise be tempted to hold that much in memory.
func WriteArchive(w io.Writer, spec Spec, image io.Reader, imageSize int64) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("start archive: %w", err)
	}
	tw := tar.NewWriter(gz)

	manifestJSON, err := json.MarshalIndent(spec.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := writeMember(tw, memberManifest, manifestJSON); err != nil {
		return err
	}
	if spec.Dockerfile != "" {
		if err := writeMember(tw, memberDockerfile, []byte(spec.Dockerfile)); err != nil {
			return err
		}
	}
	if spec.Compose != "" {
		if err := writeMember(tw, memberCompose, []byte(spec.Compose)); err != nil {
			return err
		}
	}
	if image != nil {
		if err := tw.WriteHeader(&tar.Header{
			Name:    memberImage,
			Mode:    0o600,
			Size:    imageSize,
			ModTime: time.Now(),
		}); err != nil {
			return fmt.Errorf("write image header: %w", err)
		}
		if _, err := io.Copy(tw, image); err != nil {
			return fmt.Errorf("write image: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	return gz.Close()
}

func writeMember(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    0o600,
		Size:    int64(len(body)),
		ModTime: time.Now(),
	}); err != nil {
		return fmt.Errorf("write %s header: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}
