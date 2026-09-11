package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/andrea/hexagon/internal/store"
)

// The whole point of reading from the host rather than from the container:
// nested directories, an empty one, a symlink and a file nobody but its owner
// can read all round-trip through the archive, while a fifo — nothing an
// extracted workspace needs — is left out without failing the rest of it.
func TestWriteWorkspaceArchiveRoundTrips(t *testing.T) {
	var logs bytes.Buffer
	manager, _, root := testManager(t, nil)
	manager.log = slog.New(slog.NewTextHandler(&logs, nil))

	dir := filepath.Join(root, "s1", "repo")
	if err := os.MkdirAll(filepath.Join(dir, "nested", "empty"), 0o755); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello workspace"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "secret.txt"), []byte("nope"), 0o000); err != nil {
		t.Fatalf("write unreadable file: %v", err)
	}
	if err := os.Symlink("../hello.txt", filepath.Join(dir, "nested", "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "afifo"), 0o600); err != nil {
		t.Skipf("mkfifo not supported on this system: %v", err)
	}

	session := &store.Session{ID: "s1", RepoDir: dir}

	var archive bytes.Buffer
	if err := manager.WriteWorkspaceArchive(context.Background(), session, &archive); err != nil {
		t.Fatalf("WriteWorkspaceArchive: %v", err)
	}

	gz, err := gzip.NewReader(&archive)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	found := map[string]*tar.Header{}
	contents := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		found[h.Name] = h
		if h.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %s: %v", h.Name, err)
			}
			contents[h.Name] = string(body)
		}
	}

	if body := contents["hello.txt"]; body != "hello workspace" {
		t.Errorf("hello.txt content = %q, want %q", body, "hello workspace")
	}
	if h, ok := found["nested/"]; !ok || h.Typeflag != tar.TypeDir {
		t.Errorf("nested/ not archived as a directory: %+v", h)
	}
	if h, ok := found["nested/empty/"]; !ok || h.Typeflag != tar.TypeDir {
		t.Errorf("the empty directory was not archived: %+v", h)
	}

	link, ok := found["nested/link.txt"]
	if !ok {
		t.Fatalf("the symlink was not archived")
	}
	if link.Typeflag != tar.TypeSymlink {
		t.Errorf("nested/link.txt typeflag = %v, want TypeSymlink", link.Typeflag)
	}
	if link.Linkname != "../hello.txt" {
		t.Errorf("symlink target = %q, want %q", link.Linkname, "../hello.txt")
	}

	if _, ok := found["afifo"]; ok {
		t.Errorf("the fifo was archived, want it skipped")
	}
	if _, ok := found["nested/secret.txt"]; ok {
		t.Errorf("the unreadable file was archived, want it skipped")
	}
	if !strings.Contains(logs.String(), "skipped=2") {
		t.Errorf("log output = %q, want it to report 2 skipped entries", logs.String())
	}
}

// checkedRepoDir is the guard removeWorkspace already has, applied here to a
// read instead of a delete: a corrupted or hand-edited row must fail rather
// than tar the rest of the disk.
func TestWorkspaceExportRefusesPathsOutsideTheRoot(t *testing.T) {
	manager, _, root := testManager(t, nil)

	outside := t.TempDir()
	refused := []string{
		outside,                          // somewhere else entirely
		root,                             // the root itself, not one session
		filepath.Join(root, "..", "etc"), // an escape through the root
	}
	for _, dir := range refused {
		session := &store.Session{ID: "s1", RepoDir: dir}

		if _, err := manager.WorkspaceInfo(session); err == nil {
			t.Errorf("WorkspaceInfo(%q) was allowed", dir)
		} else if !strings.Contains(err.Error(), root) {
			t.Errorf("WorkspaceInfo(%q) error = %q, want it to name the root", dir, err)
		}

		var buf bytes.Buffer
		if err := manager.WriteWorkspaceArchive(context.Background(), session, &buf); err == nil {
			t.Errorf("WriteWorkspaceArchive(%q) was allowed", dir)
		}
		if buf.Len() != 0 {
			t.Errorf("WriteWorkspaceArchive(%q) wrote bytes before refusing", dir)
		}
	}
}

func TestWriteWorkspaceArchiveHonoursACancelledContext(t *testing.T) {
	manager, _, root := testManager(t, nil)
	dir := filepath.Join(root, "s1", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	err := manager.WriteWorkspaceArchive(ctx, &store.Session{ID: "s1", RepoDir: dir}, &buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestWorkspaceInfo(t *testing.T) {
	manager, _, root := testManager(t, nil)
	session := &store.Session{ID: "s1", RepoDir: filepath.Join(root, "s1", "repo")}

	info, err := manager.WorkspaceInfo(session)
	if err != nil {
		t.Fatalf("WorkspaceInfo on a missing directory: %v", err)
	}
	if info.Exists {
		t.Errorf("Exists = true for a missing directory")
	}

	if err := os.MkdirAll(filepath.Join(session.RepoDir, "sub"), 0o755); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(session.RepoDir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(session.RepoDir, "sub", "b.txt"), []byte("hi!"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	info, err = manager.WorkspaceInfo(session)
	if err != nil {
		t.Fatalf("WorkspaceInfo: %v", err)
	}
	if !info.Exists {
		t.Fatalf("Exists = false for a directory that is there")
	}
	if info.Files != 2 {
		t.Errorf("Files = %d, want 2", info.Files)
	}
	if want := int64(len("hello") + len("hi!")); info.Bytes != want {
		t.Errorf("Bytes = %d, want %d", info.Bytes, want)
	}
}
