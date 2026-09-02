package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
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
	e.ghRepos.offer(fullName, defaultBranch)
}

// containerOf finds the container the fake daemon was asked to create for a
// session, by the label every session container carries.
func (e *testEnv) containerOf(sessionID string) (string, dockerx.ContainerSpec) {
	e.t.Helper()
	for id, spec := range e.docker.containerSpecs() {
		if spec.Labels[dockerx.LabelSessionID] == sessionID {
			return id, spec
		}
	}
	e.t.Fatalf("no container was created for session %s", sessionID)
	return "", dockerx.ContainerSpec{}
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

	// The clone happens on the host, from the URL the provider gave us.
	clones := env.cloner.clones()
	if len(clones) != 1 {
		t.Fatalf("made %d clones, want 1", len(clones))
	}
	clone := clones[0]
	if clone.CloneURL != "https://github.test/acme/widgets.git" || clone.Branch != "main" {
		t.Errorf("clone = %+v", clone)
	}
	if clone.Token != "gho_token" {
		t.Errorf("clone token = %q, want the caller's own", clone.Token)
	}
	// The username half comes from the provider, not from the account name:
	// GitHub and Bitbucket want different placeholders next to their tokens.
	if clone.Username != "x-github-token" {
		t.Errorf("clone username = %q, want the provider's", clone.Username)
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

	env2 := env.containerEnv()
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
		"no image":       {`{"repoFullName":"acme/widgets"}`, http.StatusBadRequest},
		"unknown image":  {`{"repoFullName":"acme/widgets","imageId":"nope"}`, http.StatusBadRequest},
		"nothing at all": {`{}`, http.StatusBadRequest},
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

// bootstrapScript returns the shell script of the nth bootstrap, which is where
// the decision to start Claude Code ends up.
func (e *testEnv) bootstrapScript(n int) string {
	e.t.Helper()
	commands := e.docker.bootstrapCommands()
	if len(commands) <= n {
		e.t.Fatalf("ran %d bootstraps, want more than %d", len(commands), n)
	}
	return strings.Join(commands[n], " ")
}

// containerEnv is the environment the session's container was created with,
// which is where the provider credentials either are or deliberately are not.
func (e *testEnv) containerEnv() map[string]string {
	e.t.Helper()
	env := map[string]string{}
	for _, spec := range e.docker.containerSpecs() {
		for _, entry := range spec.Env {
			if name, value, ok := strings.Cut(entry, "="); ok {
				env[name] = value
			}
		}
	}
	return env
}

func TestSessionStartsClaudeCodeByDefault(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
	if !created.AutoClaude {
		t.Error("autoClaude = false, want a session that starts Claude Code when nothing said otherwise")
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	script := env.bootstrapScript(0)
	if !strings.Contains(script, "claude") {
		t.Errorf("the tmux session was created without Claude Code: %s", script)
	}
	// Without the shell after it, a claude that exits takes the tmux session,
	// and the terminal, with it.
	if !strings.Contains(script, "exec \"${SHELL:-sh}\"") {
		t.Errorf("the tmux command has no shell to fall back to: %s", script)
	}
}

func TestSessionCanBeCreatedWithoutClaudeCode(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q,"autoClaude":false}`, image.ID)), &created)
	if created.AutoClaude {
		t.Error("autoClaude = true, want the value the request asked for")
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	if script := env.bootstrapScript(0); strings.Contains(script, "claude") {
		t.Errorf("the tmux session starts Claude Code anyway: %s", script)
	}
}

// The default has to keep working exactly as it did: a session whose request
// says nothing about the token is a session that can push.
func TestSessionCarriesTheProviderTokenByDefault(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
	if !created.PropagateToken {
		t.Error("propagateToken = false, want the token passed when nothing said otherwise")
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	containerEnv := env.containerEnv()
	for _, name := range []string{"HEXAGON_GIT_USERNAME", "HEXAGON_GIT_PASSWORD", "GITHUB_TOKEN"} {
		if containerEnv[name] == "" {
			t.Errorf("%s is missing from the container: %v", name, containerEnv)
		}
	}
	if script := env.bootstrapScript(0); !strings.Contains(script, "credential.helper") {
		t.Errorf("the bootstrap does not install a credential helper: %s", script)
	}
}

// The switch itself: the clone still happens with the credentials, because it
// runs on the host and a private repository needs them, and the container gets
// none of them.
func TestSessionCanBeCreatedWithoutTheProviderToken(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q,"propagateToken":false}`, image.ID)), &created)
	if created.PropagateToken {
		t.Error("propagateToken = true, want the value the request asked for")
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	clones := env.cloner.clones()
	if len(clones) != 1 || clones[0].Token != "gho_token" {
		t.Errorf("clones = %+v, want one made with the caller's own token", clones)
	}

	containerEnv := env.containerEnv()
	for _, name := range []string{"HEXAGON_GIT_USERNAME", "HEXAGON_GIT_PASSWORD", "GITHUB_TOKEN"} {
		if _, ok := containerEnv[name]; ok {
			t.Errorf("%s reached a container that asked not to have it: %v", name, containerEnv)
		}
	}
	// A helper with nothing to read would answer with an empty username and
	// password, which turns "no credentials" into a confusing rejection.
	if script := env.bootstrapScript(0); strings.Contains(script, "credential.helper") {
		t.Errorf("the bootstrap installs a credential helper anyway: %s", script)
	}

	// And the choice survives a round trip, because the session page reports it.
	var reloaded sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions/"+created.ID, nil), &reloaded)
	if reloaded.PropagateToken {
		t.Error("the session reports itself as carrying the token")
	}
}

