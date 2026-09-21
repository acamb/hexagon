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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrea/hexagon/internal/claudex"
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
	// ErrClaudeLoginUnavailable reports that no credentials path is configured,
	// so a browser login would have nowhere to write.
	ErrClaudeLoginUnavailable = errors.New("no claude credentials path is configured")
	// ErrComposeUnavailable reports a session from an advanced image on a
	// server with no `docker compose` to run the project with.
	ErrComposeUnavailable = errors.New("docker compose is not available")
	// ErrInvalidPorts reports a create request whose published ports cannot be
	// honoured.
	ErrInvalidPorts = errors.New("invalid published ports")
	// ErrClaudeAccountNotFound reports a request naming a claude account that
	// does not exist, or does not belong to the caller — the two look the same
	// on purpose.
	ErrClaudeAccountNotFound = errors.New("claude account not found")
)

// loopback is the address a published port binds when the session named none,
// and the only one code-server is ever published on.
const loopback = "127.0.0.1"

// publishAddress turns a session's stored address into one Docker can be given.
//
// The empty string is the trap this exists for. It is what a session that named
// no address holds, and it reads as "unset" everywhere in Hexagon — but Docker
// takes an empty HostIP to mean every interface, so passing it through would
// turn the closed default into the open one, silently, for every session that
// never asked for anything.
func publishAddress(stored string) string {
	if stored == "" {
		return loopback
	}
	return stored
}

// maxSessionPorts bounds what one session may publish. Each one is a listening
// socket on the host's loopback interface with nothing in front of it, so the
// number of them is worth a limit even though the cost of any single one is
// small.
const maxSessionPorts = 10

// CredentialSource hands over the git credentials for one of a user's connected
// accounts, and opens the sealed secret of a pasted Claude account. It is an
// interface so the orchestrator never sees the cipher, and so tests do not need
// one.
type CredentialSource interface {
	GitCredentials(ctx context.Context, userID string, kind provider.Kind) (provider.GitAuth, error)
	// ClaudeSecret opens the sealed secret of an api_key or oauth_token
	// account. It is never asked about a login account: that credential is a
	// directory the manager itself owns, not a secret to unseal.
	ClaudeSecret(ctx context.Context, account *store.ClaudeAccount) (claudex.Credential, error)
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
	// ClaudeLoginDir holds the throwaway HOME of the container the browser login
	// runs in, one directory per user or per account.
	ClaudeLoginDir string
	// ClaudeAccountsDir holds one directory per Claude account created from the
	// UI, each with the .credentials.json a login on it writes.
	ClaudeAccountsDir string
	// GitUserName and GitUserEmail are the author sessions commit as, and only
	// the value this process started with: the settings page can replace them
	// while it runs, so everything after NewManager reads GitIdentity instead.
	GitUserName  string
	GitUserEmail string
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
	// compose runs the projects of sessions made from an advanced image. Nil is
	// legal too, and means such an image cannot start a session here.
	compose Compose
	cfg     Config
	// gitMu guards the git identity below. It is the one part of cfg that moves
	// while the server runs: the settings page applies it without a restart,
	// and a session provisioned afterwards has to commit as the new author
	// rather than as the one this process was started with. Everything else in
	// cfg stays the startup snapshot, which is what the page reports as needing
	// a restart.
	gitMu    sync.RWMutex
	gitName  string
	gitEmail string
	log      *slog.Logger
}

// GitIdentity is the author sessions are provisioned to commit as, as it stands
// now.
func (m *Manager) GitIdentity() (name, email string) {
	m.gitMu.RLock()
	defer m.gitMu.RUnlock()
	return m.gitName, m.gitEmail
}

// SetGitIdentity replaces it, which is how a save on the settings page reaches
// the next session without a restart.
//
// Sessions that already exist keep the identity they were provisioned with:
// it was written into the clone's local git configuration and into the
// container's environment when they were created, and a container keeps the
// environment it was created with.
func (m *Manager) SetGitIdentity(name, email string) {
	m.gitMu.Lock()
	defer m.gitMu.Unlock()
	m.gitName, m.gitEmail = name, email
}

