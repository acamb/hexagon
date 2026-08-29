// Package session orchestrates Claude Code sessions: a repository clone on the
// host, a container built from a base image with that clone bind mounted, and a
// tmux session inside it waiting for a terminal to attach.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/gitops"
	"github.com/andrea/hexagon/internal/provider"
	"github.com/andrea/hexagon/internal/store"
	"github.com/google/uuid"
)

const (
	// provisionTimeout bounds the whole setup: cloning a large repository is
	// the slow part, the container itself is quick.
	provisionTimeout = 30 * time.Minute
	// stopTimeout is how long a container gets to exit before it is killed.
	// Nothing in it needs a graceful shutdown; tmux state lives in memory and
	// is lost either way.
	stopTimeout = 10 * time.Second
	// containerNamePrefix makes Hexagon's containers recognisable in docker ps.
	containerNamePrefix = "hexagon-"
)

// TmuxSession is the tmux session a terminal attaches to. The bootstrap creates
// it and the terminal handler joins it, so the name lives here rather than in
// both: if the two ever drifted, the browser would silently attach to an empty
// session instead of the one that was set up.
const TmuxSession = "main"

// Errors the HTTP layer turns into specific status codes.
var (
	ErrImageNotFound = errors.New("image not found")
	ErrImageNotReady = errors.New("image is not ready")
	// ErrVSCodeUnavailable reports a session asking for the VS Code integration
	// when the server has no way to provide a code-server release.
	ErrVSCodeUnavailable = errors.New("vscode integration is not available")
	// ErrVSCodeDisabled reports a session that was not created with the VS Code
	// integration.
	ErrVSCodeDisabled = errors.New("session was not created with vscode")
	// ErrVSCodeNotReady reports a session whose container is not yet publishing
	// code-server: not running, or started before the port binding exists.
	ErrVSCodeNotReady = errors.New("vscode is not ready in this session yet")
)

// CredentialSource hands over the git credentials for one of a user's connected
// accounts. It is an interface so the orchestrator never sees the cipher, and
// so tests do not need one.
type CredentialSource interface {
	GitCredentials(ctx context.Context, userID string, kind provider.Kind) (provider.GitAuth, error)
}

// Cloner checks a repository out on the host. The indirection exists so the
// orchestration can be tested without hitting the network.
type Cloner interface {
	Clone(ctx context.Context, opts gitops.Options) error
}

// GitCloner is the real implementation, backed by the git command.
type GitCloner struct{}

func (GitCloner) Clone(ctx context.Context, opts gitops.Options) error {
	return gitops.Clone(ctx, opts)
}

// VSCodeSource provides the code-server release to bind mount into a session
// created with the integration. It is nil when the server has no way to get
// one, which a session is told about rather than being failed silently.
type VSCodeSource interface {
	Ensure(ctx context.Context) (string, error)
}

// Config is what the manager needs from the process configuration.
type Config struct {
	WorkspaceRoot string
	// ClaudeCredentials is the host file bind mounted read-only into the
	// container. Empty disables the mount.
	ClaudeCredentials string
	AnthropicAPIKey   string
	GitUserName       string
	GitUserEmail      string
	// ContainerUser is the uid:gid session containers run as.
	ContainerUser string
}

// Manager creates and controls sessions.
type Manager struct {
	store       *store.Store
	docker      dockerx.API
	cloner      Cloner
	credentials CredentialSource
	// vscode provides the code-server release for sessions created with the
	// integration. Nil is legal: it means the server has none to offer.
	vscode VSCodeSource
	cfg    Config
	log    *slog.Logger
}

// NewManager wires the orchestrator.
func NewManager(st *store.Store, docker dockerx.API, cloner Cloner, credentials CredentialSource, vscode VSCodeSource, cfg Config, log *slog.Logger) *Manager {
	return &Manager{store: st, docker: docker, cloner: cloner, credentials: credentials, vscode: vscode, cfg: cfg, log: log}
}