// vscode: true is what makes the difference: a published port, the release
// bind mounted read-only, and a bootstrap that starts code-server.
func TestSessionCanBeCreatedWithVSCode(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q,"vscode":true}`, image.ID)), &created)
	if !created.VSCode {
		t.Error("vscode = false, want the value the request asked for")
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	specs := env.docker.containerSpecs()
	if len(specs) != 1 {
		t.Fatalf("created %d containers, want 1", len(specs))
	}
	var spec dockerx.ContainerSpec
	for _, s := range specs {
		spec = s
	}
	if len(spec.Ports) != 1 || spec.Ports[0].Container != dockerx.VSCodePort {
		t.Errorf("ports = %v, want [%d]", spec.Ports, dockerx.VSCodePort)
	}
	if !contains(spec.Binds, "/vscode-release:"+dockerx.VSCodeMount+":ro") {
		t.Errorf("missing the read-only code-server bind in %v", spec.Binds)
	}
	if env.vscode.ensured != 1 {
		t.Errorf("code-server was prepared %d times, want 1", env.vscode.ensured)
	}

	if script := env.bootstrapScript(0); !strings.Contains(script, "code-server") {
		t.Errorf("the bootstrap does not start code-server: %s", script)
	}

	// And the choice survives a round trip, because the session page reports it.
	var reloaded sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions/"+created.ID, nil), &reloaded)
	if !reloaded.VSCode {
		t.Error("the session does not report itself as having the integration")
	}
}

// The default has to keep looking exactly as it did before this milestone: no
// port, no mount, no code-server in the bootstrap.
func TestSessionDefaultsToNoVSCode(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
	if created.VSCode {
		t.Error("vscode = true, want off by default")
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	specs := env.docker.containerSpecs()
	var spec dockerx.ContainerSpec
	for _, s := range specs {
		spec = s
	}
	if len(spec.Ports) != 0 {
		t.Errorf("ports = %v, want none for a session created without the integration", spec.Ports)
	}
	for _, bind := range spec.Binds {
		if strings.Contains(bind, dockerx.VSCodeMount) {
			t.Errorf("code-server is bind mounted anyway: %v", spec.Binds)
		}
	}
	if env.vscode.ensured != 0 {
		t.Errorf("code-server was prepared %d times, want 0", env.vscode.ensured)
	}
	if script := env.bootstrapScript(0); strings.Contains(script, "code-server") {
		t.Errorf("the bootstrap starts code-server anyway: %s", script)
	}
}

// A release that cannot be prepared fails provisioning with the reason, the
// same as a clone that cannot be made.
func TestSessionFailsWhenVSCodeCannotBePrepared(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")
	env.vscode.err = fmt.Errorf("no route to github.test")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q,"vscode":true}`, image.ID)), &created)

	failed := env.waitForSessionStatus(created.ID, store.SessionStatusFailed)
	if !strings.Contains(failed.Error, "no route to github.test") {
		t.Errorf("error = %q, want the reason the release could not be prepared", failed.Error)
	}
}

