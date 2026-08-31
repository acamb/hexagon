package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/composex"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/provider"
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
	manager := NewManager(st, docker, nil, nil, nil, nil, Config{WorkspaceRoot: root}, slog.New(slog.DiscardHandler))
	return manager, st, root
}

// fakeCompose records what the manager asked of the compose CLI, so a test can
// say that a compose session's lifecycle went through the project rather than
// through one container.
type fakeCompose struct {
	services []string
	err      error
	calls    []string
	downV    bool
}

func (f *fakeCompose) Validate(context.Context, string) ([]string, error) {
	f.calls = append(f.calls, "validate")
	return f.services, f.err
}

func (f *fakeCompose) Up(context.Context, composex.Project) error {
	f.calls = append(f.calls, "up")
	return f.err
}

func (f *fakeCompose) Create(context.Context, composex.Project) error {
	f.calls = append(f.calls, "create")
	return f.err
}

func (f *fakeCompose) Start(context.Context, composex.Project) error {
	f.calls = append(f.calls, "start")
	return f.err
}

func (f *fakeCompose) Stop(context.Context, composex.Project) error {
	f.calls = append(f.calls, "stop")
	return f.err
}

func (f *fakeCompose) Down(_ context.Context, _ composex.Project, volumes bool) error {
	f.calls, f.downV = append(f.calls, "down"), volumes
	return f.err
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

// A credential configured in the UI outranks the one the process was started
// with: it is the one the user can see, change and be told about.
func TestContainerSpecPrefersTheStoredClaudeCredentialOverTheConfiguredKey(t *testing.T) {
	manager, _, _ := testManager(t, nil)
	manager.cfg.AnthropicAPIKey = "configured-key"

	session := &store.Session{ID: "s-1", RepoDir: "/repo", ImageRef: "ref"}

	withCredential := manager.containerSpec(session, "/home", "", provider.GitAuth{},
		claudex.Credential{Kind: claudex.KindAPIKey, Secret: "stored-key"})
	if !slices.Contains(withCredential.Env, "ANTHROPIC_API_KEY=stored-key") {
		t.Errorf("env = %v, want the stored credential", withCredential.Env)
	}
	if slices.Contains(withCredential.Env, "ANTHROPIC_API_KEY=configured-key") {
		t.Errorf("env = %v, the configured key shadowed the stored credential", withCredential.Env)
	}

	withoutCredential := manager.containerSpec(session, "/home", "", provider.GitAuth{}, claudex.Credential{})
	if !slices.Contains(withoutCredential.Env, "ANTHROPIC_API_KEY=configured-key") {
		t.Errorf("env = %v, want a fallback to the configured key", withoutCredential.Env)
	}
}

// The credentials mount is a different mechanism from the environment variable
// and must survive untouched whichever way a container was authenticated.
func TestContainerSpecKeepsTheCredentialsMountRegardless(t *testing.T) {
	manager, _, _ := testManager(t, nil)
	credentialsPath := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(credentialsPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write credentials file: %v", err)
	}
	manager.cfg.ClaudeCredentials = credentialsPath

	session := &store.Session{ID: "s-1", RepoDir: "/repo", ImageRef: "ref"}
	wantMount := credentialsPath + ":" + dockerx.AgentHome + "/.claude/.credentials.json:ro"

	withCredential := manager.containerSpec(session, "/home", "", provider.GitAuth{},
		claudex.Credential{Kind: claudex.KindAPIKey, Secret: "stored-key"})
	if !slices.Contains(withCredential.Binds, wantMount) {
		t.Errorf("binds = %v, want the credentials mount", withCredential.Binds)
	}

	withoutCredential := manager.containerSpec(session, "/home", "", provider.GitAuth{}, claudex.Credential{})
	if !slices.Contains(withoutCredential.Binds, wantMount) {
		t.Errorf("binds = %v, want the credentials mount", withoutCredential.Binds)
	}
}

// The generated compose service and the ContainerSpec for the same session must
// describe the same container. Two hand-written lists would drift inside a
// milestone; this is the test that says they are one list rendered twice.
func TestComposeServiceMatchesTheContainerSpec(t *testing.T) {
	manager, _, _ := testManager(t, nil)
	manager.cfg.ContainerUser = "1000:1000"

	session := &store.Session{ID: "s-1", RepoDir: "/repo", ImageRef: "ref",
		Ports: []int{3000}, PortAddress: "0.0.0.0", Compose: true}
	spec := manager.containerSpec(session, "/home", "", provider.GitAuth{}, claudex.Credential{})

	rendered, err := composeOverlay(spec, []string{"db"})
	if err != nil {
		t.Fatalf("composeOverlay: %v", err)
	}
	var file struct {
		Services map[string]composeService `json:"services"`
	}
	if err := json.Unmarshal(rendered, &file); err != nil {
		t.Fatalf("the rendered file is not readable: %v\n%s", err, rendered)
	}

	agent, ok := file.Services[composex.AgentService]
	if !ok {
		t.Fatalf("no %q service in the rendered file:\n%s", composex.AgentService, rendered)
	}
	if agent.Image != spec.Image {
		t.Errorf("image = %q, want %q", agent.Image, spec.Image)
	}
	if agent.ContainerName != spec.Name {
		t.Errorf("container_name = %q, want %q — the terminal finds the container by this name", agent.ContainerName, spec.Name)
	}
	if agent.User != spec.User {
		t.Errorf("user = %q, want %q", agent.User, spec.User)
	}
	if agent.WorkingDir != spec.WorkingDir {
		t.Errorf("working_dir = %q, want %q", agent.WorkingDir, spec.WorkingDir)
	}
	if !slices.Equal(agent.Volumes, spec.Binds) {
		t.Errorf("volumes = %v, want the spec's binds %v", agent.Volumes, spec.Binds)
	}
	if !slices.Equal(agent.Environment, spec.Env) {
		t.Errorf("environment = %v, want the spec's env %v", agent.Environment, spec.Env)
	}
	if !slices.Equal(agent.Command, spec.Cmd) {
		t.Errorf("command = %v, want the spec's command %v", agent.Command, spec.Cmd)
	}
	if !maps.Equal(agent.Labels, spec.Labels) {
		t.Errorf("labels = %v, want the spec's labels %v", agent.Labels, spec.Labels)
	}
	// A published port keeps the host side to Docker and the interface the
	// session chose, exactly as dockerx does.
	if !slices.Equal(agent.Ports, []string{"0.0.0.0::3000"}) {
		t.Errorf("ports = %v, want the container port on the session's address", agent.Ports)
	}
	// The agent exists to use the services, so it starts after them.
	if !slices.Equal(agent.DependsOn, []string{"db"}) {
		t.Errorf("depends_on = %v, want the user's services", agent.DependsOn)
	}

	// The user's services are labelled with their session, which is what stops
	// the reconciler reporting each of them as a container that lost its row.
	db := file.Services["db"]
	if db.Labels[dockerx.LabelSessionID] != "s-1" || db.Labels[dockerx.LabelRole] != serviceRole {
		t.Errorf("db labels = %v, want the session id and the service role", db.Labels)
	}
	if db.Image != "" {
		t.Errorf("db.image = %q: the overlay must only add labels, not redefine the service", db.Image)
	}
}

func TestCreateRejectsAComposeImageWithNoCompose(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := testManager(t, stubDocker{})

	user, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	image, err := st.CreateImage(ctx, &store.Image{UserID: user.ID, Name: "advanced",
		SourceType: store.ImageSourceCompose, Compose: "services: {}", ImageRef: "ref",
		Status: store.ImageStatusReady})
	if err != nil {
		t.Fatalf("create image: %v", err)
	}

	_, err = manager.Create(ctx, user, CreateRequest{ImageID: image.ID})
	if !errors.Is(err, ErrComposeUnavailable) {
		t.Errorf("error = %v, want ErrComposeUnavailable", err)
	}
}

func TestCheckPorts(t *testing.T) {
	cases := []struct {
		name    string
		ports   []int
		address string
		vscode  bool
		want    string
	}{
		{name: "nothing published"},
		{name: "an ordinary port", ports: []int{3000, 8080}},
		{name: "the vscode port without the integration", ports: []int{dockerx.VSCodePort}},
		{name: "every interface", ports: []int{3000}, address: "0.0.0.0"},
		{name: "one interface", ports: []int{3000}, address: "192.0.2.10"},
		{name: "every interface, v6", ports: []int{3000}, address: "::"},
		{name: "zero", ports: []int{0}, want: "not a port"},
		{name: "above the range", ports: []int{70000}, want: "not a port"},
		{name: "a duplicate", ports: []int{3000, 3000}, want: "twice"},
		{name: "too many", ports: make([]int, maxSessionPorts+1), want: "at most"},
		{name: "the vscode port with the integration", ports: []int{dockerx.VSCodePort}, vscode: true, want: "VS Code"},
		// A name would have to be resolved somewhere, and resolving it here
		// would only move the surprise to the daemon.
		{name: "a host name", ports: []int{3000}, address: "hexagon.example", want: "not an address"},
		{name: "nonsense", ports: []int{3000}, address: "0.0.0", want: "not an address"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkPorts(c.ports, c.address, c.vscode)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("checkPorts = %v, want it accepted", err)
			case c.want == "":
			case err == nil:
				t.Errorf("checkPorts accepted %v", c.ports)
			case !strings.Contains(err.Error(), c.want):
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}
