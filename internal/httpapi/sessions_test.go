package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/store"
)

// readyImage creates an image the session can be started from, the way the
// images endpoint would.
func (e *testEnv) readyImage(name string) imageResponse {
	e.t.Helper()
	var img imageResponse
	e.decode(e.postJSON("/api/images", fmt.Sprintf(
		`{"name":%q,"sourceType":"dockerfile","dockerfile":"FROM busybox"}`, name)), &img)
	return e.waitForImageStatus(img.ID, store.ImageStatusReady)
}

// offerRepo makes a repository visible in the caller's GitHub listing.
func (e *testEnv) offerRepo(fullName, defaultBranch string) {
	e.repos.mu.Lock()
	defer e.repos.mu.Unlock()
	e.repos.repos = append(e.repos.repos, github.Repo{
		FullName:      fullName,
		CloneURL:      "https://github.com/" + fullName + ".git",
		DefaultBranch: defaultBranch,
	})
}

func (e *testEnv) waitForSessionStatus(id, want string) sessionResponse {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var session sessionResponse
	for time.Now().Before(deadline) {
		e.decode(e.do(http.MethodGet, "/api/sessions/"+id, nil), &session)
		if session.Status == want {
			return session
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("session %s stayed in status %q (%s), want %q", id, session.Status, session.Error, want)
	return session
}

func TestCreateSessionProvisionsAContainer(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	resp := env.postJSON("/api/sessions", fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	var created sessionResponse
	env.decode(resp, &created)
	if created.Title != "acme/widgets" {
		t.Errorf("title = %q, want the repository name by default", created.Title)
	}
	if created.Branch != "main" {
		t.Errorf("branch = %q, want the repository's default branch", created.Branch)
	}

	running := env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	if running.Error != "" {
		t.Errorf("a running session carries an error: %q", running.Error)
	}

	// The clone happens on the host, from the URL GitHub gave us.
	clones := env.cloner.clones()
	if len(clones) != 1 {
		t.Fatalf("made %d clones, want 1", len(clones))
	}
	clone := clones[0]
	if clone.CloneURL != "https://github.com/acme/widgets.git" || clone.Branch != "main" {
		t.Errorf("clone = %+v", clone)
	}
	if clone.Token != "gho_token" {
		t.Errorf("clone token = %q, want the caller's own", clone.Token)
	}
	if want := filepath.Join(env.workspaces, created.ID, "repo"); clone.Dest != want {
		t.Errorf("clone destination = %q, want %q", clone.Dest, want)
	}

	// A writable HOME has to exist before the container mounts it.
	if _, err := os.Stat(filepath.Join(env.workspaces, created.ID, "home", ".claude")); err != nil {
		t.Errorf("the agent home was not created: %v", err)
	}
}

// The container spec is the whole contract with a session, so it is worth
// asserting field by field.
func TestSessionContainerSpec(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q,"title":"my session"}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	specs := env.docker.containerSpecs()
	if len(specs) != 1 {
		t.Fatalf("created %d containers, want 1", len(specs))
	}
	var spec dockerx.ContainerSpec
	for _, s := range specs {
		spec = s
	}

	if spec.Image != image.ImageRef {
		t.Errorf("image = %q, want %q", spec.Image, image.ImageRef)
	}
	if strings.Join(spec.Cmd, " ") != "sleep infinity" {
		t.Errorf("cmd = %v", spec.Cmd)
	}
	if spec.User != "1000:1000" {
		t.Errorf("user = %q, want the host user", spec.User)
	}
	if spec.WorkingDir != dockerx.WorkspaceMount {
		t.Errorf("working directory = %q", spec.WorkingDir)
	}
	if spec.Labels[dockerx.LabelManaged] != "true" || spec.Labels[dockerx.LabelSessionID] != created.ID {
		t.Errorf("labels = %v, want the session to be findable again", spec.Labels)
	}
	if !spec.AutoRestart {
		t.Error("the container does not come back after a Docker restart")
	}

	workspace := filepath.Join(env.workspaces, created.ID)
	wantBinds := []string{
		filepath.Join(workspace, "repo") + ":" + dockerx.WorkspaceMount,
		filepath.Join(workspace, "home") + ":" + dockerx.AgentHome,
	}
	for _, want := range wantBinds {
		if !contains(spec.Binds, want) {
			t.Errorf("missing bind %q in %v", want, spec.Binds)
		}
	}

	env2 := map[string]string{}
	for _, entry := range spec.Env {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env2[name] = value
		}
	}
	if env2["HOME"] != dockerx.AgentHome {
		t.Errorf("HOME = %q", env2["HOME"])
	}
	if env2["GITHUB_TOKEN"] != "gho_token" {
		t.Errorf("GITHUB_TOKEN = %q, want the caller's token so git can push", env2["GITHUB_TOKEN"])
	}
	if env2["GIT_AUTHOR_NAME"] != "Hexagon User" || env2["GIT_COMMITTER_EMAIL"] != "user@example.test" {
		t.Errorf("git identity = %v", env2)
	}

	// tmux must be running before any terminal can attach.
	commands := env.docker.bootstrapCommands()
	if len(commands) != 1 || !strings.Contains(strings.Join(commands[0], " "), "tmux new-session -d -s main") {
		t.Errorf("bootstrap = %v", commands)
	}
	if !strings.Contains(strings.Join(commands[0], " "), "safe.directory") {
		t.Errorf("the bootstrap does not mark the clone safe for git: %v", commands)
	}
}

func TestCreateSessionRejectsARepositoryYouDoNotHave(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	resp := env.postJSON("/api/sessions", fmt.Sprintf(`{"repoFullName":"someone/else","imageId":%q}`, image.ID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if clones := env.cloner.clones(); len(clones) != 0 {
		t.Errorf("a refused request still cloned: %v", clones)
	}
}

func TestCreateSessionValidation(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.offerRepo("acme/widgets", "main")

	building := imageResponse{}
	env.decode(env.postJSON("/api/images", `{"name":"slow","sourceType":"dockerfile","dockerfile":"FROM busybox"}`), &building)

	cases := map[string]struct {
		body string
		want int
	}{
		"no repository": {`{"imageId":"whatever"}`, http.StatusBadRequest},
		"no image":      {`{"repoFullName":"acme/widgets"}`, http.StatusBadRequest},
		"unknown image": {`{"repoFullName":"acme/widgets","imageId":"nope"}`, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := env.postJSON("/api/sessions", tc.body).StatusCode; got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCreateSessionRequiresAReachableDaemon(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")
	env.docker.pingErr = fmt.Errorf("connection refused")

	resp := env.postJSON("/api/sessions", fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// A clone that fails must leave a session that says so, and no container.
func TestFailedProvisioningIsRecorded(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")
	env.cloner.err = fmt.Errorf("Authentication failed")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)

	failed := env.waitForSessionStatus(created.ID, store.SessionStatusFailed)
	if !strings.Contains(failed.Error, "Authentication failed") {
		t.Errorf("error = %q, want the reason", failed.Error)
	}
	if specs := env.docker.containerSpecs(); len(specs) != 0 {
		t.Errorf("a failed clone still created a container: %v", specs)
	}
}

func TestSessionStopAndStart(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	json := map[string]string{"Content-Type": "application/json"}

	var stopped sessionResponse
	env.decode(env.do(http.MethodPost, "/api/sessions/"+created.ID+"/stop", json), &stopped)
	if stopped.Status != store.SessionStatusStopped {
		t.Errorf("after stop the status is %q", stopped.Status)
	}

	var started sessionResponse
	env.decode(env.do(http.MethodPost, "/api/sessions/"+created.ID+"/start", json), &started)
	if started.Status != store.SessionStatusRunning {
		t.Errorf("after start the status is %q", started.Status)
	}
	// Starting re-runs the bootstrap: a restarted container has no tmux server.
	if commands := env.docker.bootstrapCommands(); len(commands) != 2 {
		t.Errorf("ran the bootstrap %d times, want one per start", len(commands))
	}
}

func TestDeleteSession(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")
	json := map[string]string{"Content-Type": "application/json"}

	newSession := func() sessionResponse {
		var created sessionResponse
		env.decode(env.postJSON("/api/sessions",
			fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
		return env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	}

	kept := newSession()
	if got := env.do(http.MethodDelete, "/api/sessions/"+kept.ID, json).StatusCode; got != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", got)
	}
	if _, err := os.Stat(filepath.Join(env.workspaces, kept.ID, "repo")); err != nil {
		t.Errorf("the workspace was removed without being asked: %v", err)
	}

	purged := newSession()
	if got := env.do(http.MethodDelete, "/api/sessions/"+purged.ID+"?purge=true", json).StatusCode; got != http.StatusNoContent {
		t.Fatalf("delete with purge = %d, want 204", got)
	}
	if _, err := os.Stat(filepath.Join(env.workspaces, purged.ID)); !os.IsNotExist(err) {
		t.Errorf("the workspace survived a purge: %v", err)
	}

	if len(env.docker.removedContainers) != 2 {
		t.Errorf("removed %d containers, want 2", len(env.docker.removedContainers))
	}
	var sessions []sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions", nil), &sessions)
	if len(sessions) != 0 {
		t.Errorf("%d sessions left after deleting both", len(sessions))
	}
}

// The database records what Hexagon last did; the daemon knows what is true.
func TestListSessionsReconcilesAgainstDocker(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertSession("running-then-gone", store.SessionStatusRunning, "container-a")
	env.insertSession("running-then-stopped", store.SessionStatusRunning, "container-b")

	// One container vanished, the other was stopped behind our back.
	env.docker.RemoveContainer(context.Background(), "container-a", true)
	env.docker.StopContainer(context.Background(), "container-b", 0)

	var sessions []sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions", nil), &sessions)

	byID := map[string]sessionResponse{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	if got := byID["running-then-gone"].Status; got != store.SessionStatusGone {
		t.Errorf("a session whose container disappeared is %q, want gone", got)
	}
	if got := byID["running-then-stopped"].Status; got != store.SessionStatusStopped {
		t.Errorf("a session whose container was stopped is %q, want stopped", got)
	}
}

func TestSessionEndpointsRequireASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	if got := env.do(http.MethodGet, "/api/sessions", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("GET /api/sessions = %d, want 401", got)
	}
	if got := env.postJSON("/api/sessions", `{"repoFullName":"a/b","imageId":"x"}`).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("POST /api/sessions = %d, want 401", got)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Claude Code keeps its "already set up" state in $HOME/.claude.json, not in
// the credentials file. Without it every session opens on first-run onboarding,
// which reads to the user as being asked to sign in again.
func TestSessionSeedsTheClaudeConfig(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	body, err := os.ReadFile(filepath.Join(env.workspaces, created.ID, "home", ".claude.json"))
	if err != nil {
		t.Fatalf("the session has no Claude Code config: %v", err)
	}

	var config struct {
		HasCompletedOnboarding bool `json:"hasCompletedOnboarding"`
		Projects               map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatalf("the config is not valid JSON: %v", err)
	}
	if !config.HasCompletedOnboarding {
		t.Error("onboarding is not marked complete, so the session opens on the first-run flow")
	}
	if !config.Projects[dockerx.WorkspaceMount].HasTrustDialogAccepted {
		t.Errorf("/workspace is not trusted, so the session opens on the folder prompt: %s", body)
	}
}
