package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/andrea/hexagon/internal/provider"
	"github.com/andrea/hexagon/internal/store"
)

func TestAccountsListsEveryProviderConnectedOrNot(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var accounts []accountResponse
	env.decode(env.do(http.MethodGet, "/api/accounts", nil), &accounts)
	if len(accounts) != 2 {
		t.Fatalf("got %d providers, want every one this build knows", len(accounts))
	}

	byKind := map[provider.Kind]accountResponse{}
	for _, account := range accounts {
		byKind[account.Provider] = account
	}
	// GitHub arrives with the login, and cannot be forgotten: it is how the
	// user got here.
	github := byKind[provider.GitHub]
	if !github.Connected || github.Account != "alice" {
		t.Errorf("github = %+v, want it connected from the sign-in", github)
	}
	if github.Removable {
		t.Error("the account used to sign in is offered for removal")
	}
	// Bitbucket is offered but not connected, so the page can show the form.
	if bitbucket := byKind[provider.Bitbucket]; bitbucket.Connected || !bitbucket.Removable {
		t.Errorf("bitbucket = %+v, want it offered and removable", bitbucket)
	}
}

func TestConnectAccountVerifiesBeforeStoring(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.bbRepos.fail(fmt.Errorf("nope: %w", provider.ErrUnauthorized))

	resp := env.sendJSON(http.MethodPut, "/api/accounts/bitbucket",
		`{"identity":"alice@example.test","secret":"wrong"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for credentials the provider rejected", resp.StatusCode)
	}

	// Nothing was stored: an account that lists nothing, with no explanation,
	// is worse than a form that failed.
	if _, err := env.store.ProviderAccount(t.Context(), env.userID(), "bitbucket"); err == nil {
		t.Error("the rejected credentials were stored anyway")
	}
}

func TestConnectAccountStoresTheSecretSealed(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var connected accountResponse
	env.decode(env.sendJSON(http.MethodPut, "/api/accounts/bitbucket",
		`{"identity":"alice@example.test","secret":"atlassian-token"}`), &connected)
	if !connected.Connected || connected.Account != "alice-bb" {
		t.Errorf("connected = %+v, want the account the provider reported", connected)
	}

	account, err := env.store.ProviderAccount(t.Context(), env.userID(), "bitbucket")
	if err != nil {
		t.Fatalf("the account was not stored: %v", err)
	}
	if strings.Contains(string(account.SecretEnc), "atlassian-token") {
		t.Error("the API token is stored in the clear")
	}
	// The email is stored beside it because the API, unlike git, is addressed
	// with it on every call.
	if account.Identity != "alice@example.test" {
		t.Errorf("identity = %q, want the email", account.Identity)
	}
}

func TestConnectAccountNeedsBothHalves(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	for _, body := range []string{
		`{"identity":"alice@example.test","secret":"  "}`,
		`{"identity":"  ","secret":"atlassian-token"}`,
	} {
		if resp := env.sendJSON(http.MethodPut, "/api/accounts/bitbucket", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d for %s, want 400", resp.StatusCode, body)
		}
	}
}

func TestGitHubAccountIsNeitherConnectedNorDisconnectedByHand(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	if resp := env.sendJSON(http.MethodPut, "/api/accounts/github", `{"secret":"ghp_x"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("connect status = %d, want 409: it comes from signing in", resp.StatusCode)
	}
	if resp := env.do(http.MethodDelete, "/api/accounts/github", jsonHeader()); resp.StatusCode != http.StatusConflict {
		t.Errorf("disconnect status = %d, want 409", resp.StatusCode)
	}
	if _, err := env.store.ProviderAccount(t.Context(), env.userID(), "github"); err != nil {
		t.Errorf("the sign-in account was affected: %v", err)
	}
}

func TestDisconnectAccountForgetsIt(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.connectBitbucket("alice@example.test", "atlassian-token")

	if resp := env.do(http.MethodDelete, "/api/accounts/bitbucket", jsonHeader()); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if _, err := env.store.ProviderAccount(t.Context(), env.userID(), "bitbucket"); err == nil {
		t.Error("the account is still connected")
	}
	// Disconnecting twice is a 404, not a silent success.
	if resp := env.do(http.MethodDelete, "/api/accounts/bitbucket", jsonHeader()); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second disconnect = %d, want 404", resp.StatusCode)
	}
}

