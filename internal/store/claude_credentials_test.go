package store

import (
	"context"
	"errors"
	"testing"
)

func TestUpsertClaudeCredentialStoresThenReplaces(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	stored, err := s.UpsertClaudeCredential(ctx, &ClaudeCredential{
		UserID: user.ID, Kind: "api_key", SecretEnc: []byte("sealed-1"),
	})
	if err != nil {
		t.Fatalf("UpsertClaudeCredential: %v", err)
	}
	if stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
		t.Errorf("stored credential = %+v", stored)
	}

	// Replacing keeps created_at: it records when the user first configured
	// one, and replacing a credential is not creating one.
	replaced, err := s.UpsertClaudeCredential(ctx, &ClaudeCredential{
		UserID: user.ID, Kind: "oauth_token", SecretEnc: []byte("sealed-2"),
	})
	if err != nil {
		t.Fatalf("UpsertClaudeCredential again: %v", err)
	}
	if !replaced.CreatedAt.Equal(stored.CreatedAt) {
		t.Errorf("created_at changed on replace: %v -> %v", stored.CreatedAt, replaced.CreatedAt)
	}
	if replaced.Kind != "oauth_token" || string(replaced.SecretEnc) != "sealed-2" {
		t.Errorf("replace did not refresh the row: %+v", replaced)
	}
}

func TestClaudeCredentialIsScopedToItsOwner(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	alice := testUser(t, s, "alice", 1)
	bob := testUser(t, s, "bob", 2)

	if _, err := s.UpsertClaudeCredential(ctx, &ClaudeCredential{
		UserID: alice.ID, Kind: "api_key", SecretEnc: []byte("x"),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if _, err := s.ClaudeCredential(ctx, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for someone else's credential", err)
	}
	if err := s.DeleteClaudeCredential(ctx, bob.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound when deleting someone else's credential", err)
	}
	if _, err := s.ClaudeCredential(ctx, alice.ID); err != nil {
		t.Errorf("the owner's credential was affected: %v", err)
	}

	if err := s.DeleteClaudeCredential(ctx, alice.ID); err != nil {
		t.Fatalf("DeleteClaudeCredential: %v", err)
	}
	if _, err := s.ClaudeCredential(ctx, alice.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound after deleting", err)
	}
	if err := s.DeleteClaudeCredential(ctx, alice.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete error = %v, want ErrNotFound", err)
	}
}

func TestClaudeCredentialsRejectsAThirdKind(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	if _, err := s.UpsertClaudeCredential(ctx, &ClaudeCredential{
		UserID: user.ID, Kind: "something_else", SecretEnc: []byte("x"),
	}); err == nil {
		t.Error("the CHECK constraint accepted a third kind")
	}
}
