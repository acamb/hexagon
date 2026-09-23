package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/andrea/hexagon/internal/dockerx"
)

// The browser login attaches to its own tmux session, with -A, so a page
// reload rejoins the login in progress instead of starting a second one on top
// of an OAuth flow that is half done.
const ClaudeLoginTmux = "login"

// ClaudeLoginCommand is what that tmux session runs. `claude auth login` goes
// straight to the sign-in flow — a URL to open and a wait for the callback —
// rather than opening the full interactive assistant and leaving the user to
// know that `/login` is the thing to type there. The shell fallback is not
// decoration: if the command a tmux session was created with exits, the
// session goes with it, and the user is left looking at a socket that closed.
const ClaudeLoginCommand = `claude auth login; exec "${SHELL:-sh}"`

// claudeLoginRole is the dockerx.LabelRole value for a browser login container.
const claudeLoginRole = "claude-login"

// claudeLoginContainerName is deterministic per caller: at most one login
// container exists for a given user or account at a time, and a leftover from
// a previous attempt is found by this name rather than tracked separately.
func claudeLoginContainerName(id string) string {
	return "hexagon-claude-login-" + id
}

// startClaudeLoginContainer is StartClaudeLogin and StartAccountClaudeLogin's
// shared shape: a throwaway home for tmux and the CLI's own state, and
// credentialsDir bind mounted read-write at $HOME/.claude, which is where
// `claude auth login` writes .credentials.json. What differs between the two
// callers is only which directory that is — the host's shared one, or one
// account's own — so two logins never overwrite each other's credential.
func (m *Manager) startClaudeLoginContainer(ctx context.Context, id, credentialsDir, imageRef string) (string, error) {
	name := claudeLoginContainerName(id)
	if err := m.docker.RemoveContainer(ctx, name, true); err != nil {
		return "", err
	}

	home := filepath.Join(m.cfg.ClaudeLoginDir, id)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("create login home: %w", err)
	}
	// Without $HOME/.claude.json the CLI opens its first-run onboarding, which
	// is not the login the user pressed the button for.
	if err := seedClaudeConfig(home); err != nil {
		return "", err
	}

	// Docker would otherwise create this itself, owned by root, on a directory
	// nobody has signed in to yet.
	if err := os.MkdirAll(credentialsDir, 0o700); err != nil {
		return "", fmt.Errorf("create claude credentials directory: %w", err)
	}

	containerID, err := m.docker.CreateContainer(ctx, dockerx.ContainerSpec{
		Name:  name,
		Image: imageRef,
		Cmd:   []string{"sleep", "infinity"},
		// No Anthropic variable of any kind. A container that already had one
		// would consider itself authenticated, and the login the user came here
		// for would be theatre performed on the credential they are replacing.
		Env:        []string{"HOME=" + dockerx.AgentHome, "TERM=xterm-256color", "LANG=C.UTF-8"},
		WorkingDir: dockerx.AgentHome,
		User:       m.cfg.ContainerUser,
		GroupAdd:   []string{dockerx.AgentGroup},
		// Not hexagon.managed: the reconciler matches managed containers against
		// session rows, and this one has none.
		Labels: map[string]string{dockerx.LabelRole: claudeLoginRole},
		Binds: []string{
			home + ":" + dockerx.AgentHome,
			// Read-write: this is the whole mechanism. Claude Code writes
			// .credentials.json here itself, into the file a session mounts.
			credentialsDir + ":" + dockerx.AgentHome + "/.claude",
		},
	})
	if err != nil {
		return "", err
	}
	if err := m.docker.StartContainer(ctx, containerID); err != nil {
		return "", err
	}
	return containerID, nil
}

// StartClaudeLogin prepares the container the machine-wide browser login runs
// in and returns its id. Any container left over from a previous attempt is
// removed first: it is throwaway, and a stale one would be attached to instead
// of a fresh one.
func (m *Manager) StartClaudeLogin(ctx context.Context, userID, imageRef string) (string, error) {
	if m.cfg.ClaudeCredentials == "" {
		return "", ErrClaudeLoginUnavailable
	}
	return m.startClaudeLoginContainer(ctx, userID, filepath.Dir(m.cfg.ClaudeCredentials), imageRef)
}

// StopClaudeLogin removes the login container. The credentials it wrote are on
// the host, not in it, so there is nothing to keep.
func (m *Manager) StopClaudeLogin(ctx context.Context, userID string) error {
	return m.docker.RemoveContainer(ctx, claudeLoginContainerName(userID), true)
}

// StartAccountClaudeLogin is StartClaudeLogin for one Claude account rather
// than for the machine: what gets bind mounted read-write is the account's own
// directory, so two accounts can be signed in to at once without either
// overwriting the other's credential. ErrClaudeLoginUnavailable does not apply
// here — it means "no credentials path is configured", and an account's
// directory is one Hexagon makes.
func (m *Manager) StartAccountClaudeLogin(ctx context.Context, accountID, imageRef string) (string, error) {
	return m.startClaudeLoginContainer(ctx, accountID, filepath.Join(m.cfg.ClaudeAccountsDir, accountID), imageRef)
}

// StopAccountClaudeLogin removes an account's login container.
func (m *Manager) StopAccountClaudeLogin(ctx context.Context, accountID string) error {
	return m.docker.RemoveContainer(ctx, claudeLoginContainerName(accountID), true)
}