func TestAccountEndpointsRefuseUnknownProviders(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	if resp := env.sendJSON(http.MethodPut, "/api/accounts/gitlab", `{"secret":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("connect status = %d, want 404", resp.StatusCode)
	}
	if resp := env.do(http.MethodDelete, "/api/accounts/gitlab", jsonHeader()); resp.StatusCode != http.StatusNotFound {
		t.Errorf("disconnect status = %d, want 404", resp.StatusCode)
	}
}

func TestAccountEndpointsRequireASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	if got := env.do(http.MethodGet, "/api/accounts", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
	if got := env.sendJSON(http.MethodPut, "/api/accounts/bitbucket", `{"secret":"x"}`).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}

// The point of the whole change: a session from a Bitbucket repository clones
// with Bitbucket's credentials and carries them into its container.
func TestSessionFromABitbucketRepository(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.connectBitbucket("alice@example.test", "atlassian-token")
	image := env.readyImage("base")
	env.bbRepos.offer("acme/gadgets", "develop")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"provider":"bitbucket","repoFullName":"acme/gadgets","imageId":%q}`, image.ID)), &created)
	if created.Provider != "bitbucket" {
		t.Errorf("session provider = %q, want bitbucket", created.Provider)
	}
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	clones := env.cloner.clones()
	if len(clones) != 1 {
		t.Fatalf("made %d clones, want 1", len(clones))
	}
	clone := clones[0]
	if clone.CloneURL != "https://bitbucket.test/acme/gadgets.git" || clone.Branch != "develop" {
		t.Errorf("clone = %+v", clone)
	}
	// Bitbucket's git endpoint takes a placeholder username, not the email the
	// API takes: the provider decides, not the caller.
	if clone.Username != "x-bitbucket-token" || clone.Token != "atlassian-token" {
		t.Errorf("clone credentials = %q / %q, want Bitbucket's", clone.Username, clone.Token)
	}

	env2 := env.containerEnv()
	if env2["HEXAGON_GIT_USERNAME"] != "x-bitbucket-token" || env2["HEXAGON_GIT_PASSWORD"] != "atlassian-token" {
		t.Errorf("container git credentials = %v", env2)
	}
	// No GITHUB_TOKEN in a Bitbucket session: it would be a GitHub credential
	// handed to a container that has no business with one.
	if _, ok := env2["GITHUB_TOKEN"]; ok {
		t.Error("a Bitbucket session carries a GitHub token")
	}

	// And the container is taught to answer git's prompt with them.
	bootstrap := env.bootstrapScript(0)
	if !strings.Contains(bootstrap, "credential.helper") {
		t.Errorf("the bootstrap does not install a credential helper: %s", bootstrap)
	}
	if strings.Contains(bootstrap, "atlassian-token") {
		t.Error("the bootstrap wrote the secret into the container's git config")
	}
}

// jsonHeader is what the origin guard wants on a mutating request.
func jsonHeader() map[string]string {
	return map[string]string{"Content-Type": "application/json"}
}

func TestSetGitTokenVerifiesThenStoresItSealed(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var updated accountResponse
	env.decode(env.sendJSON(http.MethodPut, "/api/accounts/github/git-token",
		`{"secret":"ghp_personal"}`), &updated)
	if !updated.GitTokenSet {
		t.Errorf("account = %+v, want it reporting a token for git", updated)
	}

	account, err := env.store.ProviderAccount(t.Context(), env.userID(), "github")
	if err != nil {
		t.Fatalf("read the account back: %v", err)
	}
	if strings.Contains(string(account.GitSecretEnc), "ghp_personal") {
		t.Error("the personal access token is stored in the clear")
	}
	// The credential the user signed in with is untouched: it is what lists
	// repositories, and the two secrets answer different questions.
	if len(account.SecretEnc) == 0 {
		t.Error("storing a token for git overwrote the credential from signing in")
	}

	// And it comes back on the listing, as a boolean and never as the value.
	var accounts []accountResponse
	env.decode(env.do(http.MethodGet, "/api/accounts", nil), &accounts)
	for _, a := range accounts {
		if a.Provider == provider.GitHub && !a.GitTokenSet {
			t.Errorf("github = %+v, want gitTokenSet after storing one", a)
		}
	}
}

func TestSetGitTokenRefusesOneTheProviderRejects(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.ghRepos.fail(fmt.Errorf("bad credentials: %w", provider.ErrUnauthorized))

	resp := env.sendJSON(http.MethodPut, "/api/accounts/github/git-token", `{"secret":"nope"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a token the provider rejected", resp.StatusCode)
	}

	// A token that does not work fails inside a container hours later, so it is
	// never stored on the strength of having been typed.
	account, err := env.store.ProviderAccount(t.Context(), env.userID(), "github")
	if err != nil {
		t.Fatalf("read the account back: %v", err)
	}
	if account.GitSecretEnc != nil {
		t.Error("the rejected token was stored anyway")
	}
}

// A personal access token identifies an account of its own, and it does not
// have to be this one. Storing somebody else's would produce a session pushing
// commits under their name.
func TestSetGitTokenRefusesAnotherAccountsToken(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.ghRepos.identify("bob")

	resp := env.sendJSON(http.MethodPut, "/api/accounts/github/git-token", `{"secret":"ghp_bob"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a token belonging to another account", resp.StatusCode)
	}

	account, err := env.store.ProviderAccount(t.Context(), env.userID(), "github")
	if err != nil {
		t.Fatalf("read the account back: %v", err)
	}
	if account.GitSecretEnc != nil {
		t.Error("another account's token was stored")
	}
}

func TestClearGitTokenLeavesTheAccountConnected(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.decode(env.sendJSON(http.MethodPut, "/api/accounts/github/git-token",
		`{"secret":"ghp_personal"}`), &accountResponse{})

	resp := env.do(http.MethodDelete, "/api/accounts/github/git-token",
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	account, err := env.store.ProviderAccount(t.Context(), env.userID(), "github")
	if err != nil {
		t.Fatalf("removing the token disconnected the account: %v", err)
	}
	if account.GitSecretEnc != nil {
		t.Errorf("git secret = %q, want none after removing it", account.GitSecretEnc)
	}
}

// Signing in again refreshes the OAuth token, and it must not take the pasted
// one with it: the whole feature is a credential that outlives a login.
func TestSigningInAgainKeepsTheGitToken(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.decode(env.sendJSON(http.MethodPut, "/api/accounts/github/git-token",
		`{"secret":"ghp_personal"}`), &accountResponse{})

	env.signIn()

	account, err := env.store.ProviderAccount(t.Context(), env.userID(), "github")
	if err != nil {
		t.Fatalf("read the account back: %v", err)
	}
	if account.GitSecretEnc == nil {
		t.Error("signing in again threw away the token stored for git")
	}
}
