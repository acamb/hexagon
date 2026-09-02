package store

import (
	"context"
	"errors"
	"testing"
)

func TestCreateClaudeAccountThenListAndGet(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	account, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: user.ID, Name: "work", Kind: "api_key", SecretEnc: []byte("sealed-1"), IsDefault: true,
	})
	if err != nil {
		t.Fatalf("CreateClaudeAccount: %v", err)
	}
	if account.CreatedAt.IsZero() || account.UpdatedAt.IsZero() {
		t.Errorf("account = %+v", account)
	}
	if !account.IsDefault {
		t.Error("the first account was not stored as the default")
	}

	found, err := s.ClaudeAccountByID(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatalf("ClaudeAccountByID: %v", err)
	}
	if found.Name != "work" || string(found.SecretEnc) != "sealed-1" {
		t.Errorf("found = %+v, want the account as it was created", found)
	}

	accounts, err := s.ListClaudeAccounts(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListClaudeAccounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != account.ID {
		t.Errorf("accounts = %+v, want the one just created", accounts)
	}

	def, err := s.DefaultClaudeAccount(ctx, user.ID)
	if err != nil || def.ID != account.ID {
		t.Errorf("DefaultClaudeAccount = %+v, %v, want %q", def, err, account.ID)
	}
}

// A login account is a row with a name and a directory and no secret at all —
// the honest description of one that has never been signed in to.
func TestCreateClaudeAccountWithNoSecretForALoginAccount(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	account, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: user.ID, Name: "subscription", Kind: ClaudeAccountKindLogin,
	})
	if err != nil {
		t.Fatalf("CreateClaudeAccount: %v", err)
	}
	if account.SecretEnc != nil {
		t.Errorf("secret_enc = %v, want nil for a login account", account.SecretEnc)
	}

	found, err := s.ClaudeAccountByID(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatalf("ClaudeAccountByID: %v", err)
	}
	if found.SecretEnc != nil {
		t.Errorf("secret_enc after reload = %v, want nil", found.SecretEnc)
	}
}

func TestClaudeAccountsAreScopedToTheirOwner(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	alice := testUser(t, s, "alice", 1)
	bob := testUser(t, s, "bob", 2)

	account, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: alice.ID, Name: "work", Kind: "api_key", SecretEnc: []byte("x"), IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.ClaudeAccountByID(ctx, bob.ID, account.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for someone else's account", err)
	}
	if err := s.DeleteClaudeAccount(ctx, bob.ID, account.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound deleting someone else's account", err)
	}
	if _, err := s.DefaultClaudeAccount(ctx, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound: bob has configured none", err)
	}

	if err := s.DeleteClaudeAccount(ctx, alice.ID, account.ID); err != nil {
		t.Fatalf("DeleteClaudeAccount: %v", err)
	}
	if _, err := s.ClaudeAccountByID(ctx, alice.ID, account.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound after deleting", err)
	}
}

func TestClaudeAccountsRejectAFourthKind(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	if _, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: user.ID, Name: "x", Kind: "something_else", SecretEnc: []byte("x"),
	}); err == nil {
		t.Error("the CHECK constraint accepted a fourth kind")
	}
}

// UNIQUE(user_id, name) is what stops a card offering a rename that quietly
// collides with a sibling account.
func TestClaudeAccountNameMustBeUniquePerUser(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	alice := testUser(t, s, "alice", 1)
	bob := testUser(t, s, "bob", 2)

	if _, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: alice.ID, Name: "work", Kind: "api_key", SecretEnc: []byte("x"), IsDefault: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: alice.ID, Name: "work", Kind: "oauth_token", SecretEnc: []byte("y"),
	}); !errors.Is(err, ErrConflict) {
		t.Errorf("error = %v, want ErrConflict for a duplicate name", err)
	}
	// The same name for a different user is no conflict at all.
	if _, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: bob.ID, Name: "work", Kind: "api_key", SecretEnc: []byte("z"), IsDefault: true,
	}); err != nil {
		t.Errorf("a name shared across users was refused: %v", err)
	}

	second, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: alice.ID, Name: "personal", Kind: "api_key", SecretEnc: []byte("y"),
	})
	if err != nil {
		t.Fatalf("create second account: %v", err)
	}
	if err := s.RenameClaudeAccount(ctx, alice.ID, second.ID, "work"); !errors.Is(err, ErrConflict) {
		t.Errorf("rename error = %v, want ErrConflict", err)
	}
	if err := s.RenameClaudeAccount(ctx, alice.ID, second.ID, "renamed"); err != nil {
		t.Fatalf("rename to a free name: %v", err)
	}
}

// The partial unique index is what makes "at most one default per user" a fact
// the database enforces; SetDefaultClaudeAccount is the only path that is
// allowed to move it.
func TestOnlyOneDefaultClaudeAccountPerUser(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	first, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: user.ID, Name: "first", Kind: "api_key", SecretEnc: []byte("x"), IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: user.ID, Name: "second", Kind: "api_key", SecretEnc: []byte("y"), IsDefault: true,
	}); !errors.Is(err, ErrConflict) {
		t.Errorf("a second default at creation = %v, want ErrConflict", err)
	}

	second, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: user.ID, Name: "second", Kind: "api_key", SecretEnc: []byte("y"),
	})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	if err := s.SetDefaultClaudeAccount(ctx, user.ID, second.ID); err != nil {
		t.Fatalf("SetDefaultClaudeAccount: %v", err)
	}
	def, err := s.DefaultClaudeAccount(ctx, user.ID)
	if err != nil || def.ID != second.ID {
		t.Errorf("default = %+v, %v, want %q", def, err, second.ID)
	}
	reloaded, err := s.ClaudeAccountByID(ctx, user.ID, first.ID)
	if err != nil || reloaded.IsDefault {
		t.Errorf("the old default is still marked default: %+v, %v", reloaded, err)
	}

	if err := s.SetDefaultClaudeAccount(ctx, user.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for an account that does not exist", err)
	}
}

func TestSetClaudeAccountSecretThenReadBack(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	account, err := s.CreateClaudeAccount(ctx, &ClaudeAccount{
		UserID: user.ID, Name: "work", Kind: "api_key", SecretEnc: []byte("sealed-1"), IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetClaudeAccountSecret(ctx, user.ID, account.ID, []byte("sealed-2")); err != nil {
		t.Fatalf("SetClaudeAccountSecret: %v", err)
	}
	found, err := s.ClaudeAccountByID(ctx, user.ID, account.ID)
	if err != nil || string(found.SecretEnc) != "sealed-2" {
		t.Errorf("secret_enc = %q, %v, want the replacement", found.SecretEnc, err)
	}
}
