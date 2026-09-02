package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/andrea/hexagon/internal/provider"
)

// creds is what the provider interface takes; GitHub only reads the secret.
func creds(token string) provider.Credentials { return provider.Credentials{Secret: token} }

// pagedRepos serves a two-page listing the way GitHub does, with the next page
// announced only through the Link header.
func pagedRepos(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/user/repos" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"full_name":"acme/second","clone_url":"https://github.com/acme/second.git","default_branch":"trunk","private":true,"updated_at":"2026-02-02T10:00:00Z"}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?page=2>; rel="next", <%s/user/repos?page=2>; rel="last"`, server.URL, server.URL))
		fmt.Fprint(w, `[{"full_name":"acme/first","clone_url":"https://github.com/acme/first.git","default_branch":"main","description":"the first one","updated_at":"2026-03-03T10:00:00Z"}]`)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestListReposFollowsPagination(t *testing.T) {
	var requests atomic.Int32
	server := pagedRepos(t, &requests)

	repos, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), creds("token"))
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repositories, want both pages", len(repos))
	}
	if repos[0].FullName != "acme/first" || repos[1].FullName != "acme/second" {
		t.Errorf("repositories = %+v", repos)
	}
	if repos[0].Description != "the first one" || repos[0].DefaultBranch != "main" {
		t.Errorf("first repository = %+v", repos[0])
	}
	if !repos[1].Private || repos[1].DefaultBranch != "trunk" {
		t.Errorf("second repository = %+v", repos[1])
	}
	if repos[0].UpdatedAt.IsZero() {
		t.Error("updated_at did not parse")
	}
	// Every repository has to say where it came from: the merged listing is
	// sorted and filtered by it, and a session's credentials follow it.
	if repos[0].Provider != provider.GitHub {
		t.Errorf("provider = %q, want %q", repos[0].Provider, provider.GitHub)
	}
}

func TestNextPageURL(t *testing.T) {
	cases := map[string]string{
		`<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=9>; rel="last"`:  "https://api.github.com/user/repos?page=2",
		`<https://api.github.com/user/repos?page=1>; rel="prev", <https://api.github.com/user/repos?page=1>; rel="first"`: "",
		"":        "",
		"garbage": "",
	}
	for header, want := range cases {
		if got := nextPageURL(header); got != want {
			t.Errorf("nextPageURL(%q) = %q, want %q", header, got, want)
		}
	}
}

// A token GitHub refuses is not a transient failure, and the caller has to be
// able to tell it apart so it can ask for a new sign-in.
func TestListReposReportsARejectedToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
	}))
	defer server.Close()

	_, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), creds("stale"))
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized", err)
	}
}

// The pasted token wins wherever git is what needs authenticating, which is the
// whole point of storing a second secret: the account's own is the OAuth token
// from signing in, and it expires while sessions are still running.
func TestGitCredentialsPreferThePastedToken(t *testing.T) {
	auth := New().GitCredentials(provider.Credentials{Secret: "gho_oauth", GitSecret: "ghp_pat"})

	if auth.Username != "x-access-token" {
		t.Errorf("username = %q, want the placeholder, which a PAT accepts too", auth.Username)
	}
	if auth.Secret != "ghp_pat" {
		t.Errorf("secret = %q, want the pasted token in preference to the OAuth one", auth.Secret)
	}

	// And without one, nothing moves: this is what every account has today.
	auth = New().GitCredentials(provider.Credentials{Secret: "gho_oauth"})
	if auth.Secret != "gho_oauth" {
		t.Errorf("secret = %q, want the account's own credential when none was pasted", auth.Secret)
	}
}
