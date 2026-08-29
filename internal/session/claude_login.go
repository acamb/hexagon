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

// claudeLoginContainerName is deterministic per user: at most one login
// container exists for a user at a time, and a leftover from a previous
// attempt is found by this name rather than tracked separately.
func claudeLoginContainerName(userID string) string {
	return "hexagon-claude-login-" + userID
}

// StartClaudeLogin prepares the container the browser login runs in and
// returns its id. Any container left over from a previous attempt is removed
// first: it is throwaway, and a stale one would be attached to instead of a
// fresh one.
func (m *Manager) StartClaudeLogin(ctx context.Context, userID, imageRef string) (string, error) {
	if m.cfg.ClaudeCredentials == "" {
		return "", ErrClaudeLoginUnavailable
	}

	name := claudeLoginContainerName(userID)
	if err := m.docker.RemoveContainer(ctx, name, true); err != nil {
		return "", err
	}

	home := filepath.Join(m.cfg.ClaudeLoginDir, userID)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("create login home: %w", err)
	}
	// Without $HOME/.claude.json the CLI opens its first-run onboarding, which
	// is not the login the user pressed the button for.
	if err := seedClaudeConfig(home); err != nil {
		return "", err
	}

	credentialsDir := filepath.Dir(m.cfg.ClaudeCredentials)
	// Docker would otherwise create this itself, owned by root, on a machine
	// where nobody has run claude yet.
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
		Env:        []string{"HOME=" + dockerx.AgentHome, "TERM=xterm-256color"},
		WorkingDir: dockerx.AgentHome,
		User:       m.cfg.ContainerUser,
		// Not hexagon.managed: the reconciler matches managed containers against
		// session rows, and this one has none.
		Labels: map[string]string{dockerx.LabelRole: claudeLoginRole},
		Binds: []string{
			home + ":" + dockerx.AgentHome,
			// Read-write, and the host's own directory: this is the whole
			// mechanism. Claude Code writes .credentials.json here itself, into
			// the file every session already mounts.
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

// StopClaudeLogin removes the login container. The credentials it wrote are on
// the host, not in it, so there is nothing to keep.
func (m *Manager) StopClaudeLogin(ctx context.Context, userID string) error {
	return m.docker.RemoveContainer(ctx, claudeLoginContainerName(userID), true)
}
