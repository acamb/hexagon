package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ProviderAccount is one account a user has connected as a source of
// repositories. The secret is sealed with the server key and only
// internal/auth ever opens it.
type ProviderAccount struct {
	ID     string
	UserID string
	// Provider is the kind, as internal/provider names it: "github",
	// "bitbucket".
	Provider string
	// Account is the name shown in the UI; Identity is what the provider's API
	// wants as the user half of its credentials, empty where it wants none.
	Account   string
	Identity  string
	AvatarURL string
	SecretEnc []byte
	// GitSecretEnc is a second sealed secret, used for git and nothing else,
	// nil when there is none. It exists because GitHub's secret_enc is the
	// OAuth token from signing in and expires within hours, while a container
	// keeps the credential it was created with.
	GitSecretEnc []byte
	// ExpiresAt is when secret_enc (the OAuth token) dies, as the provider
	// reported it at sign-in; the zero value means it advertised no expiry. The
	// login session is bound to this instant so a cookie cannot outlive the
	// token that makes it useful.
	ExpiresAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

const providerAccountColumns = `id, user_id, provider, account, identity, avatar_url, secret_enc,
	git_secret_enc, created_at, updated_at, expires_at`

// UpsertProviderAccount connects an account, replacing whatever was connected
// for that provider before: one account per provider per user, so reconnecting
// with a fresh token is the same operation as connecting.
//
// git_secret_enc is deliberately absent from the update: this runs at every
// GitHub sign-in, and the pasted token there is the one that has to survive an
// OAuth token being refreshed. SetProviderGitSecret is the only writer of it.
func (s *Store) UpsertProviderAccount(ctx context.Context, a *ProviderAccount) (*ProviderAccount, error) {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO provider_accounts (`+providerAccountColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, provider) DO UPDATE SET
			account    = excluded.account,
			identity   = excluded.identity,
			avatar_url = excluded.avatar_url,
			secret_enc = excluded.secret_enc,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at`,
		uuid.NewString(), a.UserID, a.Provider, a.Account, a.Identity, a.AvatarURL, a.SecretEnc,
		a.GitSecretEnc, formatTime(now), formatTime(now), nullTime(a.ExpiresAt))
	if err != nil {
		return nil, fmt.Errorf("upsert provider account: %w", err)
	}
	return s.ProviderAccount(ctx, a.UserID, a.Provider)
}

// ProviderAccount returns one connected account, or ErrNotFound.
func (s *Store) ProviderAccount(ctx context.Context, userID, provider string) (*ProviderAccount, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+providerAccountColumns+` FROM provider_accounts
		WHERE user_id = ? AND provider = ?`, userID, provider)

	account, err := scanProviderAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return account, err
}

// ListProviderAccounts returns every account the user has connected.
func (s *Store) ListProviderAccounts(ctx context.Context, userID string) ([]*ProviderAccount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+providerAccountColumns+` FROM provider_accounts
		WHERE user_id = ? ORDER BY provider`, userID)
	if err != nil {
		return nil, fmt.Errorf("list provider accounts: %w", err)
	}
	defer rows.Close()

	accounts := []*ProviderAccount{}
	for rows.Next() {
		account, err := scanProviderAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// SetProviderGitSecret stores the sealed secret git should use for an account,
// or clears it when sealed is nil. It reports ErrNotFound when that provider is
// not connected: a secret for an account that does not exist has nowhere to go.
func (s *Store) SetProviderGitSecret(ctx context.Context, userID, provider string, sealed []byte) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE provider_accounts SET git_secret_enc = ?, updated_at = ?
		WHERE user_id = ? AND provider = ?`, sealed, formatTime(time.Now()), userID, provider)
	if err != nil {
		return fmt.Errorf("set provider git secret: %w", err)
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

// DeleteProviderAccount forgets a connected account, or reports ErrNotFound.
func (s *Store) DeleteProviderAccount(ctx context.Context, userID, provider string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM provider_accounts WHERE user_id = ? AND provider = ?`, userID, provider)
	if err != nil {
		return fmt.Errorf("delete provider account: %w", err)
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

func scanProviderAccount(row scanner) (*ProviderAccount, error) {
	var (
		account              ProviderAccount
		createdAt, updatedAt string
		expiresAt            sql.NullString
	)
	err := row.Scan(&account.ID, &account.UserID, &account.Provider, &account.Account,
		&account.Identity, &account.AvatarURL, &account.SecretEnc, &account.GitSecretEnc,
		&createdAt, &updatedAt, &expiresAt)
	if err != nil {
		return nil, err
	}
	if account.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if account.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	if account.ExpiresAt, err = parseNullTime(expiresAt); err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}
	return &account, nil
}
