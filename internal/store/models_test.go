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
	if err := s.CreateUserSession(ctx, live, user.ID, time.Now().Add(time.Hour), time.Time{}); err != nil {
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
	if err := s.CreateUserSession(ctx, expired, user.ID, time.Now().Add(-time.Minute), time.Time{}); err != nil {
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

// A session bound to an OAuth token (token_expires_at is set) is never renewed:
// its life is the token's, so it expires with the credential rather than being
// slid forward. This is the backstop for a stolen cookie.
func TestTouchUserSessionNeverRenewsATokenBoundSession(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	user, err := s.UpsertUser(ctx, &User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	token := []byte("bound-token-hash")
	tokenExpiry := time.Now().Add(time.Hour)
	// A token-bound session's expires_at is the token expiry; pass it as both.
	if err := s.CreateUserSession(ctx, token, user.ID, tokenExpiry, tokenExpiry); err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}

	// Well past the half life, so an unbound session would be extended here.
	renewed, err := s.TouchUserSession(ctx, token, time.Now().Add(90*time.Minute), time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("TouchUserSession: %v", err)
	}
	if renewed {
		t.Error("a token-bound session was extended; it must die with its token")
	}

	var got string
	if err := s.DB().QueryRow(`SELECT expires_at FROM user_sessions WHERE token_hash = ?`, token).Scan(&got); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if got != formatTime(tokenExpiry) {
		t.Errorf("expires_at = %q, want the untouched token expiry %q", got, formatTime(tokenExpiry))
	}
}

// A token-bound session past its token's expiry reads as absent, exactly like
// any other expired session: expires_at is the token expiry, so the ordinary
// expiry filter already covers it.
func TestUserBySessionTokenRejectsAnExpiredTokenBoundSession(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	user, err := s.UpsertUser(ctx, &User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	token := []byte("dead-token-hash")
	past := time.Now().Add(-time.Minute)
	if err := s.CreateUserSession(ctx, token, user.ID, past, past); err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}

	if _, err := s.UserBySessionToken(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token-bound session still resolves: err = %v", err)
	}
}

// The renewal boundary: a session in the first half of its life costs no write
// at all, and one past it is extended.
func TestTouchUserSessionRenewsOnlyPastHalfLife(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	user, err := s.UpsertUser(ctx, &User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	token := []byte("token-hash")
	expires := time.Now().Add(time.Hour)
	if err := s.CreateUserSession(ctx, token, user.ID, expires, time.Time{}); err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}

	renewed, err := s.TouchUserSession(ctx, token, time.Now().Add(30*time.Minute), time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("TouchUserSession: %v", err)
	}
	if renewed {
		t.Error("a session in the first half of its life was extended")
	}

	want := time.Now().Add(2 * time.Hour)
	renewed, err = s.TouchUserSession(ctx, token, time.Now().Add(90*time.Minute), want)
	if err != nil {
		t.Fatalf("TouchUserSession: %v", err)
	}
	if !renewed {
		t.Fatal("a session past its half life was not extended")
	}

	var got string
	if err := s.DB().QueryRow(`SELECT expires_at FROM user_sessions WHERE token_hash = ?`, token).Scan(&got); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if got != formatTime(want) {
		t.Errorf("expires_at = %q, want %q", got, formatTime(want))
	}

	// An unknown token renews nothing, which is what keeps a stale cookie from
	// resurrecting a session that was signed out.
	renewed, err = s.TouchUserSession(ctx, []byte("nobody"), time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("TouchUserSession: %v", err)
	}
	if renewed {
		t.Error("an unknown token was renewed")
	}
}

// The startup sweep needs both halves: who exists, and how to sign one of them
// out everywhere.
func TestDeleteUserSessionsForUserLeavesOtherUsersAlone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	alice, err := s.UpsertUser(ctx, &User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	bob, err := s.UpsertUser(ctx, &User{GitHubLogin: "bob", GitHubID: 2})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	for i, session := range []struct {
		token []byte
		user  string
	}{{[]byte("a1"), alice.ID}, {[]byte("a2"), alice.ID}, {[]byte("b1"), bob.ID}} {
		if err := s.CreateUserSession(ctx, session.token, session.user, time.Now().Add(time.Hour), time.Time{}); err != nil {
			t.Fatalf("CreateUserSession %d: %v", i, err)
		}
	}

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("ListUsers returned %d users, want 2", len(users))
	}

	n, err := s.DeleteUserSessionsForUser(ctx, alice.ID)
	if err != nil {
		t.Fatalf("DeleteUserSessionsForUser: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d sessions, want 2", n)
	}
	if _, err := s.UserBySessionToken(ctx, []byte("a1")); !errors.Is(err, ErrNotFound) {
		t.Errorf("alice's session survived: err = %v", err)
	}
	if _, err := s.UserBySessionToken(ctx, []byte("b1")); err != nil {
		t.Errorf("bob's session was deleted too: %v", err)
	}
}
