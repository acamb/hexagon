package bitbucket

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrea/hexagon/internal/provider"
)

func credentials() provider.Credentials {
	return provider.Credentials{Identity: "alice@example.test", Secret: "api-token"}
}

// bitbucketAPI stands in for api.bitbucket.org after CHANGE-2770: repositories
// exist only inside a workspace, so a listing is one call for the workspaces
// and one per workspace, each paginated by a URL in the body.
func bitbucketAPI(t *testing.T, authorization *string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/user/workspaces":
			fmt.Fprint(w, `{"values":[{"slug":"acme"},{"slug":"other"}]}`)
		case r.URL.Path == "/repositories/acme" && r.URL.Query().Get("page") == "2":
			fmt.Fprint(w, `{"values":[{
				"full_name":"acme/second","is_private":true,"updated_on":"2026-02-02T10:00:00.123456+00:00",
				"mainbranch":{"name":"trunk"},
				"links":{"clone":[{"name":"ssh","href":"git@bitbucket.org:acme/second.git"},
				                  {"name":"https","href":"https://bitbucket.org/acme/second.git"}]}
			}]}`)
		case r.URL.Path == "/repositories/acme":
			fmt.Fprintf(w, `{"next":%q,"values":[{
				"full_name":"acme/first","description":"the first one","updated_on":"2026-03-03T10:00:00.000000+00:00",
				"mainbranch":{"name":"main"},
				"links":{"clone":[{"name":"https","href":"https://bitbucket.org/acme/first.git"},
				                  {"name":"ssh","href":"git@bitbucket.org:acme/first.git"}]}
			}]}`, server.URL+"/repositories/acme?page=2")
		case r.URL.Path == "/repositories/other":
			fmt.Fprint(w, `{"values":[{"full_name":"other/third","mainbranch":{"name":"main"},
				"links":{"clone":[{"name":"https","href":"https://bitbucket.org/other/third.git"}]}}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// The cross-workspace listing is gone, so this walks the replacement: every
// workspace, and every page within each of them.
func TestListReposWalksEveryWorkspace(t *testing.T) {
	var authorization string
	server := bitbucketAPI(t, &authorization)

	repos, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), credentials())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 3 {
		t.Fatalf("got %d repositories, want both pages of one workspace and the other workspace: %+v", len(repos), repos)
	}
	if repos[2].FullName != "other/third" {
		t.Errorf("the second workspace was not listed: %+v", repos)
	}
	if repos[0].FullName != "acme/first" || repos[1].FullName != "acme/second" {
		t.Errorf("repositories = %+v", repos)
	}
	if repos[0].Provider != provider.Bitbucket {
		t.Errorf("provider = %q, want bitbucket", repos[0].Provider)
	}
	if repos[0].DefaultBranch != "main" || repos[0].Description != "the first one" {
		t.Errorf("first repository = %+v", repos[0])
	}
	if !repos[1].Private || repos[1].DefaultBranch != "trunk" {
		t.Errorf("second repository = %+v", repos[1])
	}
	// The clone link is picked by name: the other one is ssh, which Hexagon has
	// no key for, and it is not always second in the list.
	if repos[0].CloneURL != "https://bitbucket.org/acme/first.git" {
		t.Errorf("clone URL = %q, want the https one", repos[0].CloneURL)
	}
	if repos[1].CloneURL != "https://bitbucket.org/acme/second.git" {
		t.Errorf("clone URL = %q, want the https one even when it is not first", repos[1].CloneURL)
	}
	if repos[0].UpdatedAt.IsZero() {
		t.Error("updated_on did not parse")
	}

	// The API is addressed with the Atlassian account email, not the username.
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice@example.test:api-token"))
	if authorization != want {
		t.Errorf("Authorization = %q, want basic auth with the email and the token", authorization)
	}
}

func TestVerifyIdentifiesTheAccount(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/user") {
			fmt.Fprint(w, `{"username":"alice-bb","links":{"avatar":{"href":"https://example.test/a.png"}}}`)
			return
		}
		fmt.Fprint(w, `{"values":[{"slug":"acme"}]}`)
	}))
	defer server.Close()

	account, err := NewWithBaseURL(server.URL).Verify(context.Background(), credentials())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if account.Account != "alice-bb" || account.Kind != provider.Bitbucket {
		t.Errorf("account = %+v", account)
	}
	// The email travels with the account: the API needs it on every later call.
	if account.Identity != "alice@example.test" {
		t.Errorf("identity = %q, want the email it was verified with", account.Identity)
	}
	// Verification goes through the call a listing starts from, because that is
	// what the token will be needed for.
	if !strings.Contains(strings.Join(paths, " "), "workspaces") {
		t.Errorf("verification did not exercise the listing: %v", paths)
	}
}

// A token scoped for repositories and nothing else is refused by the account
// endpoint. That token can list and clone, which is all Hexagon asks of it, so
// connecting has to succeed and simply show the email instead of a username.
func TestVerifySurvivesAnAccountEndpointItCannotRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/user") {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"type":"error","error":{"message":"Your token does not have the required scopes"}}`)
			return
		}
		fmt.Fprint(w, `{"values":[{"slug":"acme"}]}`)
	}))
	defer server.Close()

	account, err := NewWithBaseURL(server.URL).Verify(context.Background(), credentials())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if account.Account != "alice@example.test" {
		t.Errorf("account = %q, want the email as the label", account.Account)
	}
}

