package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ClaudeAccountKindLogin is the kind of an account whose credential is a
// directory Hexagon keeps rather than a secret it stores. api_key and
// oauth_token are claudex.KindAPIKey and claudex.KindOAuthToken: the meaning
// of a kind is which environment variable the CLI reads, so those two are that
// package's word, kept as plain strings here for the reason ClaudeAccount.Kind
// gives.
const ClaudeAccountKindLogin = "login"

// ClaudeAccount is one Claude Code identity a user has configured: a pasted
// API key or OAuth token, or a subscription signed in to through the browser.
// The secret is sealed with the server key and only internal/auth ever opens
// it.
type ClaudeAccount struct {
	ID     string
	UserID string
	Name   string
	// Kind is claudex.KindAPIKey, claudex.KindOAuthToken, or
	// ClaudeAccountKindLogin. It is kept as a plain string so this package goes
	// on depending on nothing above it.
	Kind string
	// SecretEnc is nil for a login account: its credential is a file Claude
	// Code itself writes, not a secret this table holds.
	SecretEnc []byte
	IsDefault bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

const claudeAccountColumns = `id, user_id, name, kind, secret_enc, is_default, created_at, updated_at`

// CreateClaudeAccount adds a new account, assigning it an id when it has none.
// ErrConflict reports a name this user already has, or — because is_default is
// a partial unique index — a second account asking to be the default at the
// same time.
func (s *Store) CreateClaudeAccount(ctx context.Context, a *ClaudeAccount) (*ClaudeAccount, error) {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO claude_accounts (`+claudeAccountColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, a.Name, a.Kind, a.SecretEnc, a.IsDefault, formatTime(now), formatTime(now))
	switch {
	case isUniqueViolation(err):
		return nil, ErrConflict
	case err != nil:
		return nil, fmt.Errorf("create claude account: %w", err)
	}
	return s.ClaudeAccountByID(ctx, a.UserID, a.ID)
}

// ClaudeAccountByID returns one of the user's accounts, or ErrNotFound.
// Scoping the lookup to the owner means a wrong id and someone else's id look
// the same.
func (s *Store) ClaudeAccountByID(ctx context.Context, userID, id string) (*ClaudeAccount, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+claudeAccountColumns+` FROM claude_accounts WHERE id = ? AND user_id = ?`, id, userID)
	account, err := scanClaudeAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return account, err
}

// ListClaudeAccounts returns every account the user has configured, oldest
// first: the order they were added, and the order the first one became the
// default.
func (s *Store) ListClaudeAccounts(ctx context.Context, userID string) ([]*ClaudeAccount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+claudeAccountColumns+` FROM claude_accounts WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("list claude accounts: %w", err)
	}
	defer rows.Close()

	accounts := []*ClaudeAccount{}
	for rows.Next() {
		account, err := scanClaudeAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// DefaultClaudeAccount returns the user's default account, or ErrNotFound when
// they have configured none — the ordinary state of a Hexagon configured
// entirely from a file.
func (s *Store) DefaultClaudeAccount(ctx context.Context, userID string) (*ClaudeAccount, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+claudeAccountColumns+` FROM claude_accounts WHERE user_id = ? AND is_default = 1`, userID)
	account, err := scanClaudeAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return account, err
}

// RenameClaudeAccount changes an account's name, or reports ErrNotFound or,
// for a name this user already has, ErrConflict.
func (s *Store) RenameClaudeAccount(ctx context.Context, userID, id, name string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE claude_accounts SET name = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		name, formatTime(time.Now()), id, userID)
	switch {
	case isUniqueViolation(err):
		return ErrConflict
	case err != nil:
		return fmt.Errorf("rename claude account: %w", err)
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

// SetClaudeAccountSecret replaces a pasted account's sealed secret, or reports
// ErrNotFound.
func (s *Store) SetClaudeAccountSecret(ctx context.Context, userID, id string, secretEnc []byte) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE claude_accounts SET secret_enc = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		secretEnc, formatTime(time.Now()), id, userID)
	if err != nil {
		return fmt.Errorf("set claude account secret: %w", err)
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

// SetDefaultClaudeAccount makes one account the default, clearing whichever
// account held it before. The two updates run in one transaction so the
// partial unique index never has to reject the moment where both would
// otherwise be true at once.
func (s *Store) SetDefaultClaudeAccount(ctx context.Context, userID, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := formatTime(time.Now())
	if _, err := tx.ExecContext(ctx, `
		UPDATE claude_accounts SET is_default = 0, updated_at = ? WHERE user_id = ? AND is_default = 1`,
		now, userID); err != nil {
		return fmt.Errorf("clear the current default claude account: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE claude_accounts SET is_default = 1, updated_at = ? WHERE id = ? AND user_id = ?`,
		now, id, userID)
	if err != nil {
		return fmt.Errorf("set default claude account: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// DeleteClaudeAccount removes an account, or reports ErrNotFound. The
// directory a login account wrote its credential into is the caller's to
// remove: this package owns the row, not the filesystem layout above it.
func (s *Store) DeleteClaudeAccount(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM claude_accounts WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete claude account: %w", err)
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

// CountSessionsUsingClaudeAccount reports how many sessions still name this
// account, the same guard CountSessionsUsingImage gives images.
func (s *Store) CountSessionsUsingClaudeAccount(ctx context.Context, accountID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE claude_account_id = ?`, accountID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count sessions using claude account: %w", err)
	}
	return n, nil
}

func scanClaudeAccount(row scanner) (*ClaudeAccount, error) {
	var (
		account              ClaudeAccount
		createdAt, updatedAt string
	)
	err := row.Scan(&account.ID, &account.UserID, &account.Name, &account.Kind, &account.SecretEnc,
		&account.IsDefault, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if account.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if account.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	return &account, nil
}
