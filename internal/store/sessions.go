package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Session statuses. The persisted value is a view of what the orchestrator last
// did; the container's real state is re-read from Docker and reconciled.
const (
	SessionStatusCreating = "creating"
	SessionStatusCloning  = "cloning"
	SessionStatusStarting = "starting"
	SessionStatusRunning  = "running"
	SessionStatusStopped  = "stopped"
	SessionStatusFailed   = "failed"
	// SessionStatusGone means the container behind the session no longer exists.
	SessionStatusGone = "gone"
)

// Session is one Claude Code session: a repository clone on the host, bind
// mounted into a container running tmux.
type Session struct {
	ID     string
	UserID string
	Title  string
	// Provider is which connected account the repository came from, as
	// internal/provider names it.
	Provider     string
	RepoFullName string
	RepoCloneURL string
	Branch       string
	ImageID      string
	ImageRef     string
	// WorkspaceDir is the per-session directory on the host; RepoDir is the
	// clone inside it, the path bind mounted at /workspace.
	WorkspaceDir string
	RepoDir      string
	ContainerID  string
	Status       string
	Error        string
	// AutoClaude starts Claude Code inside the session's tmux instead of
	// leaving a bare shell. It is read when the tmux session is created, so a
	// change takes effect the next time the container starts.
	AutoClaude bool
	// PropagateToken hands the provider credentials to the container. The clone
	// on the host uses them either way; this is only about what runs inside.
	// It is read when the container is created, and a container's environment
	// cannot be changed afterwards, so it is decided once and never edited.
	PropagateToken bool
	// VSCode is whether this session's container publishes code-server and has
	// the release bind mounted. Set at creation only: a container keeps the
	// mounts and the port bindings it was created with, so there is no setter.
	VSCode bool
	// Ports are the container ports this session publishes. A container keeps
	// the bindings it was created with, so changing these means building
	// another container — which is why they are editable while the session is
	// stopped and not while it runs. The host port Docker picked for each is
	// never stored: Docker picks a new one every time the container starts.
	Ports []int
	// PortAddress is the host interface those ports are bound to. Empty means
	// loopback, which is what every session created before the column had.
	PortAddress string
	// Compose is whether this session is a compose project rather than a single
	// container. It comes from the image it was created from and, like the two
	// above, cannot change afterwards.
	Compose bool
	// ClaudeAccountID names which Claude account the container authenticates
	// with. Empty means "whatever the server resolves" — the user's default
	// account, or the server's own configuration — which is what a session
	// created before this column existed, and one created naming no account,
	// both mean. Editable while the session is stopped; see
	// session.Manager.SetClaudeAccount.
	ClaudeAccountID string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// formatPorts and parsePorts move Session.Ports across the one TEXT column that
// holds it. A column per port is not an option and a second table would be one
// row per integer; a comma-separated list is what the value is.
func formatPorts(ports []int) string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, strconv.Itoa(p))
	}
	return strings.Join(out, ",")
}

func parsePorts(raw string) ([]int, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ports := make([]int, 0, len(parts))
	for _, part := range parts {
		p, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("parse ports %q: %w", raw, err)
		}
		ports = append(ports, p)
	}
	return ports, nil
}

const sessionColumns = `id, user_id, title, provider, repo_full_name, repo_clone_url, branch, image_id, image_ref,
	workspace_dir, repo_dir, container_id, status, error, auto_claude, propagate_token, vscode, ports, port_address,
	compose, claude_account_id, created_at, updated_at`

// SessionByID returns one of the user's sessions, or ErrNotFound.
func (s *Store) SessionByID(ctx context.Context, userID, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+sessionColumns+` FROM sessions WHERE id = ? AND user_id = ?`, id, userID)

	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return session, err
}

