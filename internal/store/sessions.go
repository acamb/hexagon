package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	ID           string
	UserID       string
	Title        string
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
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const sessionColumns = `id, user_id, title, repo_full_name, repo_clone_url, branch, image_id, image_ref,
	workspace_dir, repo_dir, container_id, status, error, created_at, updated_at`

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
		createdAt, updatedAt string
	)
	err := row.Scan(&session.ID, &session.UserID, &session.Title, &session.RepoFullName,
		&session.RepoCloneURL, &session.Branch, &session.ImageID, &session.ImageRef,
		&session.WorkspaceDir, &session.RepoDir, &session.ContainerID, &session.Status,
		&session.Error, &createdAt, &updatedAt)
	if err != nil {
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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.UserID, session.Title, session.RepoFullName, session.RepoCloneURL,
		session.Branch, session.ImageID, session.ImageRef, session.WorkspaceDir, session.RepoDir,
		session.ContainerID, session.Status, session.Error, formatTime(now), formatTime(now))
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