// The switch is only worth anything if it reaches the bootstrap, so this walks
// the whole way: flip it, read it back, restart, and look at what tmux was
// asked to run.
func TestUpdatingAutoClaudeAppliesOnTheNextStart(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q,"autoClaude":false}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	resp := env.sendJSON(http.MethodPatch, "/api/sessions/"+created.ID, `{"autoClaude":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	var updated sessionResponse
	env.decode(resp, &updated)
	if !updated.AutoClaude {
		t.Error("the response still says autoClaude is off")
	}

	var reloaded sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions/"+created.ID, nil), &reloaded)
	if !reloaded.AutoClaude {
		t.Error("autoClaude was not persisted")
	}

	json := map[string]string{"Content-Type": "application/json"}
	env.do(http.MethodPost, "/api/sessions/"+created.ID+"/stop", json)
	env.do(http.MethodPost, "/api/sessions/"+created.ID+"/start", json)

	if script := env.bootstrapScript(1); !strings.Contains(script, "claude") {
		t.Errorf("the restarted session does not start Claude Code: %s", script)
	}
}

func TestUpdateSessionIsScopedToTheOwner(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	ctx := context.Background()
	other, err := env.store.UpsertUser(ctx, &store.User{GitHubLogin: "bob", GitHubID: 7})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	theirs, err := env.store.CreateSession(ctx, &store.Session{
		UserID: other.ID, Title: "theirs", RepoFullName: "bob/secret", ImageID: image.ID,
		ImageRef: image.ImageRef, Status: store.SessionStatusStopped, AutoClaude: true,
	})
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}

	resp := env.sendJSON(http.MethodPatch, "/api/sessions/"+theirs.ID, `{"autoClaude":false}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("patch on another user's session = %d, want 404", resp.StatusCode)
	}

	after, err := env.store.SessionByID(ctx, other.ID, theirs.ID)
	if err != nil {
		t.Fatalf("reload the other session: %v", err)
	}
	if !after.AutoClaude {
		t.Error("the refused request changed the session anyway")
	}
}