// NewManager wires the orchestrator.
func NewManager(st *store.Store, docker dockerx.API, cloner Cloner, credentials CredentialSource, vscode VSCodeSource, compose Compose, cfg Config, log *slog.Logger) *Manager {
	return &Manager{store: st, docker: docker, cloner: cloner, credentials: credentials,
		vscode: vscode, compose: compose, cfg: cfg, log: log,
		gitName: cfg.GitUserName, gitEmail: cfg.GitUserEmail}
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
	// CloneDepth truncates the clone's history to that many commits. Zero
	// clones all of it. It only means anything with a repository, and only
	// once: the clone happens during provisioning and is never redone.
	CloneDepth int
	ImageID    string
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
	// Ports are container ports to publish, with the host side left to Docker.
	// Decided here for the same reason as VSCode above, and never edited
	// afterwards.
	Ports []int
	// PortAddress is the host interface they bind. Empty is loopback, which is
	// the answer that exposes nothing beyond this machine.
	PortAddress string
	// ClaudeAccountID names which Claude account the container authenticates
	// with. Empty resolves to the user's default account, or — when they have
	// configured none — to the server's own configuration.
	ClaudeAccountID string
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
	compose := image.SourceType == store.ImageSourceCompose
	if compose && m.compose == nil {
		return nil, ErrComposeUnavailable
	}
	if err := checkPorts(req.Ports, req.PortAddress, req.VSCode); err != nil {
		return nil, err
	}

	// image.ImageRef is a tag, and handleRebuildImage reassigns it in place when
	// the image's Dockerfile changes. Pinning the session to the content behind
	// it today is what keeps a later rebuild of this session's own container —
	// from a port or Claude account change — running what was chosen here
	// rather than whatever the tag resolves to by then.
	digest, err := m.docker.InspectImage(ctx, image.ImageRef)
	if err != nil {
		return nil, fmt.Errorf("inspect image %s: %w", image.ImageRef, err)
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
	claude, err := m.resolveClaudeAccount(ctx, user.ID, req.ClaudeAccountID)
	if err != nil {
		return nil, err
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
		ID:              id,
		UserID:          user.ID,
		Title:           title,
		Provider:        string(req.Provider),
		RepoFullName:    req.RepoFullName,
		RepoCloneURL:    req.RepoCloneURL,
		Branch:          req.Branch,
		CloneDepth:      req.CloneDepth,
		ImageID:         image.ID,
		ImageRef:        image.ImageRef,
		ImageDigest:     digest,
		WorkspaceDir:    workspace,
		RepoDir:         filepath.Join(workspace, "repo"),
		AutoClaude:      req.AutoClaude,
		PropagateToken:  req.PropagateToken,
		VSCode:          req.VSCode,
		Ports:           req.Ports,
		PortAddress:     req.PortAddress,
		Compose:         compose,
		ClaudeAccountID: req.ClaudeAccountID,
		Status:          store.SessionStatusCreating,
	})
	if err != nil {
		return nil, err
	}

	go m.provision(session, image.Compose, credentials, claude)
	return session, nil
}

// checkPorts refuses a set of published ports before anything is provisioned.
//
// The collision with the VS Code port is the one worth naming: without this
// check the editor's own binding is quietly taken by something else and the
// button stops working for a reason nobody could find.
func checkPorts(ports []int, address string, vscode bool) error {
	// An address Docker would reject leaves a session that fails to start with
	// a daemon error nobody can act on. A name is refused too: the binding is an
	// interface, and resolving one here would only move the surprise.
	if address != "" && net.ParseIP(address) == nil {
		return fmt.Errorf("%w: %q is not an address to publish on", ErrInvalidPorts, address)
	}
	if len(ports) > maxSessionPorts {
		return fmt.Errorf("%w: at most %d ports", ErrInvalidPorts, maxSessionPorts)
	}
	seen := make(map[int]bool, len(ports))
	for _, p := range ports {
		switch {
		case p < 1 || p > 65535:
			return fmt.Errorf("%w: %d is not a port", ErrInvalidPorts, p)
		case seen[p]:
			return fmt.Errorf("%w: %d is listed twice", ErrInvalidPorts, p)
		case vscode && p == dockerx.VSCodePort:
			return fmt.Errorf("%w: %d is the port the VS Code integration publishes",
				ErrInvalidPorts, dockerx.VSCodePort)
		}
		seen[p] = true
	}
	return nil
}

