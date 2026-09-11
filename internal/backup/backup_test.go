package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testSpec() Spec {
	return Spec{
		Manifest: Manifest{
			FormatVersion: FormatVersion,
			Name:          "base",
			SourceType:    "dockerfile",
			ImageRef:      "hexagon/img-abc123:latest",
		},
		Dockerfile: "FROM busybox\n",
	}
}

func TestSpecOnlyArchiveRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteArchive(&buf, testSpec(), nil, 0); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	insp, err := Inspect(&buf)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if insp.Name != "base" || insp.SourceType != "dockerfile" || insp.ImageRef != "hexagon/img-abc123:latest" {
		t.Errorf("manifest = %+v", insp.Manifest)
	}
	if insp.Dockerfile != "FROM busybox\n" {
		t.Errorf("dockerfile = %q", insp.Dockerfile)
	}
	if insp.HasImage {
		t.Error("hasImage = true for an archive with no image")
	}
}

// An archive with an image reports its size from the tar header alone: HasImage
// and ImageSize come from hdr.Size, and Inspect never calls the member's own
// readCapped path the way it does for the spec files, so a many-gigabyte image
// costs Inspect nothing beyond skipping past its bytes.
func TestArchiveWithImageReportsSizeWithoutReadingIt(t *testing.T) {
	spec := testSpec()
	content := bytes.Repeat([]byte("x"), 4096)

	var buf bytes.Buffer
	if err := WriteArchive(&buf, spec, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}

	insp, err := Inspect(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !insp.HasImage || insp.ImageSize != int64(len(content)) {
		t.Errorf("hasImage/imageSize = %v/%d, want true/%d", insp.HasImage, insp.ImageSize, len(content))
	}
}

func TestOpenImageStreamsTheMemberWithoutBuffering(t *testing.T) {
	spec := testSpec()
	content := []byte("fake docker save stream")

	dir := t.TempDir()
	path := filepath.Join(dir, "backup.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := WriteArchive(f, spec, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r, err := OpenImage(path)
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("image content = %q, want %q", got, content)
	}
}

func TestOpenImageWithNoImageReportsErrNoImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := WriteArchive(f, testSpec(), nil, 0); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	f.Close()

	if _, err := OpenImage(path); err != ErrNoImage {
		t.Errorf("OpenImage: err = %v, want ErrNoImage", err)
	}
}

// buildRawArchive writes a gzip-compressed tar directly, bypassing
// WriteArchive, so a test can hand Inspect an archive no legitimate caller
// would ever produce.
func buildRawArchive(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o600,
			Size:     int64(len(e.body)),
			Typeflag: e.typeflag,
			Linkname: e.linkname,
		}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
}

func manifestEntry(t *testing.T, m Manifest) tarEntry {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return tarEntry{name: memberManifest, body: data}
}

func TestReadRefusesHostileMembers(t *testing.T) {
	validManifest := manifestEntry(t, Manifest{FormatVersion: FormatVersion, Name: "x", SourceType: "dockerfile"})

	cases := map[string]struct {
		entries []tarEntry
		wantErr string
	}{
		"path traversal": {
			entries: []tarEntry{validManifest, {name: "spec/../../../etc/passwd", body: []byte("x")}},
			wantErr: "escapes the archive",
		},
		"absolute path": {
			entries: []tarEntry{validManifest, {name: "/etc/passwd", body: []byte("x")}},
			wantErr: "absolute path",
		},
		"symlink": {
			entries: []tarEntry{validManifest, {name: "spec/evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
			wantErr: "not a regular file",
		},
		"hard link": {
			entries: []tarEntry{validManifest, {name: "spec/evil", typeflag: tar.TypeLink, linkname: memberManifest}},
			wantErr: "not a regular file",
		},
		"only unknown members": {
			entries: []tarEntry{{name: "spec/mystery", body: []byte("x")}},
			wantErr: "none of the expected members",
		},
		"no manifest": {
			entries: []tarEntry{{name: memberDockerfile, body: []byte("FROM x")}},
			wantErr: "no spec/image.json",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := buildRawArchive(t, tc.entries)
			_, err := Inspect(bytes.NewReader(raw))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Inspect: err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// An unknown member alongside recognized ones is silently ignored, not
// refused: only an archive of nothing but unknown members is an error.
func TestUnknownMemberAlongsideKnownOnesIsIgnored(t *testing.T) {
	raw := buildRawArchive(t, []tarEntry{
		manifestEntry(t, Manifest{FormatVersion: FormatVersion, Name: "x", SourceType: "dockerfile"}),
		{name: "spec/mystery", body: []byte("whatever")},
	})
	insp, err := Inspect(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if insp.Name != "x" {
		t.Errorf("name = %q, want the manifest read despite the unknown member", insp.Name)
	}
}

// A spec member over the limit is refused as it is read, not after: the
// declared size lies, and the reader still stops at MaxSpecMember+1 bytes
// pulled through decompression.
func TestOversizedSpecMemberIsRefused(t *testing.T) {
	raw := buildRawArchive(t, []tarEntry{
		manifestEntry(t, Manifest{FormatVersion: FormatVersion, Name: "x", SourceType: "dockerfile"}),
		{name: memberDockerfile, body: bytes.Repeat([]byte("a"), int(MaxSpecMember)+1)},
	})
	_, err := Inspect(bytes.NewReader(raw))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("Inspect: err = %v, want a refusal naming the limit", err)
	}
}

// A manifest whose format version is newer than this build understands is
// refused with a sentence, rather than a confusing failure partway through an
// import.
func TestFutureFormatVersionIsRefused(t *testing.T) {
	raw := buildRawArchive(t, []tarEntry{
		manifestEntry(t, Manifest{FormatVersion: FormatVersion + 1, Name: "x", SourceType: "dockerfile"}),
	})
	_, err := Inspect(bytes.NewReader(raw))
	if err == nil || !strings.Contains(err.Error(), "format") {
		t.Errorf("Inspect: err = %v, want a refusal naming the format version", err)
	}
}

func TestArchiveWithNoManifestIsRefused(t *testing.T) {
	raw := buildRawArchive(t, []tarEntry{{name: memberDockerfile, body: []byte("FROM x")}})
	_, err := Inspect(bytes.NewReader(raw))
	if err == nil || !strings.Contains(err.Error(), "no "+memberManifest) {
		t.Errorf("Inspect: err = %v, want a refusal naming the missing manifest", err)
	}
}
