package httpapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrea/hexagon/internal/store"
)

// A session without a repository still gets an empty RepoDir on the host —
// see TestSessionWithoutARepository — which is all the export endpoints need:
// they read from there, never from the container.
func (e *testEnv) createBareSession() sessionResponse {
	e.t.Helper()
	image := e.readyImage("base")
	var created sessionResponse
	e.decode(e.postJSON("/api/sessions", fmt.Sprintf(`{"imageId":%q}`, image.ID)), &created)
	return e.waitForSessionStatus(created.ID, store.SessionStatusRunning)
}

func TestWorkspaceEndpointsRequireASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	if got := env.do(http.MethodGet, "/api/sessions/s1/workspace/info", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("GET .../workspace/info = %d, want 401", got)
	}
	if got := env.do(http.MethodGet, "/api/sessions/s1/workspace", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("GET .../workspace = %d, want 401", got)
	}
}

func TestWorkspaceEndpointsAreScopedToTheCaller(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	other, err := env.store.UpsertUser(context.Background(), &store.User{GitHubLogin: "bob", GitHubID: 7})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	theirs, err := env.store.CreateSession(context.Background(), &store.Session{
		UserID: other.ID, Title: "theirs", ImageID: image.ID, ImageRef: image.ImageRef,
		WorkspaceDir: filepath.Join(env.workspaces, "theirs"),
		RepoDir:      filepath.Join(env.workspaces, "theirs", "repo"),
		Status:       store.SessionStatusStopped,
	})
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}

	if got := env.do(http.MethodGet, "/api/sessions/"+theirs.ID+"/workspace/info", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("GET another user's .../workspace/info = %d, want 404", got)
	}
	if got := env.do(http.MethodGet, "/api/sessions/"+theirs.ID+"/workspace", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("GET another user's .../workspace = %d, want 404", got)
	}
}

// The clone is in flight and the tree is half a repository: the milestone
// asks for the check to fail loudly rather than offer a download of it.
func TestWorkspaceInfoRefusesWhileCloning(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	session, err := env.store.CreateSession(context.Background(), &store.Session{
		UserID: env.userID(), Title: "cloning", ImageID: image.ID, ImageRef: image.ImageRef,
		WorkspaceDir: filepath.Join(env.workspaces, "cloning"),
		RepoDir:      filepath.Join(env.workspaces, "cloning", "repo"),
		Status:       store.SessionStatusCloning,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	resp := env.do(http.MethodGet, "/api/sessions/"+session.ID+"/workspace/info", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// The whole point read back: the probe reports what is really there, the
// download carries the file-download headers and a body that untars to
// exactly the fixture, and nothing of it is left behind on this server.
func TestExportWorkspaceDownloadsTheFixtureTree(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	created := env.createBareSession()

	repoDir := filepath.Join(env.workspaces, created.ID, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, "src"), 0o755); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	var info workspaceInfoResponse
	env.decode(env.do(http.MethodGet, "/api/sessions/"+created.ID+"/workspace/info", nil), &info)
	if !info.Exists {
		t.Fatalf("info.Exists = false, want true")
	}
	if info.Files != 2 {
		t.Errorf("info.Files = %d, want 2", info.Files)
	}
	if want := int64(len("hello") + len("package main")); info.Bytes != want {
		t.Errorf("info.Bytes = %d, want %d", info.Bytes, want)
	}

	before, err := os.ReadDir(env.cfg.TransfersDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read transfers dir: %v", err)
	}

	resp := env.do(http.MethodGet, "/api/sessions/"+created.ID+"/workspace", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="workspace.tar.gz"` {
		t.Errorf("Content-Disposition = %q", got)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if h.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %s: %v", h.Name, err)
			}
			files[h.Name] = string(body)
		}
	}
	if files["README.md"] != "hello" {
		t.Errorf("README.md = %q, want %q", files["README.md"], "hello")
	}
	if files["src/main.go"] != "package main" {
		t.Errorf("src/main.go = %q, want %q", files["src/main.go"], "package main")
	}

	after, err := os.ReadDir(env.cfg.TransfersDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read transfers dir: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("transfers dir changed from %d entries to %d: the export left something behind", len(before), len(after))
	}
}

// "Segnalato all'utente subito prima di tentare il backup": a session with no
// workspace directory answers 404 as JSON, before any archive byte is
// written.
func TestExportWorkspaceRefusesAMissingDirectory(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	created := env.createBareSession()

	if err := os.RemoveAll(filepath.Join(env.workspaces, created.ID, "repo")); err != nil {
		t.Fatalf("remove the workspace: %v", err)
	}

	var info workspaceInfoResponse
	env.decode(env.do(http.MethodGet, "/api/sessions/"+created.ID+"/workspace/info", nil), &info)
	if info.Exists {
		t.Fatalf("info.Exists = true for a removed directory")
	}

	resp := env.do(http.MethodGet, "/api/sessions/"+created.ID+"/workspace", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json: the refusal must be JSON, not the start of an archive", got)
	}
}