// provision walks the session from an empty directory to a running container
// with tmux in it. It runs on its own context: a browser navigating away must
// not cancel a clone half way through.
func (m *Manager) provision(session *store.Session, composeFile string, credentials provider.GitAuth, claude claudex.Credential) {
	ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
	defer cancel()

	if err := m.provisionSteps(ctx, session, composeFile, credentials, claude); err != nil {
		m.log.Error("provision session", "session", session.ID, "err", err)
		m.cleanUpFailure(session)
		m.setStatus(session.ID, store.SessionStatusFailed, err.Error())
		return
	}
	m.setStatus(session.ID, store.SessionStatusRunning, "")
	m.log.Info("session running", "session", session.ID, "repo", session.RepoFullName)
}

func (m *Manager) provisionSteps(ctx context.Context, session *store.Session, composeFile string, credentials provider.GitAuth, claude claudex.Credential) error {
	homeDir := sessionHome(session)
	// The container runs as the host user with HOME here, and Claude Code wants
	// somewhere to keep its own state.
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o700); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}

	if err := seedClaudeConfig(homeDir); err != nil {
		return err
	}

	if session.RepoCloneURL != "" {
		gitName, gitEmail := m.GitIdentity()
		m.setStatus(session.ID, store.SessionStatusCloning, "")
		err := m.cloner.Clone(ctx, gitops.Options{
			CloneURL:  session.RepoCloneURL,
			Branch:    session.Branch,
			Depth:     session.CloneDepth,
			Dest:      session.RepoDir,
			Username:  credentials.Username,
			Token:     credentials.Secret,
			UserName:  gitName,
			UserEmail: gitEmail,
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
	spec := m.containerSpec(session, homeDir, vscodeDir, credentials, claude)

	if session.Compose {
		if err := m.createProject(ctx, session, composeFile, spec); err != nil {
			return err
		}
		return m.bootstrap(ctx, session)
	}

	containerID, err := m.docker.CreateContainer(ctx, spec)
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

// createProject brings up the compose project behind a session and records the
// agent container as the session's own.
//
// The user's file is validated again here, even though the image was refused at
// registration unless it passed: what is checked and what is run must be the
// same text, and between the two there is a database and a restart.
func (m *Manager) createProject(ctx context.Context, session *store.Session, composeFile string, spec dockerx.ContainerSpec) error {
	services, err := m.compose.Validate(ctx, composeFile)
	if err != nil {
		return err
	}
	project := m.composeProject(session)
	if err := writeComposeFiles(project.Dir, composeFile, spec, services); err != nil {
		return err
	}

	m.setStatus(session.ID, store.SessionStatusStarting, "")
	if err := m.compose.Up(ctx, project); err != nil {
		return err
	}

	// The container id is learned rather than returned: compose names the
	// container, and Docker takes that name wherever it takes an id.
	state, err := m.docker.InspectContainer(ctx, spec.Name)
	if err != nil {
		return fmt.Errorf("find the agent container of the project: %w", err)
	}
	session.ContainerID = state.ID
	return m.store.SetSessionContainer(ctx, session.ID, state.ID)
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

// sessionHome is the directory bind mounted as the container's HOME. It is
// derived from the workspace rather than stored, and it is a function because
// two paths need it: provisioning, which creates it, and rebuilding the
// container of a session that already has one.
func sessionHome(session *store.Session) string {
	return filepath.Join(session.WorkspaceDir, "home")
}

// rebuildSpec assembles the container specification of a session that already
// exists, gathering again everything provisioning gathered the first time.
//
// The credentials are re-read rather than remembered, which is the part worth
// knowing: a rebuilt container carries whatever the user's accounts hold now,
// not what they held when the session was created. That is the only honest
// answer — the sealed values are the only ones there are — and it is why a
// disconnected account fails the rebuild instead of quietly producing a
// container that cannot authenticate.
func (m *Manager) rebuildSpec(ctx context.Context, session *store.Session) (dockerx.ContainerSpec, error) {
	var credentials provider.GitAuth
	if session.Provider != "" {
		var err error
		credentials, err = m.credentials.GitCredentials(ctx, session.UserID, provider.Kind(session.Provider))
		if err != nil {
			return dockerx.ContainerSpec{}, fmt.Errorf("read the stored %s credentials: %w", session.Provider, err)
		}
	}
	claude, err := m.resolveClaudeAccount(ctx, session.UserID, session.ClaudeAccountID)
	if err != nil {
		return dockerx.ContainerSpec{}, fmt.Errorf("read the stored claude credential: %w", err)
	}

	var vscodeDir string
	if session.VSCode {
		if m.vscode == nil {
			return dockerx.ContainerSpec{}, ErrVSCodeUnavailable
		}
		dir, err := m.vscode.Ensure(ctx)
		if err != nil {
			return dockerx.ContainerSpec{}, fmt.Errorf("prepare vscode: %w", err)
		}
		vscodeDir = dir
	}
	return m.containerSpec(session, sessionHome(session), vscodeDir, credentials, claude), nil
}

// resolveClaudeAccount decides what a session's container authenticates Claude
// Code with: the account named in the request, else the user's default, else
// the zero value — in which case the caller falls back to the server's own
// configuration, which is today's behaviour left exactly as it was.
//
// It is resolved here, and read again on every rebuild, rather than
// remembered: a rebuilt container carries whatever the user's accounts hold
// now, not what they held when the session was created. That is the only
// honest answer — the sealed values are the only ones there are — and it is
// why an account that has been signed out of since fails the rebuild instead
// of quietly producing a container that cannot authenticate.
func (m *Manager) resolveClaudeAccount(ctx context.Context, userID, accountID string) (claudex.Credential, error) {
	var (
		account *store.ClaudeAccount
		err     error
	)
	if accountID != "" {
		account, err = m.store.ClaudeAccountByID(ctx, userID, accountID)
		if errors.Is(err, store.ErrNotFound) {
			return claudex.Credential{}, ErrClaudeAccountNotFound
		}
	} else {
		account, err = m.store.DefaultClaudeAccount(ctx, userID)
		if errors.Is(err, store.ErrNotFound) {
			return claudex.Credential{}, nil
		}
	}
	if err != nil {
		return claudex.Credential{}, err
	}
	if account.Kind == store.ClaudeAccountKindLogin {
		return claudex.Credential{Kind: claudex.KindLogin, File: m.claudeAccountCredentialsFile(account.ID)}, nil
	}
	return m.credentials.ClaudeSecret(ctx, account)
}

// claudeAccountCredentialsFile is where a login account's browser login writes
// its credential, and where a session mounts it from.
func (m *Manager) claudeAccountCredentialsFile(accountID string) string {
	return filepath.Join(m.cfg.ClaudeAccountsDir, accountID, ".credentials.json")
}

// ClaudeCredential resolves what a session naming no account would
// authenticate with, all the way down to the server's own configuration. It is
// exported for the Images page, which authenticates the source editor the same
// way a session does — see the comment on containerSpec's credential branch for
// why the two must agree.
func (m *Manager) ClaudeCredential(ctx context.Context, userID string) (claudex.Credential, error) {
	cred, err := m.resolveClaudeAccount(ctx, userID, "")
	if err != nil || cred.Secret != "" || cred.File != "" {
		return cred, err
	}
	if m.cfg.AnthropicAPIKey != "" {
		return claudex.Credential{Kind: claudex.KindAPIKey, Secret: m.cfg.AnthropicAPIKey}, nil
	}
	if m.cfg.ClaudeCredentials != "" {
		if _, err := os.Stat(m.cfg.ClaudeCredentials); err == nil {
			return claudex.Credential{Kind: claudex.KindLogin, File: m.cfg.ClaudeCredentials}, nil
		}
	}
	return claudex.Credential{}, nil
}

// sessionImage is the image reference a session's container is created or
// rebuilt from: ImageDigest when it was captured at creation, so a rebuild
// stays on the content the session was actually made with even if the image's
// tag has since been reassigned by handleRebuildImage. A session created
// before that column existed has none, and falls back to ImageRef exactly as
// every session did before — the content that tag pointed to when this session
// was created may already be gone, so there is nothing more honest to do.
func sessionImage(session *store.Session) string {
	if session.ImageDigest != "" {
		return session.ImageDigest
	}
	return session.ImageRef
}

// containerSpec is the whole contract between Hexagon and a session container.
func (m *Manager) containerSpec(session *store.Session, homeDir, vscodeDir string, credentials provider.GitAuth, claude claudex.Credential) dockerx.ContainerSpec {
	env := []string{
		"HOME=" + dockerx.AgentHome,
		"TERM=xterm-256color",
		// C.UTF-8 rather than en_US.UTF-8: glibc provides it without the
		// `locales` package, so it works in an image Hexagon did not build.
		// Without it the container runs in the C locale, and tmux decides its
		// client cannot show UTF-8 — it then draws an underscore in place of
		// every accent and every box-drawing rule.
		"LANG=C.UTF-8",
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
	gitName, gitEmail := m.GitIdentity()
	if gitName != "" {
		env = append(env, "GIT_AUTHOR_NAME="+gitName, "GIT_COMMITTER_NAME="+gitName)
	}
	if gitEmail != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+gitEmail, "GIT_COMMITTER_EMAIL="+gitEmail)
	}
	binds := []string{
		session.RepoDir + ":" + dockerx.WorkspaceMount,
		homeDir + ":" + dockerx.AgentHome,
	}
	// An account resolved for this session outranks the server's own
	// configuration: it is the one the user can see, change and be told about.
	// With one in play the legacy mount below is not added — a session carries
	// an environment credential or a file and never both, which is what retires
	// the shadowing the environment used to win silently over a mount.
	switch {
	case claude.File != "":
		// Read-only: the container gets to use the credentials, not to change them.
		binds = append(binds, claude.File+":"+dockerx.AgentHome+"/.claude/.credentials.json:ro")
	case claude.Secret != "":
		env = append(env, claude.Env()...)
	default:
		if m.cfg.AnthropicAPIKey != "" {
			env = append(env, "ANTHROPIC_API_KEY="+m.cfg.AnthropicAPIKey)
		}
		if path := m.cfg.ClaudeCredentials; path != "" {
			if _, err := os.Stat(path); err == nil {
				binds = append(binds, path+":"+dockerx.AgentHome+"/.claude/.credentials.json:ro")
			} else {
				m.log.Warn("claude credentials not found, sessions will need their own login",
					"path", path, "err", err)
			}
		}
	}
	ports := make([]dockerx.PortPublication, 0, len(session.Ports)+1)
	for _, p := range session.Ports {
		ports = append(ports, dockerx.PortPublication{Container: p, Address: publishAddress(session.PortAddress)})
	}
	if vscodeDir != "" {
		// Read-only: the container gets to run the editor, not to modify it.
		// Everything code-server writes goes under $HOME, which is already a
		// bind mount of the session's own directory, so its settings and
		// extensions survive a restart.
		binds = append(binds, vscodeDir+":"+dockerx.VSCodeMount+":ro")
		// Loopback whatever the session chose for its own ports. code-server
		// asks nobody for anything — the session cookie in front of the proxy
		// is the only thing that does — so a copy of it on a public interface
		// is an unauthenticated shell in the workspace.
		ports = append(ports, dockerx.PortPublication{Container: dockerx.VSCodePort, Address: loopback})
	}

	return dockerx.ContainerSpec{
		Name:  containerNamePrefix + session.ID,
		Image: sessionImage(session),
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

// cleanUpFailure removes what a failed setup left behind. The workspace is
// deliberately kept: it is the evidence for what went wrong.
//
// A compose session is torn down whether or not it got as far as a container
// id, because `up` can fail with half the project standing; its volumes are
// kept, for the same reason the workspace is.
func (m *Manager) cleanUpFailure(session *store.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if session.Compose {
		if err := m.compose.Down(ctx, m.composeProject(session), false); err != nil {
			m.log.Warn("remove the project of a failed session", "session", session.ID, "err", err)
		}
		return
	}
	if session.ContainerID == "" {
		return
	}
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