// CreateRequest describes the session to set up. The repository is named by the
// caller but resolved against their GitHub account before it gets here, so the
// clone URL is one GitHub gave us rather than one the browser made up.
type CreateRequest struct {
	Title string
	// Provider is the connected account this session is attached to: the one
	// its repository came from, or — for a session created without a
	// repository — the one whose credentials it asked for. Empty means the
	// session is attached to no account and carries no credentials.
	Provider provider.Kind
	// The repository, or all three empty for a session that starts on an empty
	// workspace instead of a clone.
	RepoFullName string
	RepoCloneURL string
	Branch       string
	ImageID      string
	// AutoClaude starts Claude Code in the session's tmux rather than leaving a
	// shell.
	AutoClaude bool
	// PropagateToken hands the provider credentials to the container as well as
	// using them for the clone. Off means the session can read the repository
	// it was created with and authenticate to nothing.
	PropagateToken bool
	// VSCode asks for the container to publish code-server and have the
	// release bind mounted. Decided here because the mount and the port
	// binding are properties of the container, fixed when it is created.
	VSCode bool
}

// Create records the session and provisions it in the background. It returns as
// soon as the row exists, so the UI can follow the status from there.
func (m *Manager) Create(ctx context.Context, user *store.User, req CreateRequest) (*store.Session, error) {
	image, err := m.store.ImageByID(ctx, user.ID, req.ImageID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, ErrImageNotFound
	case err != nil:
		return nil, err
	case image.Status != store.ImageStatusReady:
		return nil, fmt.Errorf("%w: %s is %s", ErrImageNotReady, image.Name, image.Status)
	}
	if req.VSCode && m.vscode == nil {
		return nil, ErrVSCodeUnavailable
	}

	// A session attached to no account has nothing to unseal: no clone to
	// authenticate and no credentials to hand over.
	var credentials provider.GitAuth
	if req.Provider != "" {
		credentials, err = m.credentials.GitCredentials(ctx, user.ID, req.Provider)
		if err != nil {
			return nil, fmt.Errorf("read the stored %s credentials: %w", req.Provider, err)
		}
	}

	// The workspace path is derived from the id, so it has to exist first.
	id := uuid.NewString()
	workspace := filepath.Join(m.cfg.WorkspaceRoot, id)

	title := strings.TrimSpace(req.Title)
	if title == "" {
		// The image is all a session without a repository has to be named
		// after; a fallback that is always there beats a precise one that
		// sometimes is not.
		title = req.RepoFullName
		if title == "" {
			title = image.Name
		}
	}

	session, err := m.store.CreateSession(ctx, &store.Session{
		ID:             id,
		UserID:         user.ID,
		Title:          title,
		Provider:       string(req.Provider),
		RepoFullName:   req.RepoFullName,
		RepoCloneURL:   req.RepoCloneURL,
		Branch:         req.Branch,
		ImageID:        image.ID,
		ImageRef:       image.ImageRef,
		WorkspaceDir:   workspace,
		RepoDir:        filepath.Join(workspace, "repo"),
		AutoClaude:     req.AutoClaude,
		PropagateToken: req.PropagateToken,
		VSCode:         req.VSCode,
		Status:         store.SessionStatusCreating,
	})
	if err != nil {
		return nil, err
	}

	go m.provision(session, credentials)
	return session, nil
}

// provision walks the session from an empty directory to a running container
// with tmux in it. It runs on its own context: a browser navigating away must
// not cancel a clone half way through.
func (m *Manager) provision(session *store.Session, credentials provider.GitAuth) {
	ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
	defer cancel()

	if err := m.provisionSteps(ctx, session, credentials); err != nil {
		m.log.Error("provision session", "session", session.ID, "err", err)
		m.cleanUpFailure(session)
		m.setStatus(session.ID, store.SessionStatusFailed, err.Error())
		return
	}
	m.setStatus(session.ID, store.SessionStatusRunning, "")
	m.log.Info("session running", "session", session.ID, "repo", session.RepoFullName)
}