// What Bitbucket says about a refusal is the only useful thing to show, so it
// has to survive as far as the user.
func TestRefusalCarriesBitbucketsOwnMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"type":"error","error":{"message":"Your token does not have the required scopes"}}`)
	}))
	defer server.Close()

	_, err := NewWithBaseURL(server.URL).Verify(context.Background(), credentials())
	if !strings.Contains(err.Error(), "Your token does not have the required scopes") {
		t.Errorf("error = %q, want Bitbucket's message", err)
	}
	if strings.Contains(err.Error(), "api-token") {
		t.Errorf("the error leaks the token: %q", err)
	}
}

// A rejected or under-scoped token is not a transient failure, and the caller
// has to tell it apart to ask for a new one.
func TestRejectedCredentialsAreRecognisable(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"message":"Access token expired"}}`)
			}))
			defer server.Close()

			_, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), credentials())
			if !errors.Is(err, provider.ErrUnauthorized) {
				t.Errorf("error = %v, want provider.ErrUnauthorized", err)
			}
		})
	}
}

// git and the API do not take the same pair, which is the whole reason this
// provider has its own package.
func TestGitCredentialsUseThePlaceholderNotTheEmail(t *testing.T) {
	auth := New().GitCredentials(credentials())

	if auth.Username != "x-bitbucket-api-token-auth" {
		t.Errorf("username = %q, want the fixed placeholder", auth.Username)
	}
	if auth.Secret != "api-token" {
		t.Errorf("secret = %q, want the API token", auth.Secret)
	}
}

// The listing walks the workspaces the account belongs to, so it starts at the
// endpoint that names them and asks each workspace in turn.
func TestListReposStartsFromTheWorkspacesEndpoint(t *testing.T) {
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if r.URL.Path == "/user/workspaces" {
			fmt.Fprint(w, `{"values":[{"slug":"acme"}]}`)
			return
		}
		fmt.Fprint(w, `{"values":[{"full_name":"acme/widgets","mainbranch":{"name":"main"},
			"links":{"clone":[{"name":"https","href":"https://bitbucket.org/acme/widgets.git"}]}}]}`)
	}))
	defer server.Close()

	repos, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), credentials())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/widgets" {
		t.Errorf("repositories = %+v", repos)
	}
	if strings.Join(asked, " ") != "/user/workspaces /repositories/acme" {
		t.Errorf("asked %v, want the workspaces first and then each of them", asked)
	}
}

