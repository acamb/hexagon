package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/store"
)

// ErrNoContainer reports a session that never got as far as having one.
var ErrNoContainer = errors.New("session has no container")

// Start brings a stopped session back up and makes sure tmux is running in it.
// A container that has been restarted has an empty tmux server, so the
// bootstrap runs again; the previous session's scrollback is gone either way.
func (m *Manager) Start(ctx context.Context, session *store.Session) error {
	if session.ContainerID == "" {
		return ErrNoContainer
	}
	if err := m.docker.StartContainer(ctx, session.ContainerID); err != nil {
		if errors.Is(err, dockerx.ErrContainerNotFound) {
			m.setStatus(session.ID, store.SessionStatusGone, "the container no longer exists")
		}
		return err
	}
	if err := m.bootstrap(ctx, session.ContainerID); err != nil {
		m.setStatus(session.ID, store.SessionStatusFailed, err.Error())
		return err
	}
	m.setStatus(session.ID, store.SessionStatusRunning, "")
	return nil
}

// Stop shuts the container down, keeping the workspace and the container itself.
func (m *Manager) Stop(ctx context.Context, session *store.Session) error {
	if session.ContainerID == "" {
		return ErrNoContainer
	}
	if err := m.docker.StopContainer(ctx, session.ContainerID, stopTimeout); err != nil {
		if errors.Is(err, dockerx.ErrContainerNotFound) {
			m.setStatus(session.ID, store.SessionStatusGone, "the container no longer exists")
		}
		return err
	}
	m.setStatus(session.ID, store.SessionStatusStopped, "")
	return nil
}

// Delete removes the session. The container always goes; the workspace only
// when asked, because it holds the repository clone and any work in it that has
// not been pushed.
func (m *Manager) Delete(ctx context.Context, session *store.Session, purge bool) error {
	if session.ContainerID != "" {
		if err := m.docker.RemoveContainer(ctx, session.ContainerID, true); err != nil {
			return err
		}
	}
	if err := m.store.DeleteSession(ctx, session.UserID, session.ID); err != nil {
		return err
	}
	if purge {
		if err := m.removeWorkspace(session.WorkspaceDir); err != nil {
			// The row is already gone, so this cannot fail the request; it is
			// left for the operator to clean up.
			m.log.Error("remove workspace", "session", session.ID, "dir", session.WorkspaceDir, "err", err)
		}
	}
	return nil
}

// removeWorkspace deletes a session's directory, after checking it really is
// one. The path comes from the database, and this is a recursive delete: if a
// row is ever corrupted, it must fail rather than take the rest of the disk.
func (m *Manager) removeWorkspace(dir string) error {
	if dir == "" {
		return nil
	}
	root, err := filepath.Abs(m.cfg.WorkspaceRoot)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if target == root || !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return fmt.Errorf("refusing to remove %s: it is not inside %s", target, root)
	}
	return os.RemoveAll(target)
}

// Refresh reconciles one session against the daemon. The database records what
// Hexagon last did; Docker knows what is actually true.
func (m *Manager) Refresh(ctx context.Context, session *store.Session) *store.Session {
	if session.ContainerID == "" || !isSettled(session.Status) {
		return session
	}

	state, err := m.docker.InspectContainer(ctx, session.ContainerID)
	switch {
	case errors.Is(err, dockerx.ErrContainerNotFound):
		m.applyStatus(session, store.SessionStatusGone, "the container no longer exists")
	case err != nil:
		m.log.Warn("inspect session container", "session", session.ID, "err", err)
	case state.Running:
		m.applyStatus(session, store.SessionStatusRunning, "")
	default:
		m.applyStatus(session, store.SessionStatusStopped, "")
	}
	return session
}

// RefreshAll reconciles a listing.
func (m *Manager) RefreshAll(ctx context.Context, sessions []*store.Session) []*store.Session {
	for _, session := range sessions {
		m.Refresh(ctx, session)
	}
	return sessions
}

// isSettled reports whether a session has finished being set up. While it is
// still provisioning, the manager owns its status and Docker must not overrule
// it: the container may exist before the session is ready to use.
func isSettled(status string) bool {
	switch status {
	case store.SessionStatusRunning, store.SessionStatusStopped, store.SessionStatusGone:
		return true
	}
	return false
}

func (m *Manager) applyStatus(session *store.Session, status, errMessage string) {
	if session.Status == status {
		return
	}
	session.Status = status
	session.Error = errMessage
	m.setStatus(session.ID, status, errMessage)
}

// Reconcile lines the database up with the daemon at startup: sessions whose
// container has disappeared are marked gone, the rest take the container's real
// state, and containers with no session left are reported but never removed.
func (m *Manager) Reconcile(ctx context.Context) error {
	containers, err := m.docker.ListManagedContainers(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]dockerx.ManagedContainer, len(containers))
	claimed := make(map[string]bool, len(containers))
	for _, c := range containers {
		byID[c.ID] = c
	}

	sessions, err := m.store.AllSessions(ctx)
	if err != nil {
		return err
	}

	var gone, adjusted int
	for _, session := range sessions {
		if session.ContainerID == "" || !isSettled(session.Status) {
			continue
		}
		container, ok := byID[session.ContainerID]
		if !ok {
			m.applyStatus(session, store.SessionStatusGone, "the container no longer exists")
			gone++
			continue
		}
		claimed[container.ID] = true

		status := store.SessionStatusStopped
		if container.Running {
			status = store.SessionStatusRunning
		}
		if session.Status != status {
			m.applyStatus(session, status, "")
			adjusted++
		}
	}

	for _, c := range containers {
		if !claimed[c.ID] {
			// Not removed on our own initiative: it may hold work that was
			// never pushed, and deleting it is the user's call.
			m.log.Warn("container with no session left", "container", c.ID, "session", c.SessionID)
		}
	}
	if gone > 0 || adjusted > 0 {
		m.log.Info("reconciled sessions", "gone", gone, "adjusted", adjusted, "containers", len(containers))
	}
	return nil
}
