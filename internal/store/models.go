package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// timeLayout is how every DATETIME column is written: UTC, fixed width, so
// string comparison in SQL matches chronological order.
const timeLayout = "2006-01-02 15:04:05.000"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

// User is someone allowed to use Hexagon, identified by their GitHub account.
//
// The account is the identity only. The token that came with it lives in
// provider_accounts, with the other accounts the user has connected, because
// listing repositories and cloning them are not identity.
type User struct {
	ID          string
	GitHubLogin string
	GitHubID    int64
	AvatarURL   string
	CreatedAt   time.Time
	LastLoginAt time.Time
}

// UpsertUser records a login, creating the user on first sight and refreshing
// the token, display fields and last_login_at afterwards.
func (s *Store) UpsertUser(ctx context.Context, u *User) (*User, error) {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, github_login, github_id, avatar_url, created_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(github_id) DO UPDATE SET
			github_login  = excluded.github_login,
			avatar_url    = excluded.avatar_url,
			last_login_at = excluded.last_login_at`,
		uuid.NewString(), u.GitHubLogin, u.GitHubID, u.AvatarURL,
		formatTime(now), formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}
	return s.UserByGitHubID(ctx, u.GitHubID)
}

// UserByID returns the user with the given id, or ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return s.userWhere(ctx, "id = ?", id)
}

// UserByGitHubID returns the user with the given GitHub account id, or ErrNotFound.
func (s *Store) UserByGitHubID(ctx context.Context, githubID int64) (*User, error) {
	return s.userWhere(ctx, "github_id = ?", githubID)
}

func (s *Store) userWhere(ctx context.Context, where string, args ...any) (*User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, github_login, github_id, avatar_url, created_at, last_login_at
		FROM users WHERE `+where, args...)
	return scanUser(row)
}

func scanUser(row *sql.Row) (*User, error) {
	var (
		u                      User
		createdAt, lastLoginAt string
	)
	err := row.Scan(&u.ID, &u.GitHubLogin, &u.GitHubID, &u.AvatarURL, &createdAt, &lastLoginAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("scan user: %w", err)
	}
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if u.LastLoginAt, err = parseTime(lastLoginAt); err != nil {
		return nil, fmt.Errorf("parse last_login_at: %w", err)
	}
	return &u, nil
}

// CreateUserSession stores a browser login session. tokenHash is the SHA-256 of
// the cookie value; the value itself is never persisted.
func (s *Store) CreateUserSession(ctx context.Context, tokenHash []byte, userID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_sessions (token_hash, user_id, created_at, expires_at)
		VALUES (?, ?, ?, ?)`,
		tokenHash, userID, formatTime(time.Now()), formatTime(expiresAt))
	if err != nil {
		return fmt.Errorf("create user session: %w", err)
	}
	return nil
}

// UserBySessionToken resolves a live session to its user. Expired sessions are
// treated as absent.
func (s *Store) UserBySessionToken(ctx context.Context, tokenHash []byte) (*User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.github_login, u.github_id, u.avatar_url, u.created_at, u.last_login_at
		FROM user_sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, formatTime(time.Now()))
	return scanUser(row)
}

// DeleteUserSession logs one browser out.
func (s *Store) DeleteUserSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return fmt.Errorf("delete user session: %w", err)
	}
	return nil
}

// DeleteExpiredUserSessions clears out sessions nobody can use any more.
func (s *Store) DeleteExpiredUserSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE expires_at <= ?`, formatTime(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("delete expired user sessions: %w", err)
	}
	return res.RowsAffected()
}
