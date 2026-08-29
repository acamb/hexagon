package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestUpsertProviderAccountConnectsThenReplaces(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	connected, err := s.UpsertProviderAccount(ctx, &ProviderAccount{
		UserID: user.ID, Provider: "bitbucket", Account: "alice-bb",
		Identity: "alice@example.test", SecretEnc: []byte("sealed-1"),
	})
	if err != nil {
		t.Fatalf("UpsertProviderAccount: %v", err)
	}
	if connected.ID == "" || connected.CreatedAt.IsZero() {
		t.Errorf("connected account = %+v", connected)
	}

	// Reconnecting with a fresh token replaces the secret rather than adding a
	// second account: one per provider is the rule the unique index enforces.
	again, err := s.UpsertProviderAccount(ctx, &ProviderAccount{
		UserID: user.ID, Provider: "bitbucket", Account: "alice-renamed",
		Identity: "alice@example.test", SecretEnc: []byte("sealed-2"),
	})
	if err != nil {
		t.Fatalf("UpsertProviderAccount again: %v", err)
	}
	if again.ID != connected.ID {
		t.Errorf("id changed on reconnect: %s -> %s", connected.ID, again.ID)
	}
	if string(again.SecretEnc) != "sealed-2" || again.Account != "alice-renamed" {
		t.Errorf("reconnect did not refresh the row: %+v", again)
	}

	accounts, err := s.ListProviderAccounts(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListProviderAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("got %d accounts, want the one, replaced", len(accounts))
	}
}

func TestProviderAccountsAreScopedToTheirOwner(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	alice := testUser(t, s, "alice", 1)
	bob := testUser(t, s, "bob", 2)

	if _, err := s.UpsertProviderAccount(ctx, &ProviderAccount{
		UserID: alice.ID, Provider: "bitbucket", Account: "alice-bb", SecretEnc: []byte("x"),
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	if _, err := s.ProviderAccount(ctx, bob.ID, "bitbucket"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for someone else's account", err)
	}
	if err := s.DeleteProviderAccount(ctx, bob.ID, "bitbucket"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound when deleting someone else's account", err)
	}
	if _, err := s.ProviderAccount(ctx, alice.ID, "bitbucket"); err != nil {
		t.Errorf("the owner's account was affected: %v", err)
	}

	if err := s.DeleteProviderAccount(ctx, alice.ID, "bitbucket"); err != nil {
		t.Fatalf("DeleteProviderAccount: %v", err)
	}
	if _, err := s.ProviderAccount(ctx, alice.ID, "bitbucket"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound after deleting", err)
	}
}

// The migration that moves the GitHub token out of users drops the column it
// read from, so it gets exactly one chance to be right. This builds a database
// at the schema before it and checks what came out the other side.
func TestMigrationMovesTheGitHubTokenOutOfUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexagon.db")
	seedSchemaVersion(t, path, 2, func(db *sql.DB) {
		if _, err := db.Exec(`
			INSERT INTO users (id, github_login, github_id, avatar_url, github_token_enc, created_at, last_login_at)
			VALUES ('u-1', 'alice', 42, 'https://example.test/a.png', ?, '2026-01-01 00:00:00.000', '2026-01-02 00:00:00.000')`,
			[]byte("sealed-token")); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		if _, err := db.Exec(`
			INSERT INTO images (id, user_id, name, source_type, status, created_at)
			VALUES ('i-1', 'u-1', 'base', 'dockerfile', 'ready', '2026-01-01 00:00:00.000')`); err != nil {
			t.Fatalf("insert image: %v", err)
		}
		if _, err := db.Exec(`
			INSERT INTO sessions (id, user_id, title, repo_full_name, repo_clone_url, image_id, image_ref,
				workspace_dir, repo_dir, status, created_at, updated_at)
			VALUES ('s-1', 'u-1', 'widgets', 'acme/widgets', 'https://github.com/acme/widgets.git', 'i-1', 'ref',
				'/w/s-1', '/w/s-1/repo', 'stopped', '2026-01-01 00:00:00.000', '2026-01-01 00:00:00.000')`); err != nil {
			t.Fatalf("insert session: %v", err)
		}
	})

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	account, err := s.ProviderAccount(ctx, "u-1", "github")
	if err != nil {
		t.Fatalf("the user's GitHub token did not survive the migration: %v", err)
	}
	if string(account.SecretEnc) != "sealed-token" {
		t.Errorf("secret = %q, want the sealed token from users", account.SecretEnc)
	}
	if account.Account != "alice" || account.ID == "" {
		t.Errorf("migrated account = %+v", account)
	}

	// Everything that existed came from GitHub, and has to keep starting.
	session, err := s.SessionByID(ctx, "u-1", "s-1")
	if err != nil {
		t.Fatalf("load the migrated session: %v", err)
	}
	if session.Provider != "github" {
		t.Errorf("session provider = %q, want github", session.Provider)
	}
	// And keeps the credentials its container was already created with: that
	// environment cannot be changed now, so any other value would be a lie.
	if !session.PropagateToken {
		t.Error("propagateToken = false, want the token an existing container already carries")
	}
	// And with the integration off: its container has neither the mount nor the
	// published port, and any other value would be a lie about what is inside it.
	if session.VSCode {
		t.Error("vscode = true, want off for a session whose container predates the integration")
	}

	// And the column it was copied from is gone: two places holding one secret
	// is what this migration exists to prevent.
	if err := s.DB().QueryRow(`SELECT github_token_enc FROM users`).Scan(new([]byte)); err == nil {
		t.Error("users.github_token_enc is still there")
	}
}

// seedSchemaVersion builds a database with the first n migrations applied and
// nothing more, then lets seed put rows in it.
func seedSchemaVersion(t *testing.T, path string, n int, seed func(*sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range mustLoadMigrations(t) {
		if m.version > n {
			break
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.version, m.name); err != nil {
			t.Fatalf("record %s: %v", m.name, err)
		}
	}
	seed(db)
}