// The other half of the create flow: no repository at all. Nothing is cloned,
// /workspace is an empty directory, and with no account named the container
// carries no credentials.
func TestSessionWithoutARepository(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(`{"imageId":%q}`, image.ID)), &created)
	if created.RepoFullName != "" || created.Branch != "" || created.Provider != "" {
		t.Errorf("session = %+v, want no repository and no account", created)
	}
	if created.Title != "base" {
		t.Errorf("title = %q, want the image name: there is no repository to name it after", created.Title)
	}
	if created.PropagateToken {
		t.Error("propagateToken = true, want no token: no account was asked for")
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	if clones := env.cloner.clones(); len(clones) != 0 {
		t.Errorf("a session without a repository cloned something: %+v", clones)
	}
	// The directory is still ours to create: Docker would make the missing bind
	// source itself, owned by root, and the session runs as the host user.
	workspace := filepath.Join(env.workspaces, created.ID, "repo")
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		t.Fatalf("the workspace directory is missing: %v", err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil || len(entries) != 0 {
		t.Errorf("workspace contents = %v (%v), want it empty", entries, err)
	}
	var spec dockerx.ContainerSpec
	for _, s := range env.docker.containerSpecs() {
		spec = s
	}
	if !contains(spec.Binds, workspace+":"+dockerx.WorkspaceMount) {
		t.Errorf("the empty workspace is not mounted at %s: %v", dockerx.WorkspaceMount, spec.Binds)
	}

	containerEnv := env.containerEnv()
	for _, name := range []string{"HEXAGON_GIT_USERNAME", "HEXAGON_GIT_PASSWORD", "GITHUB_TOKEN"} {
		if _, ok := containerEnv[name]; ok {
			t.Errorf("%s reached a session that named no account: %v", name, containerEnv)
		}
	}
	if script := env.bootstrapScript(0); strings.Contains(script, "credential.helper") {
		t.Errorf("the bootstrap installs a credential helper with nothing to read: %s", script)
	}
	// Cloning something by hand is the obvious thing to do in such a session,
	// and the mount is owned by whoever owns it on the host either way.
	if script := env.bootstrapScript(0); !strings.Contains(script, "safe.directory") {
		t.Errorf("the bootstrap does not mark the workspace safe for git: %s", script)
	}
}

// The second sentence of the point: no repository, and a token anyway. The
// account is named directly, and it does not have to be the one Hexagon signs
// in with.
func TestSessionWithoutARepositoryCanStillCarryAToken(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.connectBitbucket("alice@example.test", "atlassian-token")
	image := env.readyImage("base")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"imageId":%q,"provider":"bitbucket","title":"scratch"}`, image.ID)), &created)
	if created.Provider != "bitbucket" || !created.PropagateToken {
		t.Errorf("session = %+v, want it attached to Bitbucket with its token", created)
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	if clones := env.cloner.clones(); len(clones) != 0 {
		t.Errorf("a session without a repository cloned something: %+v", clones)
	}

	containerEnv := env.containerEnv()
	if containerEnv["HEXAGON_GIT_USERNAME"] != "x-bitbucket-token" ||
		containerEnv["HEXAGON_GIT_PASSWORD"] != "atlassian-token" {
		t.Errorf("container git credentials = %v, want Bitbucket's", containerEnv)
	}
	// GITHUB_TOKEN is GitHub's name for GitHub's token, and this is not one.
	if _, ok := containerEnv["GITHUB_TOKEN"]; ok {
		t.Errorf("a Bitbucket session carries GITHUB_TOKEN: %v", containerEnv)
	}
	if script := env.bootstrapScript(0); !strings.Contains(script, "credential.helper") {
		t.Errorf("the bootstrap does not install a credential helper: %s", script)
	}
}

// Naming an account the caller does not have has to be refused here: nothing
// else in this path looks, and the session would fail to provision with a
// message about unsealing a credential that was never there.
func TestSessionWithoutARepositoryChecksTheAccount(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	cases := map[string]string{
		"not connected": `{"imageId":%q,"provider":"bitbucket"}`,
		"unknown":       `{"imageId":%q,"provider":"gitlab"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := env.postJSON("/api/sessions", fmt.Sprintf(body, image.ID)).StatusCode; got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", got)
			}
		})
	}

	var sessions []sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions", nil), &sessions)
	if len(sessions) != 0 {
		t.Errorf("a refused request left %d sessions behind", len(sessions))
	}
}