// A refused token has to be recognisable from the first call, which is the one
// the connect form makes.
func TestRefusedCredentialsFailAtTheFirstCall(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"type":"error","error":{"message":"Access denied"}}`)
	}))
	defer server.Close()

	_, err := NewWithBaseURL(server.URL).Verify(context.Background(), credentials())
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Errorf("error = %v, want provider.ErrUnauthorized", err)
	}
	if calls != 1 {
		t.Errorf("made %d calls, want one", calls)
	}
}

// The same preference as GitHub's, so the field means one thing everywhere. No
// Bitbucket account needs it today — what one is connected with is already a
// pasted token — but a secret that was honoured by one provider and ignored by
// the other would be a trap.
func TestGitCredentialsPreferThePastedToken(t *testing.T) {
	auth := New().GitCredentials(provider.Credentials{
		Identity: "alice@example.test", Secret: "api-token", GitSecret: "git-token",
	})

	if auth.Username != "x-bitbucket-api-token-auth" {
		t.Errorf("username = %q, want the fixed placeholder", auth.Username)
	}
	if auth.Secret != "git-token" {
		t.Errorf("secret = %q, want the pasted token in preference to the API one", auth.Secret)
	}
}

// role=member is why a connected account listed nothing: it asks for the
// repositories the account is an explicit member of, which excludes everything
// it reads through a workspace-level or group grant.
func TestWorkspaceListingAsksForEveryRepositoryNotJustExplicitMemberships(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Path == "/user/workspaces" {
			fmt.Fprint(w, `{"values":[{"slug":"acme"}]}`)
			return
		}
		if r.URL.Query().Has("role") {
			// What Bitbucket answers for an account whose access to the
			// workspace is inherited rather than per repository.
			fmt.Fprint(w, `{"values":[]}`)
			return
		}
		fmt.Fprint(w, `{"values":[{"full_name":"acme/widgets","mainbranch":{"name":"main"},
			"links":{"clone":[{"name":"https","href":"https://bitbucket.org/acme/widgets.git"}]}}]}`)
	}))
	defer server.Close()

	repos, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), credentials())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/widgets" {
		t.Fatalf("repositories = %+v, want the workspace's repositories", repos)
	}
	for _, query := range queries {
		if strings.Contains(query, "role=") {
			t.Errorf("query = %q, want no role filter: it hides inherited access", query)
		}
	}
}

// Connecting an account that can see no workspace has to fail at the form. It
// can never list a repository, and accepting it produces an account that shows
// as connected while contributing nothing, with nothing anywhere saying why.
func TestVerifyRefusesATokenThatSeesNoWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"values":[]}`)
	}))
	defer server.Close()

	_, err := NewWithBaseURL(server.URL).Verify(context.Background(), credentials())
	if err == nil {
		t.Fatal("Verify accepted a token that sees no workspace")
	}
	if !strings.Contains(err.Error(), "read:workspace:bitbucket") {
		t.Errorf("error = %v, want the scope it is probably missing", err)
	}
}

// One workspace the token cannot read must not empty the picker of every
// Bitbucket repository the account does have.
func TestListReposKeepsTheWorkspacesItCanRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/workspaces":
			fmt.Fprint(w, `{"values":[{"slug":"acme"},{"slug":"locked"}]}`)
		case "/repositories/locked":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"type":"error","error":{"message":"Access denied"}}`)
		default:
			fmt.Fprint(w, `{"values":[{"full_name":"acme/widgets","mainbranch":{"name":"main"},
				"links":{"clone":[{"name":"https","href":"https://bitbucket.org/acme/widgets.git"}]}}]}`)
		}
	}))
	defer server.Close()

	repos, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), credentials())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/widgets" {
		t.Errorf("repositories = %+v, want the readable workspace's", repos)
	}
}

// Every workspace refusing is a different thing: there is nothing to show and a
// reason to give, so it is an error rather than an empty list.
func TestListReposFailsWhenNoWorkspaceCanBeRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/workspaces" {
			fmt.Fprint(w, `{"values":[{"slug":"acme"}]}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"type":"error","error":{"message":"Access denied"}}`)
	}))
	defer server.Close()

	_, err := NewWithBaseURL(server.URL).ListRepos(context.Background(), credentials())
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Errorf("error = %v, want provider.ErrUnauthorized", err)
	}
}
