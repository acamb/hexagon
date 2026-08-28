package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/github"
)

// fakeRepos records what the handler asks for, so the test can check that the
// caller's own token is used and that a refresh really invalidates.
type fakeRepos struct {
	mu          sync.Mutex
	repos       []github.Repo
	err         error
	tokens      []string
	invalidated []string
}

func (f *fakeRepos) List(_ context.Context, _ string, token string) ([]github.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	return f.repos, f.err
}

func (f *fakeRepos) Invalidate(userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, userID)
}

func (f *fakeRepos) seen() (tokens, invalidated []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...), append([]string(nil), f.invalidated...)
}

func TestListReposReturnsTheCallersRepositories(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.repos.repos = []github.Repo{{
		FullName:      "acme/widgets",
		CloneURL:      "https://github.com/acme/widgets.git",
		DefaultBranch: "main",
		Private:       true,
		Description:   "widgets",
		UpdatedAt:     time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC),
	}}

	resp := env.do(http.MethodGet, "/api/github/repos", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var repos []repoResponse
	env.decode(resp, &repos)
	if len(repos) != 1 {
		t.Fatalf("got %d repositories, want 1", len(repos))
	}
	got := repos[0]
	if got.FullName != "acme/widgets" || got.DefaultBranch != "main" || !got.Private {
		t.Errorf("repository = %+v", got)
	}

	// The listing must use the signed-in user's own token, unsealed from the
	// database, not some server-wide credential.
	tokens, _ := env.repos.seen()
	if len(tokens) != 1 || tokens[0] != "gho_token" {
		t.Errorf("tokens used = %v, want the caller's", tokens)
	}
}

func TestListReposRefreshInvalidatesTheCache(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	env.do(http.MethodGet, "/api/github/repos", nil)
	if _, invalidated := env.repos.seen(); len(invalidated) != 0 {
		t.Errorf("a plain listing invalidated the cache: %v", invalidated)
	}

	env.do(http.MethodGet, "/api/github/repos?refresh=1", nil)
	if _, invalidated := env.repos.seen(); len(invalidated) != 1 {
		t.Errorf("refresh did not invalidate the cache: %v", invalidated)
	}
}

// A token GitHub no longer accepts is a sign-in problem, not a server fault:
// 401 is what makes the SPA send the user back to the login page.
func TestListReposReportsARejectedTokenAsUnauthorized(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.repos.err = fmt.Errorf("listing: %w", github.ErrUnauthorized)

	if got := env.do(http.MethodGet, "/api/github/repos", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}

func TestListReposReportsGitHubBeingUnreachable(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.repos.err = fmt.Errorf("dial tcp: connection refused")

	if got := env.do(http.MethodGet, "/api/github/repos", nil).StatusCode; got != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", got)
	}
}

func TestListReposRequiresASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	if got := env.do(http.MethodGet, "/api/github/repos", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
	if tokens, _ := env.repos.seen(); len(tokens) != 0 {
		t.Errorf("an unauthenticated request reached GitHub: %v", tokens)
	}
}

// The response is what the session dialog will consume, so the field names
// matter as much as the values.
func TestListReposUsesTheFrontendFieldNames(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.repos.repos = []github.Repo{{FullName: "acme/widgets", CloneURL: "https://example.test/x.git"}}

	var raw []map[string]any
	env.decode(env.do(http.MethodGet, "/api/github/repos", nil), &raw)
	if len(raw) != 1 {
		t.Fatalf("got %d repositories", len(raw))
	}
	for _, field := range []string{"fullName", "cloneUrl", "defaultBranch", "private", "updatedAt"} {
		if _, ok := raw[0][field]; !ok {
			t.Errorf("the response has no %q field: %v", field, keys(raw[0]))
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
