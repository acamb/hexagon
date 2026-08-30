package session

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/store"
)

// recordingDocker notes the container operations a lifecycle call made, so a
// test can say that a compose session went through the project instead.
type recordingDocker struct {
	dockerx.API
	containers []dockerx.ManagedContainer
	calls      []string
}

func (d *recordingDocker) ListManagedContainers(context.Context) ([]dockerx.ManagedContainer, error) {
	return d.containers, nil
}

func (d *recordingDocker) StartContainer(context.Context, string) error {
	d.calls = append(d.calls, "start")
	return nil
}

func (d *recordingDocker) StopContainer(context.Context, string, time.Duration) error {
	d.calls = append(d.calls, "stop")
	return nil
}

func (d *recordingDocker) RemoveContainer(context.Context, string, bool) error {
	d.calls = append(d.calls, "remove")
	return nil
}

func (d *recordingDocker) RunExec(context.Context, string, []string) (string, int, error) {
	d.calls = append(d.calls, "exec")
	return "", 0, nil
}

// A compose session is a project, so its lifecycle has to act on the project.
// Stopping only the agent would leave the database running; removing only the
// agent would leave it behind for good.
func TestComposeSessionLifecycleGoesThroughTheProject(t *testing.T) {
	ctx := context.Background()
	docker := &recordingDocker{}
	compose := &fakeCompose{}

	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	root := filepath.Join(t.TempDir(), "workspaces")
	manager := NewManager(st, docker, nil, nil, nil, compose, Config{WorkspaceRoot: root}, slog.New(slog.DiscardHandler))

	user, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	image, err := st.CreateImage(ctx, &store.Image{UserID: user.ID, Name: "advanced",
		SourceType: store.ImageSourceCompose, ImageRef: "ref", Status: store.ImageStatusReady})
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	session, err := st.CreateSession(ctx, &store.Session{
		UserID: user.ID, Title: "t", ImageID: image.ID, ImageRef: "ref",
		WorkspaceDir: filepath.Join(root, "s-1"), RepoDir: filepath.Join(root, "s-1", "repo"),
		ContainerID: "c-1", Status: store.SessionStatusRunning, Compose: true,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := manager.Start(ctx, session); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := manager.Stop(ctx, session); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := manager.Delete(ctx, session, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if want := []string{"start", "stop", "down"}; !slices.Equal(compose.calls, want) {
		t.Errorf("compose calls = %v, want %v", compose.calls, want)
	}
	// The bootstrap still runs in the agent container; nothing else touches it.
	if want := []string{"exec"}; !slices.Equal(docker.calls, want) {
		t.Errorf("docker calls = %v, want only the bootstrap exec %v", docker.calls, want)
	}
	// purge means "the work too", and what a service wrote is work.
	if !compose.downV {
		t.Error("a purging delete did not take the project's volumes with it")
	}
}

// The services of a compose project carry their session's id and the service
// role. Without the role they would each look like a container whose session
// row is gone, and every startup would warn about the databases.
func TestReconcileDoesNotReportAProjectsServicesAsOrphans(t *testing.T) {
	ctx := context.Background()
	docker := &recordingDocker{containers: []dockerx.ManagedContainer{
		{ID: "c-agent", SessionID: "s-1", Running: true},
		{ID: "c-db", SessionID: "s-1", Role: serviceRole, Running: true},
		{ID: "c-lost", SessionID: "s-deleted", Running: true},
	}}

	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	root := filepath.Join(t.TempDir(), "workspaces")

	var warned []string
	log := slog.New(slog.NewTextHandler(writerFunc(func(line string) { warned = append(warned, line) }), nil))
	manager := NewManager(st, docker, nil, nil, nil, &fakeCompose{}, Config{WorkspaceRoot: root}, log)

	user, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	image, err := st.CreateImage(ctx, &store.Image{UserID: user.ID, Name: "advanced",
		SourceType: store.ImageSourceCompose, ImageRef: "ref", Status: store.ImageStatusReady})
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if _, err := st.CreateSession(ctx, &store.Session{
		ID: "s-1", UserID: user.ID, Title: "t", ImageID: image.ID, ImageRef: "ref",
		WorkspaceDir: filepath.Join(root, "s-1"), RepoDir: filepath.Join(root, "s-1", "repo"),
		ContainerID: "c-agent", Status: store.SessionStatusRunning, Compose: true,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := manager.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, line := range warned {
		if strings.Contains(line, "c-db") {
			t.Errorf("a service of a live project was reported as an orphan:\n%s", line)
		}
	}
	// The one that really has no session left is still reported.
	found := false
	for _, line := range warned {
		if strings.Contains(line, "c-lost") {
			found = true
		}
	}
	if !found {
		t.Errorf("a container with no session left was not reported:\n%v", warned)
	}
}

// writerFunc turns a callback into an io.Writer, so a test can read the log
// lines a call produced without a file.
type writerFunc func(string)

func (w writerFunc) Write(p []byte) (int, error) {
	w(string(p))
	return len(p), nil
}
