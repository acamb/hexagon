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
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/gitops"
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
)

// TokenSource unseals the GitHub token stored for a user.
type TokenSource interface {
	GitHubToken(user *store.User) (string, error)
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
	store  *store.Store
	docker dockerx.API
	cloner Cloner
	tokens TokenSource
	cfg    Config
	log    *slog.Logger
}

// NewManager wires the orchestrator.
func NewManager(st *store.Store, docker dockerx.API, cloner Cloner, tokens TokenSource, cfg Config, log *slog.Logger) *Manager {
	return &Manager{store: st, docker: docker, cloner: cloner, tokens: tokens, cfg: cfg, log: log}
}

// CreateRequest describes the session to set up. The repository is named by the
// caller but resolved against their GitHub account before it gets here, so the
// clone URL is one GitHub gave us rather than one the browser made up.
type CreateRequest struct {
	Title        string
	RepoFullName string
	RepoCloneURL string
	Branch       string
	ImageID      string
	// AutoClaude starts Claude Code in the session's tmux rather than leaving a
	// shell.
	AutoClaude bool
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

	token, err := m.tokens.GitHubToken(user)
	if err != nil {
		return nil, fmt.Errorf("read the stored GitHub token: %w", err)
	}

	// The workspace path is derived from the id, so it has to exist first.
	id := uuid.NewString()
	workspace := filepath.Join(m.cfg.WorkspaceRoot, id)

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = req.RepoFullName
	}

	session, err := m.store.CreateSession(ctx, &store.Session{
		ID:           id,
		UserID:       user.ID,
		Title:        title,
		RepoFullName: req.RepoFullName,
		RepoCloneURL: req.RepoCloneURL,
		Branch:       req.Branch,
		ImageID:      image.ID,
		ImageRef:     image.ImageRef,
		WorkspaceDir: workspace,
		RepoDir:      filepath.Join(workspace, "repo"),
		AutoClaude:   req.AutoClaude,
		Status:       store.SessionStatusCreating,
	})
	if err != nil {
		return nil, err
	}

	go m.provision(session, token)
	return session, nil
}

// provision walks the session from an empty directory to a running container
// with tmux in it. It runs on its own context: a browser navigating away must
// not cancel a clone half way through.
func (m *Manager) provision(session *store.Session, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
	defer cancel()

	if err := m.provisionSteps(ctx, session, token); err != nil {
		m.log.Error("provision session", "session", session.ID, "err", err)
		m.cleanUpFailure(session)
		m.setStatus(session.ID, store.SessionStatusFailed, err.Error())
		return
	}
	m.setStatus(session.ID, store.SessionStatusRunning, "")
	m.log.Info("session running", "session", session.ID, "repo", session.RepoFullName)
}

func (m *Manager) provisionSteps(ctx context.Context, session *store.Session, token string) error {
	homeDir := filepath.Join(session.WorkspaceDir, "home")
	// The container runs as the host user with HOME here, and Claude Code wants
	// somewhere to keep its own state.
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o700); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}

	if err := seedClaudeConfig(homeDir); err != nil {
		return err
	}

	m.setStatus(session.ID, store.SessionStatusCloning, "")
	err := m.cloner.Clone(ctx, gitops.Options{
		CloneURL:  session.RepoCloneURL,
		Branch:    session.Branch,
		Dest:      session.RepoDir,
		Token:     token,
		UserName:  m.cfg.GitUserName,
		UserEmail: m.cfg.GitUserEmail,
	})
	if err != nil {
		return err
	}

	m.setStatus(session.ID, store.SessionStatusCreating, "")
	containerID, err := m.docker.CreateContainer(ctx, m.containerSpec(session, homeDir, token))
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
func (m *Manager) containerSpec(session *store.Session, homeDir, token string) dockerx.ContainerSpec {
	env := []string{
		"HOME=" + dockerx.AgentHome,
		"TERM=xterm-256color",
		"GITHUB_TOKEN=" + token,
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

// bootstrapScript prepares a freshly started container: git has to be told the
// bind mounted clone is safe to use (its owner may not match inside the
// container), and tmux has to be running before a terminal can attach.
//
// Whether Claude Code starts by itself is decided here, once, because it is the
// command the tmux session is created with — which is why flipping the switch
// on a running session only shows up the next time it starts.
func bootstrapScript(autoClaude bool) string {
	newSession := "tmux new-session -d -s " + TmuxSession + " -c " + dockerx.WorkspaceMount
	if autoClaude {
		newSession += " '" + claudeCommand + "'"
	}
	return "set -e\n" +
		"git config --global --add safe.directory " + dockerx.WorkspaceMount + "\n" +
		"tmux has-session -t " + TmuxSession + " 2>/dev/null || " + newSession
}

func (m *Manager) bootstrap(ctx context.Context, session *store.Session) error {
	containerID := session.ContainerID
	script := bootstrapScript(session.AutoClaude)
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
