package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ClaudeCredential is the Anthropic credential a user configured from the UI.
// The secret is sealed with the server key and only internal/auth ever opens
// it.
type ClaudeCredential struct {
	UserID string
	// Kind is claudex.KindAPIKey or claudex.KindOAuthToken. It is kept as a
	// plain string so this package goes on depending on nothing above it.
	Kind      string
	SecretEnc []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

const claudeCredentialColumns = `user_id, kind, secret_enc, created_at, updated_at`

// UpsertClaudeCredential stores the credential this user configured, replacing
// whatever was stored before: one credential per user, so reconfiguring is the
// same operation as configuring.
func (s *Store) UpsertClaudeCredential(ctx context.Context, c *ClaudeCredential) (*ClaudeCredential, error) {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO claude_credentials (`+claudeCredentialColumns+`)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			kind       = excluded.kind,
			secret_enc = excluded.secret_enc,
			updated_at = excluded.updated_at`,
		c.UserID, c.Kind, c.SecretEnc, formatTime(now), formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("upsert claude credential: %w", err)
	}
	return s.ClaudeCredential(ctx, c.UserID)
}

// ClaudeCredential returns the credential this user configured, or ErrNotFound.
func (s *Store) ClaudeCredential(ctx context.Context, userID string) (*ClaudeCredential, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+claudeCredentialColumns+` FROM claude_credentials WHERE user_id = ?`, userID)

	credential, err := scanClaudeCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return credential, err
}

// DeleteClaudeCredential forgets the credential, or reports ErrNotFound.
func (s *Store) DeleteClaudeCredential(ctx context.Context, userID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM claude_credentials WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete claude credential: %w", err)
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

func scanClaudeCredential(row scanner) (*ClaudeCredential, error) {
	var (
		credential           ClaudeCredential
		createdAt, updatedAt string
	)
	err := row.Scan(&credential.UserID, &credential.Kind, &credential.SecretEnc, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if credential.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if credential.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	return &credential, nil
}
