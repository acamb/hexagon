package auth

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/andrea/hexagon/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newAllowlist(t *testing.T, st *store.Store, entries ...string) *Allowlist {
	t.Helper()
	list, err := NewAllowlist(entries, st, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	return list
}

func TestNewAllowlistRefusesAnEmptyList(t *testing.T) {
	st := openStore(t)
	for name, entries := range map[string][]string{
		"nothing at all":    nil,
		"only empty values": {"", "  "},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAllowlist(entries, st, slog.New(slog.DiscardHandler)); err == nil {
				t.Error("NewAllowlist accepted an empty list: every GitHub account would be admitted")
			}
		})
	}
}

func TestAllowlistAdmitsByLoginAndByAccountID(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	list := newAllowlist(t, st, "Alice", " 12345 ")

	for _, c := range []struct {
		name    string
		login   string
		id      int64
		allowed bool
	}{
		// GitHub logins are case insensitive, so the list is too.
		{"the listed login", "alice", 1, true},
		{"the listed login in another case", "ALICE", 1, true},
		{"the listed id", "someone-else", 12345, true},
		// The id is what was listed; the login it happens to carry is not on
		// the list and does not have to be.
		{"a login nobody listed", "mallory", 9, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			allowed, err := list.Allowed(ctx, c.login, c.id)
			if err != nil {
				t.Fatalf("Allowed: %v", err)
			}
			if allowed != c.allowed {
				t.Errorf("Allowed(%q, %d) = %v, want %v", c.login, c.id, allowed, c.allowed)
			}
		})
	}
}

// GitHub releases a login when an account is renamed, and anyone may claim it.
// Admitting on the login alone would hand this machine to whoever did.
func TestAllowlistRefusesALoginClaimedByAnotherAccount(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "alice", GitHubID: 1}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	list := newAllowlist(t, st, "alice")

	// The original account still passes, which is the whole point of not
	// simply refusing every login match.
	allowed, err := list.Allowed(ctx, "alice", 1)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if !allowed {
		t.Error("the account that owns the login was refused")
	}

	// A different account under the same login is somebody else. This used to
	// fail later and illegibly, when the insert hit the login's unique
	// constraint and the login ended in server_error.
	allowed, err = list.Allowed(ctx, "alice", 2)
	if err != nil {
		t.Fatalf("Allowed returned an error rather than a refusal: %v", err)
	}
	if allowed {
		t.Error("an account that claimed a released login was admitted")
	}

	// Case is not a way around it: GitHub would treat these as one account.
	allowed, err = list.Allowed(ctx, "Alice", 2)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if allowed {
		t.Error("the same claim in another case was admitted")
	}
}

// An id on the list is not affected by any of that: ids do not move.
func TestAllowlistAdmitsAListedIDWhateverTheLogin(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.UpsertUser(ctx, &store.User{GitHubLogin: "alice", GitHubID: 1}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	list := newAllowlist(t, st, "alice", "2")

	allowed, err := list.Allowed(ctx, "alice", 2)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if !allowed {
		t.Error("an account listed by id was refused because of the login it uses")
	}
}
