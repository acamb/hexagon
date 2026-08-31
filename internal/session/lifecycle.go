package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/store"
)

// ErrNoContainer reports a session that never got as far as having one.
var ErrNoContainer = errors.New("session has no container")

// ErrSessionNotStopped reports a change that can only be made to a session that
// is down, asked of one that is not.
var ErrSessionNotStopped = errors.New("session is not stopped")

// VSCodeEndpoint returns the base URL of the code-server this session's
// container publishes. It is looked up rather than stored: Docker picks a new
// host port every time the container starts.
func (m *Manager) VSCodeEndpoint(ctx context.Context, s *store.Session) (string, error) {
	if !s.VSCode {
		return "", ErrVSCodeDisabled
	}
	if s.ContainerID == "" {
		return "", ErrNoContainer
	}
	state, err := m.docker.InspectContainer(ctx, s.ContainerID)
	if err != nil {
		return "", err
	}
	if !state.Running {
		return "", ErrVSCodeNotReady
	}
	port, ok := state.Ports[dockerx.VSCodePort]
	if !ok {
		return "", ErrVSCodeNotReady
	}
	return "http://127.0.0.1:" + strconv.Itoa(port), nil
}

// PublishedPorts reports the host binding of each port a session publishes,
// keyed by the container port. It is looked up and never stored, the way
// VSCodeEndpoint is: Docker picks a new host port every time the container
// starts, so a stored one would be a lie as soon as the session was restarted.
//
// A session that is not running has no bindings to report, and that is the
// truth rather than a gap: there is nothing listening.
func (m *Manager) PublishedPorts(ctx context.Context, session *store.Session) map[int]int {
	if len(session.Ports) == 0 || session.ContainerID == "" {
		return nil
	}
	state, err := m.docker.InspectContainer(ctx, session.ContainerID)
	if err != nil {
		if !errors.Is(err, dockerx.ErrContainerNotFound) {
			m.log.Warn("look up published ports", "session", session.ID, "err", err)
		}
		return nil
	}
	if !state.Running {
		return nil
	}
	out := make(map[int]int, len(session.Ports))
	for _, container := range session.Ports {
		if host, ok := state.Ports[container]; ok {
			out[container] = host
		}
	}
	return out
}

// SetPorts changes what a stopped session publishes, and where.
//
// It is a lifecycle operation and not a settings change, which is the whole
// reason it lives here. A container keeps the port bindings it was created
// with, so the only way to publish something else is to build another container
// from the same image over the same workspace — which is why the session has to
// be stopped, and why what was installed inside the old container by hand does
// not survive. The workspace and the home directory do: they are bind mounts on
// the host, and they are where a session's work actually is.
func (m *Manager) SetPorts(ctx context.Context, session *store.Session, ports []int, address string) error {
	if session.Status != store.SessionStatusStopped {
		return ErrSessionNotStopped
	}
	if session.ContainerID == "" {
		return ErrNoContainer
	}
	if err := checkPorts(ports, address, session.VSCode); err != nil {
		return err
	}
	// Nothing to rebuild for a request that asks for what is already there, and
	// rebuilding anyway would throw away a container for no reason at all. The
	// addresses are compared as Docker would be given them: a session that named
	// none holds the empty string, and the browser sends back the 127.0.0.1 the
	// API answered with, which is the same binding written differently.
	if samePorts(session.Ports, ports) && publishAddress(session.PortAddress) == publishAddress(address) {
		return nil
	}

	previousPorts, previousAddress := session.Ports, session.PortAddress
	session.Ports, session.PortAddress = ports, address
	spec, err := m.rebuildSpec(ctx, session)
	if err != nil {
		session.Ports, session.PortAddress = previousPorts, previousAddress
		return err
	}

	// The row is written before the container is built, deliberately. If the
	// rebuild then fails, the session claims a binding its container does not
	// have, and the UI warns about an address nothing is published on; the other
	// order would show a session bound to loopback while its container answered
	// the network.
	if err := m.store.SetSessionPorts(ctx, session.UserID, session.ID, ports, address); err != nil {
		session.Ports, session.PortAddress = previousPorts, previousAddress
		return err
	}
	return m.rebuildContainer(ctx, session, spec)
}

// samePorts reports whether two port lists ask for the same thing. Order is
// part of the answer: it is the order the user typed, and reordering it is a
// change to the row rather than to the container.
func samePorts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rebuildContainer replaces a stopped session's container with one built to
// spec, leaving it stopped.
//
// The new container takes the name the old one had, because everything that
// finds a session container — the terminal, the bootstrap, the VS Code proxy —
// finds it by the id recorded here, and compose finds it by that name.
func (m *Manager) rebuildContainer(ctx context.Context, session *store.Session, spec dockerx.ContainerSpec) error {
	if session.Compose {
		return m.rebuildProject(ctx, session, spec)
	}
	// The old container holds the name, so it has to go before the new one can
	// take it. One that has already disappeared is not a failure: what this has
	// to leave behind is a container matching the spec, and there being none is
	// the state it starts from.
	if err := m.docker.RemoveContainer(ctx, session.ContainerID, true); err != nil &&
		!errors.Is(err, dockerx.ErrContainerNotFound) {
		return fmt.Errorf("remove the container to rebuild it: %w", err)
	}

	containerID, err := m.docker.CreateContainer(ctx, spec)
	if err != nil {
		// The old container is gone and the new one was not made: the session
		// has nothing behind it, which is exactly what `gone` says. Leaving the
		// row pointing at a container id that no longer exists would make every
		// later action fail with a message about a container instead.
		m.applyStatus(session, store.SessionStatusGone,
			"the container was removed to change its published ports and could not be created again: "+err.Error())
		return fmt.Errorf("create the rebuilt container: %w", err)
	}
	session.ContainerID = containerID
	return m.store.SetSessionContainer(ctx, session.ID, containerID)
}

