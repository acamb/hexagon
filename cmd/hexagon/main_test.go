package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/store"
)

// Removing someone from the allowlist has to reach a browser that never comes
// back, which the per-request check by itself cannot do.
func TestPruneRevokedSessionsSignsOutOnlyUsersNoLongerAllowed(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	alice, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	mallory, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "mallory", GitHubID: 2})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	for _, s := range []struct {
		token []byte
		user  string
	}{{[]byte("alice"), alice.ID}, {[]byte("mallory"), mallory.ID}} {
		if err := st.CreateUserSession(ctx, s.token, s.user, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	allowlist, err := auth.NewAllowlist([]string{"alice"}, st, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new allowlist: %v", err)
	}
	pruneRevokedSessions(ctx, st, allowlist, slog.New(slog.DiscardHandler))

	if _, err := st.UserBySessionToken(ctx, []byte("alice")); err != nil {
		t.Errorf("the allowed user was signed out: %v", err)
	}
	if _, err := st.UserBySessionToken(ctx, []byte("mallory")); err == nil {
		t.Error("a user who is not in the allowlist kept their session")
	}
}