// Asking for no token and naming an account at the same time is a contradiction
// with no repository to settle it. The account is what gets dropped: it would
// otherwise be recorded as an attachment the session does not have.
func TestSessionWithoutARepositoryRefusingTheTokenDropsTheAccount(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"imageId":%q,"provider":"github","propagateToken":false}`, image.ID)), &created)
	if created.Provider != "" || created.PropagateToken {
		t.Errorf("session = %+v, want no account and no token", created)
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	if containerEnv := env.containerEnv(); containerEnv["GITHUB_TOKEN"] != "" {
		t.Errorf("GITHUB_TOKEN reached a session that refused it: %v", containerEnv)
	}
}

// A published port is a property of the container, so it is chosen when the
// session is created and reported live afterwards: Docker picks a new host port
// every time the container starts, so there is nothing to store.
func TestSessionPublishesThePortsItAskedFor(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"imageId":%q,"ports":[3000,5173]}`, image.ID)), &created)
	if len(created.Ports) != 2 || created.Ports[0].Container != 3000 || created.Ports[1].Container != 5173 {
		t.Fatalf("ports = %+v, want the two container ports", created.Ports)
	}
	// Nothing is up yet, so there is no binding to report — and saying so is the
	// truth rather than a gap.
	for _, p := range created.Ports {
		if p.Host != 0 {
			t.Errorf("port %d reported host %d before the container was running", p.Container, p.Host)
		}
	}

	running := env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	containerID, spec := env.containerOf(running.ID)
	if !slices.Contains(spec.Ports, dockerx.PortPublication{Container: 3000, Address: "127.0.0.1"}) ||
		!slices.Contains(spec.Ports, dockerx.PortPublication{Container: 5173, Address: "127.0.0.1"}) {
		t.Errorf("container ports = %v, want both published on loopback", spec.Ports)
	}

	// Once Docker has picked the host side, the session reports the pair.
	env.docker.setContainerPort(containerID, 3000, 49154)
	var refreshed sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions/"+running.ID, nil), &refreshed)
	found := false
	for _, p := range refreshed.Ports {
		if p.Container == 3000 {
			found = p.Host == 49154
		}
		if p.Container == 5173 && p.Host != 0 {
			t.Errorf("port 5173 reported host %d, want none: Docker has not bound it", p.Host)
		}
	}
	if !found {
		t.Errorf("ports = %+v, want 3000 mapped to 49154", refreshed.Ports)
	}
}

// A container keeps the port bindings it was created with, so changing them
// means building another container. That is allowed while the session is
// stopped, and what it costs — the container, not the workspace — is the reason
// it is not allowed while it runs.
func TestSessionPortsAreChangedWhileItIsStopped(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	json := map[string]string{"Content-Type": "application/json"}

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"imageId":%q,"ports":[3000]}`, image.ID)), &created)
	running := env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	first, _ := env.containerOf(running.ID)

	// While it runs the change is refused, and the container is untouched.
	if resp := env.sendJSON(http.MethodPut, "/api/sessions/"+running.ID+"/ports",
		`{"ports":[8080]}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d for a running session, want 409", resp.StatusCode)
	}

	env.do(http.MethodPost, "/api/sessions/"+running.ID+"/stop", json)

	var changed sessionResponse
	resp := env.sendJSON(http.MethodPut, "/api/sessions/"+running.ID+"/ports",
		`{"ports":[8080,5173],"portAddress":"0.0.0.0"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	env.decode(resp, &changed)
	if len(changed.Ports) != 2 || changed.Ports[0].Container != 8080 || changed.Ports[1].Container != 5173 {
		t.Errorf("ports = %+v, want the two that were asked for", changed.Ports)
	}
	if changed.PortAddress != "0.0.0.0" {
		t.Errorf("portAddress = %q, want the address that was asked for", changed.PortAddress)
	}
	// Still stopped: a port change is not a way to start a session.
	if changed.Status != store.SessionStatusStopped {
		t.Errorf("status = %q, want the session left stopped", changed.Status)
	}

	second, spec := env.containerOf(running.ID)
	if second == first {
		t.Fatalf("container %s was reused, want it rebuilt for the new bindings", first)
	}
	if !contains(env.docker.removedContainers, first) {
		t.Errorf("removed containers = %v, want the old one among them", env.docker.removedContainers)
	}
	want := []dockerx.PortPublication{{Container: 8080, Address: "0.0.0.0"}, {Container: 5173, Address: "0.0.0.0"}}
	if !slices.Equal(spec.Ports, want) {
		t.Errorf("container ports = %v, want %v", spec.Ports, want)
	}

	// The rebuilt container is the one the rest of the server acts on.
	var started sessionResponse
	env.decode(env.do(http.MethodPost, "/api/sessions/"+running.ID+"/start", json), &started)
	if started.Status != store.SessionStatusRunning {
		t.Errorf("status after start = %q, want the rebuilt container started", started.Status)
	}
}

// A request that asks for what the session already publishes rebuilds nothing.
// The browser sends back the 127.0.0.1 the API answered with, where the row
// holds the empty string that means it, and the two are the same binding.
func TestSessionPortsChangeToWhatIsAlreadyThereRebuildsNothing(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	json := map[string]string{"Content-Type": "application/json"}

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"imageId":%q,"ports":[3000]}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	env.do(http.MethodPost, "/api/sessions/"+created.ID+"/stop", json)
	before, _ := env.containerOf(created.ID)

	resp := env.sendJSON(http.MethodPut, "/api/sessions/"+created.ID+"/ports",
		`{"ports":[3000],"portAddress":"127.0.0.1"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if after, _ := env.containerOf(created.ID); after != before {
		t.Errorf("container %s was rebuilt for a request that changed nothing", after)
	}
}

// The same refusals the create path applies, applied again here: this route
// builds a container too, and a check that only guarded creation would be a
// check the user can walk around.
func TestSessionPortsChangeRefusesPortsItCannotHonour(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	json := map[string]string{"Content-Type": "application/json"}

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"imageId":%q,"vscode":true}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	env.do(http.MethodPost, "/api/sessions/"+created.ID+"/stop", json)
	before, _ := env.containerOf(created.ID)

	for _, body := range []string{
		`{"ports":[3000,3000]}`,
		`{"ports":[70000]}`,
		`{"ports":[3000],"portAddress":"not-an-address"}`,
		fmt.Sprintf(`{"ports":[%d]}`, dockerx.VSCodePort),
	} {
		resp := env.sendJSON(http.MethodPut, "/api/sessions/"+created.ID+"/ports", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d for %s, want 400", resp.StatusCode, body)
		}
	}

	if after, _ := env.containerOf(created.ID); after != before {
		t.Errorf("container %s was rebuilt for a request that was refused", after)
	}
	var unchanged sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions/"+created.ID, nil), &unchanged)
	if len(unchanged.Ports) != 0 {
		t.Errorf("ports = %+v, want none: every request was refused", unchanged.Ports)
	}
}

