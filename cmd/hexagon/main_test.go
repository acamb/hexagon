package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/httpapi"
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

// A server with no OAuth application configured has to start anyway: the
// first-time wizard is served by the same process, and it is the only way out
// of that state.
func TestBuildDepsStartsWithNoGitHubLoginConfigured(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	key := make([]byte, 32)
	deps, err := buildDeps(&config.Config{
		PublicURL:  "http://127.0.0.1:8080",
		SecretKey:  key,
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
	}, st, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildDeps with no OAuth settings: %v", err)
	}
	if deps.Gate.Configured() {
		t.Error("the gate reports a login on a server that was given none")
	}

	// And the wizard is open, because nobody has signed in.
	if err := openSetup(context.Background(), st, &deps, &config.Config{PublicURL: "http://127.0.0.1:8080"},
		slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("openSetup: %v", err)
	}
	if deps.Setup == nil {
		t.Error("no first-time setup on a server nobody has signed in to")
	}
}

// Once somebody has signed in there is nothing left to set up, and no password
// is printed at any later start.
func TestOpenSetupIsClosedOnceAUserExists(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if _, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "alice", GitHubID: 1}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	var deps httpapi.Deps
	if err := openSetup(ctx, st, &deps, &config.Config{PublicURL: "http://127.0.0.1:8080"},
		slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("openSetup: %v", err)
	}
	if deps.Setup != nil {
		t.Error("the first-time wizard is open on a server somebody has signed in to")
	}
}