// rebuildProject does the same for a compose session, by rewriting Hexagon's
// half of the project and letting compose recreate what changed.
//
// The user's half is read back from the workspace rather than from the image it
// came from: that file is what this project was validated and brought up with,
// and the image behind it may have been edited or deleted since. It is checked
// again all the same, for the reason createProject gives — what is checked and
// what is run must be the same text — and because the check is what names the
// services the overlay has to label and depend on.
func (m *Manager) rebuildProject(ctx context.Context, session *store.Session, spec dockerx.ContainerSpec) error {
	if m.compose == nil {
		return ErrComposeUnavailable
	}
	project := m.composeProject(session)
	userFile, err := os.ReadFile(filepath.Join(project.Dir, "user.yaml"))
	if err != nil {
		return fmt.Errorf("read the compose file of the session: %w", err)
	}
	services, err := m.compose.Validate(ctx, string(userFile))
	if err != nil {
		return err
	}
	if err := writeComposeFiles(project.Dir, string(userFile), spec, services); err != nil {
		return err
	}

	// `create` and not `up`: compose recreates the agent container because its
	// definition changed, and the session stays down until somebody starts it.
	if err := m.compose.Create(ctx, project); err != nil {
		return err
	}
	// The container is a new one with a new id, and compose named it; the name
	// is what finds it, exactly as at provisioning.
	state, err := m.docker.InspectContainer(ctx, spec.Name)
	if err != nil {
		return fmt.Errorf("find the agent container of the project: %w", err)
	}
	session.ContainerID = state.ID
	return m.store.SetSessionContainer(ctx, session.ID, state.ID)
}

// Start brings a stopped session back up and makes sure tmux is running in it.
// A container that has been restarted has an empty tmux server, so the
// bootstrap runs again; the previous session's scrollback is gone either way.
func (m *Manager) Start(ctx context.Context, session *store.Session) error {
	if session.ContainerID == "" {
		return ErrNoContainer
	}
	if err := m.startContainers(ctx, session); err != nil {
		if errors.Is(err, dockerx.ErrContainerNotFound) {
			m.setStatus(session.ID, store.SessionStatusGone, "the container no longer exists")
		}
		return err
	}
	if err := m.bootstrap(ctx, session); err != nil {
		m.setStatus(session.ID, store.SessionStatusFailed, err.Error())
		return err
	}
	m.setStatus(session.ID, store.SessionStatusRunning, "")
	return nil
}

// startContainers brings a session's containers back, which for a compose
// session is the whole project and not only the agent: the services it was
// created beside are what it was created for.
func (m *Manager) startContainers(ctx context.Context, session *store.Session) error {
	if session.Compose {
		return m.compose.Start(ctx, m.composeProject(session))
	}
	return m.docker.StartContainer(ctx, session.ContainerID)
}

// Stop shuts the container down, keeping the workspace and the container itself.
func (m *Manager) Stop(ctx context.Context, session *store.Session) error {
	if session.ContainerID == "" {
		return ErrNoContainer
	}
	if err := m.stopContainers(ctx, session); err != nil {
		if errors.Is(err, dockerx.ErrContainerNotFound) {
			m.setStatus(session.ID, store.SessionStatusGone, "the container no longer exists")
		}
		return err
	}
	m.setStatus(session.ID, store.SessionStatusStopped, "")
	return nil
}

func (m *Manager) stopContainers(ctx context.Context, session *store.Session) error {
	if session.Compose {
		return m.compose.Stop(ctx, m.composeProject(session))
	}
	return m.docker.StopContainer(ctx, session.ContainerID, stopTimeout)
}

// Delete removes the session. The container always goes; the workspace only
// when asked, because it holds the repository clone and any work in it that has
// not been pushed.
//
// A compose project's named volumes follow the workspace rather than the
// container: purge already means "the work too", and what a database wrote is
// the work, not the machinery.
func (m *Manager) Delete(ctx context.Context, session *store.Session, purge bool) error {
	switch {
	case session.Compose:
		if err := m.compose.Down(ctx, m.composeProject(session), purge); err != nil {
			return err
		}
	case session.ContainerID != "":
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

	known := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		known[session.ID] = true
	}
	for _, c := range containers {
		switch {
		case claimed[c.ID]:
		case c.Role == serviceRole && known[c.SessionID]:
			// A service of a compose project: it belongs to a session that is
			// still here, it is simply not the container the row points at.
			// This is the problem dockerx.LabelRole exists for.
		default:
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