// The VS Code integration publishes a port of its own. A session that took it
// for something else would leave the editor's button broken for a reason nobody
// could find, so the request is refused instead.
func TestSessionRefusesPortsItCannotHonour(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	for _, body := range []string{
		fmt.Sprintf(`{"imageId":%q,"ports":[3000,3000]}`, image.ID),
		fmt.Sprintf(`{"imageId":%q,"ports":[0]}`, image.ID),
		fmt.Sprintf(`{"imageId":%q,"ports":[%d],"vscode":true}`, image.ID, dockerx.VSCodePort),
	} {
		if resp := env.postJSON("/api/sessions", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d for %s, want 400", resp.StatusCode, body)
		}
	}
}

// An advanced image starts a compose project, and the agent container is the
// one the project names: that is what keeps the terminal, the bootstrap and the
// VS Code proxy working unchanged.
func TestSessionFromAComposeImageBringsUpTheProject(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var image imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"advanced","sourceType":"compose",
		"dockerfile":"FROM busybox","compose":"services:\n  db:\n    image: postgres:16\n"}`), &image)
	env.waitForImageStatus(image.ID, store.ImageStatusReady)

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(`{"imageId":%q}`, image.ID)), &created)
	if !created.Compose {
		t.Error("a session from a compose image is not marked as a project")
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	env.compose.mu.Lock()
	calls := append([]string(nil), env.compose.calls...)
	env.compose.mu.Unlock()
	if !slices.Contains(calls, "up") {
		t.Errorf("compose calls = %v, want the project to have been brought up", calls)
	}
	// No container was created directly: the project owns them all.
	if n := env.docker.createdContainers(); n != 0 {
		t.Errorf("created %d containers outside the project, want none", n)
	}

	// The two halves of the project are on disk, and the generated one carries
	// Hexagon's own service.
	dir := filepath.Join(env.workspaces, created.ID, "compose")
	if _, err := os.Stat(filepath.Join(dir, "user.yaml")); err != nil {
		t.Errorf("the user's compose file was not written: %v", err)
	}
	overlay, err := os.ReadFile(filepath.Join(dir, "hexagon.yaml"))
	if err != nil {
		t.Fatalf("the generated compose file was not written: %v", err)
	}
	if !strings.Contains(string(overlay), "hexagon-"+created.ID) {
		t.Errorf("the generated file does not name the agent container:\n%s", overlay)
	}
}

// Without `docker compose` the request is refused rather than provisioned into
// a session that could never start.
func TestSessionFromAComposeImageWithoutCompose(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var image imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"advanced","sourceType":"compose",
		"dockerfile":"FROM busybox","compose":"services:\n  db:\n    image: postgres:16\n"}`), &image)
	env.waitForImageStatus(image.ID, store.ImageStatusReady)

	env.withoutCompose()
	resp := env.postJSON("/api/sessions", fmt.Sprintf(`{"imageId":%q}`, image.ID))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// Rebuilding replaces a container, and between removing the old one and