func (m *Manager) provisionSteps(ctx context.Context, session *store.Session, credentials provider.GitAuth) error {
	homeDir := filepath.Join(session.WorkspaceDir, "home")
	// The container runs as the host user with HOME here, and Claude Code wants
	// somewhere to keep its own state.
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o700); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}

	if err := seedClaudeConfig(homeDir); err != nil {
		return err
	}

	if session.RepoCloneURL != "" {
		m.setStatus(session.ID, store.SessionStatusCloning, "")
		err := m.cloner.Clone(ctx, gitops.Options{
			CloneURL:  session.RepoCloneURL,
			Branch:    session.Branch,
			Dest:      session.RepoDir,
			Username:  credentials.Username,
			Token:     credentials.Secret,
			UserName:  m.cfg.GitUserName,
			UserEmail: m.cfg.GitUserEmail,
		})
		if err != nil {
			return err
		}
	} else if err := os.MkdirAll(session.RepoDir, 0o700); err != nil {
		// A session without a repository still gets the directory: it is what
		// is bind mounted at /workspace, and Docker would otherwise create it
		// itself, owned by root.
		return fmt.Errorf("create workspace: %w", err)
	}

	var vscodeDir string
	if session.VSCode {
		dir, err := m.vscode.Ensure(ctx)
		if err != nil {
			return fmt.Errorf("prepare vscode: %w", err)
		}
		vscodeDir = dir
	}

	m.setStatus(session.ID, store.SessionStatusCreating, "")
	containerID, err := m.docker.CreateContainer(ctx, m.containerSpec(session, homeDir, vscodeDir, credentials))
	if err != nil {
		return err
	}
	session.ContainerID = containerID
	if err := m.store.SetSessionContainer(ctx, session.ID, containerID); err != nil {
		return err
	}

	m.setStatus(session.ID, store.SessionStatusStarting, "")
	if err := m.docker.StartContainer(ctx, containerID); err != nil {
		return err
	}
	return m.bootstrap(ctx, session)
}

