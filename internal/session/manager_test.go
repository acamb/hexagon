package session

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/store"
)

// stubDocker implements only what a test needs; anything else panics, which is
// the right outcome for a call the test did not expect.
type stubDocker struct {
	dockerx.API
	containers []dockerx.ManagedContainer
}

func (s stubDocker) ListManagedContainers(context.Context) ([]dockerx.ManagedContainer, error) {
	return s.containers, nil
}

func testManager(t *testing.T, docker dockerx.API) (*Manager, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	root := filepath.Join(t.TempDir(), "workspaces")
	manager := NewManager(st, docker, nil, nil, nil, Config{WorkspaceRoot: root}, slog.New(slog.DiscardHandler))
	return manager, st, root
}

// removeWorkspace is a recursive delete driven by a path from the database.
// If that path is ever wrong, it has to refuse rather than take the disk with it.
func TestRemoveWorkspaceRefusesPathsOutsideTheRoot(t *testing.T) {
	manager, _, root := testManager(t, nil)

	outside := t.TempDir()
	canary := filepath.Join(outside, "important.txt")
	if err := os.WriteFile(canary, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("write canary: %v", err)
	}

	refused := []string{
		outside,                          // somewhere else entirely
		root,                             // the root itself, not one session
		filepath.Join(root, "..", "etc"), // an escape through the root
		"/",
	}
	for _, dir := range refused {
		if err := manager.removeWorkspace(dir); err == nil {
			t.Errorf("removeWorkspace(%q) was allowed", dir)
		}
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("the canary was deleted: %v", err)
	}

	// A real session workspace is removed.
	workspace := filepath.Join(root, "session-1")
	if err := os.MkdirAll(filepath.Join(workspace, "repo"), 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := manager.removeWorkspace(workspace); err != nil {
		t.Fatalf("removeWorkspace on a real workspace: %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("the workspace survived: %v", err)
	}

	// An empty path is a session that never got that far, not an error.
	if err := manager.removeWorkspace(""); err != nil {
		t.Errorf("removeWorkspace(\"\") = %v, want no error", err)
	}
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	docker := stubDocker{containers: []dockerx.ManagedContainer{
		{ID: "c-running", SessionID: "s-running", Running: true},
		{ID: "c-stopped", SessionID: "s-stopped", Running: false},
		{ID: "c-orphan", SessionID: "s-deleted", Running: true},
	}}
	manager, st, root := testManager(t, docker)

	user, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	image, err := st.CreateImage(ctx, &store.Image{UserID: user.ID, Name: "base",
		SourceType: store.ImageSourceDockerfile, ImageRef: "ref", Status: store.ImageStatusReady})
	if err != nil {
		t.Fatalf("create image: %v", err)
	}

	newSession := func(id, containerID, status string) {
		t.Helper()
		_, err := st.CreateSession(ctx, &store.Session{
			ID: id, UserID: user.ID, Title: id, Provider: "github", RepoFullName: "acme/widgets",
			RepoCloneURL: "https://example.test/x.git", ImageID: image.ID, ImageRef: "ref",
			WorkspaceDir: filepath.Join(root, id), RepoDir: filepath.Join(root, id, "repo"),
			ContainerID: containerID, Status: status,
		})
		if err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
	}

	// Recorded as stopped, actually running.
	newSession("s-running", "c-running", store.SessionStatusStopped)
	// Recorded as running, actually stopped.
	newSession("s-stopped", "c-stopped", store.SessionStatusRunning)
	// Its container is gone.
	newSession("s-vanished", "c-vanished", store.SessionStatusRunning)
	// Still being set up: the manager owns this one, not the daemon.
	newSession("s-cloning", "", store.SessionStatusCloning)

	if err := manager.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	want := map[string]string{
		"s-running":  store.SessionStatusRunning,
		"s-stopped":  store.SessionStatusStopped,
		"s-vanished": store.SessionStatusGone,
		"s-cloning":  store.SessionStatusCloning,
	}
	for id, status := range want {
		session, err := st.SessionByID(ctx, user.ID, id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if session.Status != status {
			t.Errorf("%s is %q, want %q", id, session.Status, status)
		}
	}

	if session, _ := st.SessionByID(ctx, user.ID, "s-vanished"); !strings.Contains(session.Error, "no longer exists") {
		t.Errorf("a vanished session does not say why: %q", session.Error)
	}
}

func TestIsSettled(t *testing.T) {
	settled := []string{store.SessionStatusRunning, store.SessionStatusStopped, store.SessionStatusGone}
	for _, status := range settled {
		if !isSettled(status) {
			t.Errorf("isSettled(%q) = false, want true", status)
		}
	}
	// While a session is being provisioned the manager owns its status, and
	// Docker must not overrule it: the container can exist before it is usable.
	inFlight := []string{store.SessionStatusCreating, store.SessionStatusCloning,
		store.SessionStatusStarting, store.SessionStatusFailed}
	for _, status := range inFlight {
		if isSettled(status) {
			t.Errorf("isSettled(%q) = true, want false", status)
		}
	}
}

// The bootstrap is the only place that teaches git inside the container how to
// authenticate, and it has to work with images Hexagon did not build.
func TestBootstrapScriptInstallsTheCredentialHelper(t *testing.T) {
	script := bootstrapScript(false, true, false)

	if !strings.Contains(script, "credential.helper") {
		t.Errorf("no credential helper in the bootstrap:\n%s", script)
	}
	// The helper names the variables; it must never carry their values, which
	// would put the secret in a file inside the workspace.
	for _, want := range []string{"$HEXAGON_GIT_USERNAME", "$HEXAGON_GIT_PASSWORD"} {
		if !strings.Contains(script, want) {
			t.Errorf("the helper does not read %s:\n%s", want, script)
		}
	}

	// A session with no repository has nothing to authenticate to.
	if script := bootstrapScript(false, false, false); strings.Contains(script, "credential.helper") {
		t.Errorf("a session with no credentials got a helper anyway:\n%s", script)
	}
}

// The launch is only appended when the session asked for it, and it names the
// release's fixed mount point and port.
func TestBootstrapScriptStartsCodeServerWhenAsked(t *testing.T) {
	script := bootstrapScript(false, false, true)
	if !strings.Contains(script, dockerx.VSCodeMount+"/bin/code-server") {
		t.Errorf("code-server is not started from its mount:\n%s", script)
	}
	if !strings.Contains(script, "--bind-addr 0.0.0.0:8443") {
		t.Errorf("code-server does not bind the published port on every interface:\n%s", script)
	}

	if script := bootstrapScript(false, false, false); strings.Contains(script, "code-server") {
		t.Errorf("a session without the integration got code-server anyway:\n%s", script)
	}
}

// A server with no VSCodeSource at all must refuse the request rather than
// provision a container that will never publish an editor.
func TestCreateRejectsVSCodeWithNoSource(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := testManager(t, stubDocker{})

	user, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	image, err := st.CreateImage(ctx, &store.Image{UserID: user.ID, Name: "base",
		SourceType: store.ImageSourceDockerfile, ImageRef: "ref", Status: store.ImageStatusReady})
	if err != nil {
		t.Fatalf("create image: %v", err)
	}

	_, err = manager.Create(ctx, user, CreateRequest{ImageID: image.ID, VSCode: true})
	if !errors.Is(err, ErrVSCodeUnavailable) {
		t.Errorf("error = %v, want ErrVSCodeUnavailable", err)
	}
}