// creating the new one there is a moment with none. If the second half fails
// the session has nothing behind it, and the row has to say so: anything else
// leaves it pointing at a container id that no longer exists.
func TestSessionPortsChangeThatCannotRebuildLeavesTheSessionGone(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	json := map[string]string{"Content-Type": "application/json"}

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(`{"imageId":%q}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	env.do(http.MethodPost, "/api/sessions/"+created.ID+"/stop", json)

	env.docker.mu.Lock()
	env.docker.createErr = errors.New("no room for another container")
	env.docker.mu.Unlock()

	resp := env.sendJSON(http.MethodPut, "/api/sessions/"+created.ID+"/ports", `{"ports":[3000]}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	var after sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions/"+created.ID, nil), &after)
	if after.Status != store.SessionStatusGone {
		t.Errorf("status = %q, want %q: the container was removed and not replaced",
			after.Status, store.SessionStatusGone)
	}
	if after.Error == "" {
		t.Error("the session carries no explanation of what happened to its container")
	}
}

// A compose session takes new ports the same way, except that compose owns the
// containers: Hexagon rewrites its half of the project and lets `compose
// create` replace what changed.
func TestComposeSessionPortsAreChangedThroughTheProject(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	json := map[string]string{"Content-Type": "application/json"}

	var image imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"advanced","sourceType":"compose",
		"dockerfile":"FROM busybox","compose":"services:\n  db:\n    image: postgres:16\n"}`), &image)
	env.waitForImageStatus(image.ID, store.ImageStatusReady)

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(`{"imageId":%q}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	env.do(http.MethodPost, "/api/sessions/"+created.ID+"/stop", json)

	resp := env.sendJSON(http.MethodPut, "/api/sessions/"+created.ID+"/ports", `{"ports":[3000]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	env.compose.mu.Lock()
	calls := append([]string(nil), env.compose.calls...)
	env.compose.mu.Unlock()
	if !slices.Contains(calls, "create") {
		t.Errorf("compose calls = %v, want the project asked to recreate what changed", calls)
	}
	// Not `up`: a stopped session stays stopped while it takes a new container.
	if slices.Contains(calls[slices.Index(calls, "stop"):], "up") {
		t.Errorf("compose calls = %v, want nothing started after the stop", calls)
	}
	// No container was created outside the project either.
	if n := env.docker.createdContainers(); n != 0 {
		t.Errorf("created %d containers outside the project, want none", n)
	}

	overlay, err := os.ReadFile(filepath.Join(env.workspaces, created.ID, "compose", "hexagon.yaml"))
	if err != nil {
		t.Fatalf("read the generated compose file: %v", err)
	}
	if !strings.Contains(string(overlay), "127.0.0.1::3000") {
		t.Errorf("the generated file does not publish the new port:\n%s", overlay)
	}
}

// A Hexagon on a remote machine is the case published ports exist for, and a
// port on that machine's loopback interface is reachable by nobody. The address
// is the session's own choice, fixed with the binding.
func TestSessionPublishesOnTheAddressItAskedFor(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"imageId":%q,"ports":[3000],"portAddress":"0.0.0.0","vscode":true}`, image.ID)), &created)
	if created.PortAddress != "0.0.0.0" {
		t.Errorf("portAddress = %q, want the address that was asked for", created.PortAddress)
	}

	running := env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	_, spec := env.containerOf(running.ID)
	if !slices.Contains(spec.Ports, dockerx.PortPublication{Container: 3000, Address: "0.0.0.0"}) {
		t.Errorf("ports = %v, want 3000 on every interface", spec.Ports)
	}
	// code-server stays on loopback whatever the session chose: it asks nobody
	// for anything, so a copy of it on a public interface is an
	// unauthenticated shell in the workspace.
	if !slices.Contains(spec.Ports, dockerx.PortPublication{Container: dockerx.VSCodePort, Address: "127.0.0.1"}) {
		t.Errorf("ports = %v, want code-server left on loopback", spec.Ports)
	}
}