// seedClaudeConfig writes the state Claude Code keeps outside its credentials
// file. The credentials mount alone is not enough: without $HOME/.claude.json,
// Claude Code sees a machine it has never run on and starts its first-run
// onboarding, which looks like being asked to sign in again.
//
// Only two things are seeded, and Claude Code fills in the rest on first start:
//
//   - hasCompletedOnboarding, because the account behind the mounted
//     credentials has been through onboarding already;
//   - trust for /workspace, because the repository was chosen by this user from
//     their own GitHub account — the question the prompt asks was answered when
//     the session was created.
//
// The host's own ~/.claude.json is deliberately not mounted: it is a large file
// of personal state that Claude Code writes to constantly, and every session
// would be fighting over it.
func seedClaudeConfig(homeDir string) error {
	path := filepath.Join(homeDir, ".claude.json")
	if _, err := os.Stat(path); err == nil {
		// A session being provisioned into an existing workspace keeps whatever
		// state it already had.
		return nil
	}

	body, err := json.MarshalIndent(map[string]any{
		"hasCompletedOnboarding": true,
		"projects": map[string]any{
			dockerx.WorkspaceMount: map[string]any{"hasTrustDialogAccepted": true},
		},
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("build claude config: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write claude config: %w", err)
	}
	return nil
}

// containerSpec is the whole contract between Hexagon and a session container.
func (m *Manager) containerSpec(session *store.Session, homeDir, vscodeDir string, credentials provider.GitAuth) dockerx.ContainerSpec {
	env := []string{
		"HOME=" + dockerx.AgentHome,
		"TERM=xterm-256color",
	}
	// The clone on the host has already used these; whether the container gets
	// them too is the session's own choice, made when it was created. There is
	// no way to revisit it here: a container keeps the environment it was
	// created with.
	if credentials.Secret != "" && session.PropagateToken {
		// Provider-neutral, because the credential helper the bootstrap
		// installs is the same whichever account the repository came from.
		env = append(env,
			gitUserEnv+"="+credentials.Username,
			gitSecretEnv+"="+credentials.Secret,
		)
		// GITHUB_TOKEN as well for a GitHub session: the reference image bakes
		// in an askpass that reads it, and tools in the container — gh, and
		// Claude Code itself — expect it under that name.
		if session.Provider == string(provider.GitHub) {
			env = append(env, "GITHUB_TOKEN="+credentials.Secret)
		}
	}
	if m.cfg.GitUserName != "" {
		env = append(env, "GIT_AUTHOR_NAME="+m.cfg.GitUserName, "GIT_COMMITTER_NAME="+m.cfg.GitUserName)
	}
	if m.cfg.GitUserEmail != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+m.cfg.GitUserEmail, "GIT_COMMITTER_EMAIL="+m.cfg.GitUserEmail)
	}
	if m.cfg.AnthropicAPIKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+m.cfg.AnthropicAPIKey)
	}

	binds := []string{
		session.RepoDir + ":" + dockerx.WorkspaceMount,
		homeDir + ":" + dockerx.AgentHome,
	}
	// Read-only: the container gets to use the credentials, not to change them.
	if path := m.cfg.ClaudeCredentials; path != "" {
		if _, err := os.Stat(path); err == nil {
			binds = append(binds, path+":"+dockerx.AgentHome+"/.claude/.credentials.json:ro")
		} else {
			m.log.Warn("claude credentials not found, sessions will need their own login",
				"path", path, "err", err)
		}
	}
	var ports []int
	if vscodeDir != "" {
		// Read-only: the container gets to run the editor, not to modify it.
		// Everything code-server writes goes under $HOME, which is already a
		// bind mount of the session's own directory, so its settings and
		// extensions survive a restart.
		binds = append(binds, vscodeDir+":"+dockerx.VSCodeMount+":ro")
		ports = []int{dockerx.VSCodePort}
	}

	return dockerx.ContainerSpec{
		Name:  containerNamePrefix + session.ID,
		Image: session.ImageRef,
		// The container is a place to run things, not a process in itself. The
		// terminal and the bootstrap arrive later as execs.
		Cmd:        []string{"sleep", "infinity"},
		Env:        env,
		WorkingDir: dockerx.WorkspaceMount,
		User:       m.cfg.ContainerUser,
		Labels: map[string]string{
			dockerx.LabelManaged:   "true",
			dockerx.LabelSessionID: session.ID,
		},
		Binds:       binds,
		AutoRestart: true,
		Ports:       ports,
	}
}

// claudeCommand is what a session that starts Claude Code runs in its tmux.
//
// The shell after it is not decoration. When the command a tmux session was
// created with exits, its window closes, and the last window closing takes the
// session with it: a claude missing from the image, or one whose credentials
// have expired, would end the terminal for no visible reason. This turns that
// into "Claude Code exited, here is a prompt". The fallback is sh rather than
// bash because the reference base image is a reference, not a guarantee.
const claudeCommand = `claude; exec "${SHELL:-sh}"`

// Environment variables carrying the git credentials into the container. The
// bootstrap's credential helper reads them by name, so they are part of the
// contract between the two.
const (
	gitUserEnv   = "HEXAGON_GIT_USERNAME"
	gitSecretEnv = "HEXAGON_GIT_PASSWORD"
)

// containerCredentialHelper teaches git inside the container to answer with the
// credentials Hexagon put in its environment.
//
// It is installed here rather than baked into the image on purpose. The
// reference Dockerfile carries an askpass that answers x-access-token and
// $GITHUB_TOKEN, which is GitHub and nothing else; a Bitbucket session in an
// image someone built last month would fail to authenticate, and Hexagon cannot
// tell what is inside an image it did not build. A helper written at bootstrap
// works with any image, wins over GIT_ASKPASS, and still keeps the secret out of
// every file: what is written names the variables, it does not contain them.
const containerCredentialHelper = `!f(){ echo "username=$` + gitUserEnv + `"; echo "password=$` + gitSecretEnv + `"; }; f`

