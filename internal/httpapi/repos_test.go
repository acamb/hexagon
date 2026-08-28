package httpapi

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/andrea/hexagon/internal/provider"
)

// connectBitbucket links a Bitbucket account to the signed-in user, the way the
// accounts endpoint does.
func (e *testEnv) connectBitbucket(identity, secret string) {
	e.t.Helper()
	resp := e.sendJSON(http.MethodPut, "/api/accounts/bitbucket",
		fmt.Sprintf(`{"identity":%q,"secret":%q}`, identity, secret))
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("connect bitbucket: status = %d", resp.StatusCode)
	}
}

func TestListReposReturnsTheCallersRepositories(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.ghRepos.offer("acme/widgets", "main")

	resp := env.do(http.MethodGet, "/api/repos", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var listing listingResponse
	env.decode(resp, &listing)
	if len(listing.Repos) != 1 {
		t.Fatalf("got %d repositories, want 1", len(listing.Repos))
	}
	got := listing.Repos[0]
	if got.FullName != "acme/widgets" || got.DefaultBranch != "main" {
		t.Errorf("repository = %+v", got)
	}
	if got.Provider != provider.GitHub {
		t.Errorf("provider = %q, want github", got.Provider)
	}

	// The listing must use the signed-in user's own credentials, unsealed from
	// the database, not some server-wide token.
	seen := env.ghRepos.credentials()
	if len(seen) != 1 || seen[0].Secret != "gho_token" {
		t.Errorf("credentials used = %+v, want the caller's", seen)
	}
}

// The whole point of the change: two accounts, one list.
func TestListReposMergesEveryConnectedAccount(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.connectBitbucket("alice@example.test", "atlassian-token")
	env.ghRepos.offer("acme/widgets", "main")
	env.bbRepos.offer("acme/gadgets", "develop")

	var listing listingResponse
	env.decode(env.do(http.MethodGet, "/api/repos", nil), &listing)

	byName := map[string]repoResponse{}
	for _, repo := range listing.Repos {
		byName[repo.FullName] = repo
	}
	if len(byName) != 2 {
		t.Fatalf("got %d repositories, want one from each account: %+v", len(byName), listing.Repos)
	}
	if byName["acme/widgets"].Provider != provider.GitHub {
		t.Errorf("the GitHub repository is not marked as one: %+v", byName["acme/widgets"])
	}
	if byName["acme/gadgets"].Provider != provider.Bitbucket {
		t.Errorf("the Bitbucket repository is not marked as one: %+v", byName["acme/gadgets"])
	}

	// Bitbucket's API is addressed with the Atlassian email, which is why it is
	// stored alongside the token rather than derived from the account name.
	seen := env.bbRepos.credentials()
	last := seen[len(seen)-1]
	if last.Identity != "alice@example.test" || last.Secret != "atlassian-token" {
		t.Errorf("bitbucket was called with %+v", last)
	}
}

// One account being unreachable must not hide the other's repositories: the
// user is told what is missing and still gets on with their work.
func TestListReposKeepsGoingWhenOneAccountFails(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.connectBitbucket("alice@example.test", "atlassian-token")
	env.ghRepos.offer("acme/widgets", "main")
	env.bbRepos.fail(fmt.Errorf("listing: %w", provider.ErrUnauthorized))

	var listing listingResponse
	env.decode(env.do(http.MethodGet, "/api/repos", nil), &listing)

	if len(listing.Repos) != 1 || listing.Repos[0].FullName != "acme/widgets" {
		t.Errorf("repositories = %+v, want the working account's", listing.Repos)
	}
	if _, ok := listing.Failed["bitbucket"]; !ok {
		t.Errorf("the failing account was not reported: %+v", listing.Failed)
	}
}

func TestListReposRefreshInvalidatesTheCache(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	env.do(http.MethodGet, "/api/repos", nil)
	env.do(http.MethodGet, "/api/repos", nil)
	if calls := len(env.ghRepos.credentials()); calls != 1 {
		t.Errorf("made %d listings, want the second one served from the cache", calls)
	}

	env.do(http.MethodGet, "/api/repos?refresh=1", nil)
	if calls := len(env.ghRepos.credentials()); calls != 2 {
		t.Errorf("refresh did not refetch: %d listings", calls)
	}
}

func TestListReposRequiresASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	if got := env.do(http.MethodGet, "/api/repos", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
	if seen := env.ghRepos.credentials(); len(seen) != 0 {
		t.Errorf("an unauthenticated request reached a provider: %+v", seen)
	}
}

// The response is what the session dialog consumes, so the field names matter
// as much as the values.
func TestListReposUsesTheFrontendFieldNames(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.ghRepos.offer("acme/widgets", "main")

	var raw struct {
		Repos []map[string]any `json:"repos"`
	}
	env.decode(env.do(http.MethodGet, "/api/repos", nil), &raw)
	if len(raw.Repos) != 1 {
		t.Fatalf("got %d repositories", len(raw.Repos))
	}
	for _, field := range []string{"provider", "fullName", "cloneUrl", "defaultBranch", "private", "updatedAt"} {
		if _, ok := raw.Repos[0][field]; !ok {
			t.Errorf("the response has no %q field: %v", field, keys(raw.Repos[0]))
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

// A user with no Bitbucket account still gets their GitHub repositories, and
// nothing asks Bitbucket anything.
func TestListReposSkipsAccountsThatAreNotConnected(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.ghRepos.offer("acme/widgets", "main")
	env.bbRepos.offer("acme/gadgets", "develop")

	var listing listingResponse
	env.decode(env.do(http.MethodGet, "/api/repos", nil), &listing)
	if len(listing.Repos) != 1 || listing.Repos[0].Provider != provider.GitHub {
		t.Errorf("repositories = %+v, want only the connected account's", listing.Repos)
	}
	if seen := env.bbRepos.credentials(); len(seen) != 0 {
		t.Errorf("an account nobody connected was called anyway: %+v", seen)
	}
}
