package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertUserCreatesThenUpdates(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	created, err := s.UpsertUser(ctx, &User{
		GitHubLogin: "alice",
		GitHubID:    42,
		AvatarURL:   "https://example.test/old.png",
	})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created user has no id")
	}
	if created.CreatedAt.IsZero() || created.LastLoginAt.IsZero() {
		t.Error("timestamps did not round-trip")
	}

	// A second login refreshes the token and the display fields but keeps the
	// identity: sessions and workspaces are tied to the id.
	updated, err := s.UpsertUser(ctx, &User{
		GitHubLogin: "alice-renamed",
		GitHubID:    42,
		AvatarURL:   "https://example.test/new.png",
	})
	if err != nil {
		t.Fatalf("UpsertUser again: %v", err)
	}
	if updated.ID != created.ID {
		t.Errorf("id changed on re-login: %s -> %s", created.ID, updated.ID)
	}
	if updated.GitHubLogin != "alice-renamed" {
		t.Errorf("re-login did not refresh the row: %+v", updated)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Error("created_at changed on re-login")
	}
}

func TestUserByIDReportsMissingUsers(t *testing.T) {
	if _, err := testStore(t).UserByID(context.Background(), "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("UserByID error = %v, want ErrNotFound", err)
	}
}

func TestUserSessionsLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	user, err := s.UpsertUser(ctx, &User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	live := []byte("live-hash")
	if err := s.CreateUserSession(ctx, live, user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}
	got, err := s.UserBySessionToken(ctx, live)
	if err != nil {
		t.Fatalf("UserBySessionToken: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("session resolved to %s, want %s", got.ID, user.ID)
	}

	expired := []byte("expired-hash")
	if err := s.CreateUserSession(ctx, expired, user.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("CreateUserSession (expired): %v", err)
	}
	if _, err := s.UserBySessionToken(ctx, expired); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session resolved: err = %v, want ErrNotFound", err)
	}

	n, err := s.DeleteExpiredUserSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredUserSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d sessions, want 1", n)
	}
	if _, err := s.UserBySessionToken(ctx, live); err != nil {
		t.Errorf("pruning removed the live session: %v", err)
	}

	if err := s.DeleteUserSession(ctx, live); err != nil {
		t.Fatalf("DeleteUserSession: %v", err)
	}
	if _, err := s.UserBySessionToken(ctx, live); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted session still resolves: err = %v", err)
	}
}