// bootstrapScript prepares a freshly started container: git has to be told the
// bind mounted workspace is safe to use (its owner may not match inside the
// container) and how to authenticate to the provider, and tmux has to be
// running before a terminal can attach.
//
// safe.directory is set even for a session that started without a repository,
// because cloning one by hand is the obvious thing to do in it, and the mount
// would refuse to be used for the same reason a clone made outside would.
//
// Whether Claude Code starts by itself is decided here, once, because it is the
// command the tmux session is created with — which is why flipping the switch
// on a running session only shows up the next time it starts.
func bootstrapScript(autoClaude, credentials, vscode bool) string {
	newSession := "tmux new-session -d -s " + TmuxSession + " -c " + dockerx.WorkspaceMount
	if autoClaude {
		newSession += " '" + claudeCommand + "'"
	}

	script := "set -e\n" +
		"git config --global --add safe.directory " + dockerx.WorkspaceMount + "\n"
	if credentials {
		script += "git config --global credential.helper '" + containerCredentialHelper + "'\n"
	}
	script += "tmux has-session -t " + TmuxSession + " 2>/dev/null || " + newSession + "\n"
	if vscode {
		script += vscodeCommand + "\n"
	}
	return strings.TrimSuffix(script, "\n")
}

// vscodeCommand starts code-server detached, when the session asks for it.
//
// It is started with its output redirected to a file rather than left attached:
// RunExec only returns once nothing holds its stdout open, so a background
// process that inherited the pipe would hold provisioning open for as long as
// the editor ran.
//
// It binds 0.0.0.0 and not loopback: a published port is forwarded to the
// container's own interface, and a server on the container's loopback would
// never see a packet — a failure that looks exactly like a server that did not
// start.
//
// The guard is a pid file rather than pgrep because the bootstrap runs as
// `sh -c "<script>"`, so its own command line contains the code-server path and
// `pgrep -f` would match it every time and never start anything.
var vscodeCommand = `if ! kill -0 "$(cat /tmp/hexagon-code-server.pid 2>/dev/null)" 2>/dev/null; then
  nohup ` + dockerx.VSCodeMount + `/bin/code-server --bind-addr 0.0.0.0:` + strconv.Itoa(dockerx.VSCodePort) + ` --auth none \
    --disable-telemetry --disable-update-check --disable-workspace-trust \
    ` + dockerx.WorkspaceMount + ` >/tmp/hexagon-code-server.log 2>&1 &
  echo $! >/tmp/hexagon-code-server.pid
fi`

func (m *Manager) bootstrap(ctx context.Context, session *store.Session) error {
	containerID := session.ContainerID
	// The container was created with the credentials in its environment, or
	// deliberately without them, so whether to install the helper is a question
	// about the session rather than about anything the bootstrap can see. A
	// helper with no variables to read would answer with an empty username and
	// password, turning "no credentials" into a confusing rejection.
	//
	// The provider, not the clone URL, is what says the environment has them: a
	// session created without a repository can still have asked for a token.
	script := bootstrapScript(session.AutoClaude, session.PropagateToken && session.Provider != "", session.VSCode)
	output, code, err := m.docker.RunExec(ctx, containerID, []string{"sh", "-c", script})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("bootstrap failed (exit %d): %s", code, output)
	}
	return nil
}

// cleanUpFailure removes a container left behind by a failed setup. The
// workspace is deliberately kept: it is the evidence for what went wrong.
func (m *Manager) cleanUpFailure(session *store.Session) {
	if session.ContainerID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.docker.RemoveContainer(ctx, session.ContainerID, true); err != nil {
		m.log.Warn("remove the container of a failed session", "session", session.ID, "err", err)
	}
}

func (m *Manager) setStatus(id, status, errMessage string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.store.SetSessionStatus(ctx, id, status, errMessage); err != nil {
		m.log.Error("record session status", "session", id, "status", status, "err", err)
	}
}