// A client that says nothing gets the closed answer. The browser proposes
// 0.0.0.0 with a warning beside it; the API does not.
func TestSessionWithoutAnAddressStaysOnLoopback(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions",
		fmt.Sprintf(`{"imageId":%q,"ports":[3000]}`, image.ID)), &created)
	if created.PortAddress != "127.0.0.1" {
		t.Errorf("portAddress = %q, want loopback for a request that named none", created.PortAddress)
	}

	running := env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	_, spec := env.containerOf(running.ID)
	if !slices.Contains(spec.Ports, dockerx.PortPublication{Container: 3000, Address: "127.0.0.1"}) {
		t.Errorf("ports = %v, want 3000 on loopback", spec.Ports)
	}
}

func TestSessionRefusesAnAddressItCannotPublishOn(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	for _, address := range []string{"hexagon.example", "0.0.0", "not an address"} {
		body := fmt.Sprintf(`{"imageId":%q,"ports":[3000],"portAddress":%s}`, image.ID, quote(address))
		if resp := env.postJSON("/api/sessions", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d for %q, want 400", resp.StatusCode, address)
		}
	}
}

// The reason the second secret exists: a session created while a personal
// access token is stored authenticates git with that, not with the OAuth token
// from signing in, which is the one that expires under a running container.
func TestSessionCarriesThePersonalAccessTokenWhenThereIsOne(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	env.decode(env.sendJSON(http.MethodPut, "/api/accounts/github/git-token",
		`{"secret":"ghp_personal"}`), &accountResponse{})

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	// Both halves of "everything git": the clone on the host, and the container
	// that has to go on pushing long after the sign-in token has died.
	clones := env.cloner.clones()
	if len(clones) != 1 {
		t.Fatalf("made %d clones, want 1", len(clones))
	}
	if clones[0].Token != "ghp_personal" {
		t.Errorf("clone token = %q, want the personal access token", clones[0].Token)
	}

	containerEnv := env.containerEnv()
	if containerEnv["HEXAGON_GIT_PASSWORD"] != "ghp_personal" {
		t.Errorf("HEXAGON_GIT_PASSWORD = %q, want the personal access token",
			containerEnv["HEXAGON_GIT_PASSWORD"])
	}
	if containerEnv["GITHUB_TOKEN"] != "ghp_personal" {
		t.Errorf("GITHUB_TOKEN = %q, want the personal access token: the reference image's "+
			"askpass and gh both read it under that name", containerEnv["GITHUB_TOKEN"])
	}
}

// And with none stored nothing moves, which is every session that exists today.
func TestSessionCarriesTheSignInTokenWithoutAPersonalAccessToken(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	if clones := env.cloner.clones(); len(clones) != 1 || clones[0].Token != "gho_token" {
		t.Errorf("clones = %+v, want the token from signing in", clones)
	}
	if containerEnv := env.containerEnv(); containerEnv["GITHUB_TOKEN"] != "gho_token" {
		t.Errorf("GITHUB_TOKEN = %q, want the token from signing in", containerEnv["GITHUB_TOKEN"])
	}
}