func scanSession(row scanner) (*Session, error) {
	var (
		session              Session
		ports                string
		createdAt, updatedAt string
	)
	err := row.Scan(&session.ID, &session.UserID, &session.Title, &session.Provider, &session.RepoFullName,
		&session.RepoCloneURL, &session.Branch, &session.ImageID, &session.ImageRef,
		&session.WorkspaceDir, &session.RepoDir, &session.ContainerID, &session.Status,
		&session.Error, &session.AutoClaude, &session.PropagateToken, &session.VSCode, &ports,
		&session.PortAddress, &session.Compose, &session.ClaudeAccountID, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if session.Ports, err = parsePorts(ports); err != nil {
		return nil, err
	}
	if session.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if session.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	return &session, nil
}

// CreateSession inserts a new session, assigning it an id when it has none.
func (s *Store) CreateSession(ctx context.Context, session *Session) (*Session, error) {
	if session.ID == "" {
		session.ID = uuid.NewString()
	}
	now := time.Now()
	session.CreatedAt, session.UpdatedAt = now, now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (`+sessionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.UserID, session.Title, session.Provider, session.RepoFullName, session.RepoCloneURL,
		session.Branch, session.ImageID, session.ImageRef, session.WorkspaceDir, session.RepoDir,
		session.ContainerID, session.Status, session.Error, session.AutoClaude, session.PropagateToken, session.VSCode,
		formatPorts(session.Ports), session.PortAddress, session.Compose, session.ClaudeAccountID,
		formatTime(now), formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return session, nil
}

// ListSessions returns the user's sessions, newest first.
func (s *Store) ListSessions(ctx context.Context, userID string) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+sessionColumns+` FROM sessions WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	sessions := []*Session{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// AllSessions returns every session, across users. Reconciliation needs it:
// containers outlive the browser session that created them.
func (s *Store) AllSessions(ctx context.Context) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("list all sessions: %w", err)
	}
	defer rows.Close()

	sessions := []*Session{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// SetSessionStatus records where a session got to, with an explanation when it
// failed.
func (s *Store) SetSessionStatus(ctx context.Context, id, status, errMessage string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, errMessage, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set session status: %w", err)
	}
	return nil
}

// SetSessionContainer records the container backing a session.
func (s *Store) SetSessionContainer(ctx context.Context, id, containerID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET container_id = ?, updated_at = ? WHERE id = ?`,
		containerID, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("set session container: %w", err)
	}
	return nil
}

// SetSessionAutoClaude records whether the session starts Claude Code. It is
// scoped to the owner, like every other session query, and reports ErrNotFound
// when there is no such session for them.
func (s *Store) SetSessionAutoClaude(ctx context.Context, userID, id string, auto bool) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET auto_claude = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		auto, formatTime(time.Now()), id, userID)
	if err != nil {
		return fmt.Errorf("set session auto claude: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSessionPorts records the ports a session publishes and the interface they
// bind. Unlike the other setters this one describes a container that is about
// to be rebuilt, not one that exists: the caller writes the row first, so a
// failed rebuild leaves a session claiming a binding it does not have rather
// than one publishing a binding it does not admit to.
func (s *Store) SetSessionPorts(ctx context.Context, userID, id string, ports []int, address string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET ports = ?, port_address = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		formatPorts(ports), address, formatTime(time.Now()), id, userID)
	if err != nil {
		return fmt.Errorf("set session ports: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSessionClaudeAccount records which Claude account a session's container
// authenticates with. Like SetSessionPorts, the caller writes the row before
// rebuilding the container it describes, so a failed rebuild leaves a session
// claiming an account its container does not carry rather than one carrying an
// account it does not admit to.
func (s *Store) SetSessionClaudeAccount(ctx context.Context, userID, id, accountID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET claude_account_id = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		accountID, formatTime(time.Now()), id, userID)
	if err != nil {
		return fmt.Errorf("set session claude account: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSession removes one of the user's sessions, or reports ErrNotFound.
func (s *Store) DeleteSession(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailInterruptedSessions marks sessions that were still being set up when the
// server stopped. Nothing is going to finish them.
func (s *Store) FailInterruptedSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET status = ?, error = ?, updated_at = ?
		WHERE status IN (?, ?, ?)`,
		SessionStatusFailed, "interrupted by a server restart", formatTime(time.Now()),
		SessionStatusCreating, SessionStatusCloning, SessionStatusStarting)
	if err != nil {
		return 0, fmt.Errorf("fail interrupted sessions: %w", err)
	}
	return res.RowsAffected()
}

// CountSessions reports how many sessions a user has. Every one of them is a
// container, a clone on disk and a workspace directory, which is why there is a
// cap on the number at all.
func (s *Store) CountSessions(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count sessions: %w", err)
	}
	return n, nil
}
