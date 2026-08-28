package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
