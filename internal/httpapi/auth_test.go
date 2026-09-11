package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/gitops"
	"github.com/andrea/hexagon/internal/provider"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
)

// fakeGitHub stands in for api.github.com when identifying a token's owner.
type fakeGitHub struct {
	user *github.User
	err  error
}

func (f *fakeGitHub) CurrentUser(context.Context, string) (*github.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}

type testEnv struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	store  *store.Store
	auth   *auth.Service
	github *fakeGitHub
	// ghRepos and bbRepos are the two sources of repositories; repos is the
	// real lister over them, so a test exercises the merging and the caching
	// rather than a stub of them.
	ghRepos *fakeProvider
	bbRepos *fakeProvider
	repos   *provider.Lister
	cfg     *config.Config
	// gate is the login the server reads on every request, so a test can watch
	// the first-time wizard replace it.
	gate *auth.Gate
	// setupPassword is what this server printed for the first-time wizard.
	setupPassword string
	docker        *fakeDocker
	cloner        *fakeCloner
	editor        *fakeEditor
	compose       *fakeCompose
	vscode        *fakeVSCode
	defaultImage  *fakeDefaultImage
	// deps is what the running server was built from, so a test can rebuild it
	// with an optional collaborator left out, and newSessions rebuilds the
	// orchestrator the same way.
	deps        Deps
	newSessions func(session.Compose) *session.Manager
	// workspaces is the root the session manager provisions into.
	workspaces string
}

// fakeDefaultImage stands in for the image Hexagon builds for itself. A test
// that cares sets the state; every other one gets an image that is ready, which
// is what a server that has been up for a minute has.
type fakeDefaultImage struct {
	mu    sync.Mutex
	state claudex.State
}

func (f *fakeDefaultImage) State() claudex.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeDefaultImage) set(state claudex.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
}

// fakeVSCode stands in for a code-server release: Ensure just reports a fixed
// directory, and records how many times it was asked, which is what the "an
// existing install is never touched again" rule is about at this level.
type fakeVSCode struct {
	mu      sync.Mutex
	dir     string
	err     error
	ensured int
}

func (f *fakeVSCode) Ensure(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured++
	if f.err != nil {
		return "", f.err
	}
	return f.dir, nil
}

// fakeCloner stands in for git. It records what it was asked to clone and
// creates the destination, which is all the manager cares about.
type fakeCloner struct {
	mu   sync.Mutex
	err  error
	seen []gitops.Options
}

func (f *fakeCloner) Clone(_ context.Context, opts gitops.Options) error {
	f.mu.Lock()
	f.seen = append(f.seen, opts)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(opts.Dest, ".git"), 0o700)
}

func (f *fakeCloner) clones() []gitops.Options {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gitops.Options(nil), f.seen...)
}

// newTestEnv builds a server with the GitHub login wired to stubs, plus a
// client that keeps cookies and does not follow redirects, so each hop of the
// login can be inspected.
func newTestEnv(t *testing.T, allowedUsers ...string) *testEnv {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	key := make([]byte, 32)
	cipher, err := auth.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}

	tokenStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"gho_token","token_type":"bearer"}`)
	}))
	t.Cleanup(tokenStub.Close)

	workspaces := filepath.Join(t.TempDir(), "workspaces")
	// A path under the test's own temp dir: it need not exist for a browser
	// login to start (the login container only needs somewhere to write), and
	// its absence keeps the mount out of every session that does not exercise
	// it on purpose. Both the httpapi and the session config need it: one
	// reports canLogin, the other actually starts the container.
	claudeCredentials := filepath.Join(workspaces, "claude", ".credentials.json")
	claudeAccountsDir := filepath.Join(workspaces, "claude-accounts")

	// DataDir and WorkspaceRoot back real, existing directories: the Stats page
	// statfs's them, and a path that is not there yet would fail the read
	// rather than exercise it.
	dataDir := t.TempDir()
	if err := os.MkdirAll(workspaces, 0o700); err != nil {
		t.Fatalf("create workspaces dir: %v", err)
	}

	cfg := &config.Config{
		Addr: "127.0.0.1:0", PublicURL: "http://127.0.0.1:8080", ClaudeCredentials: claudeCredentials,
		ClaudeAccountsDir: claudeAccountsDir,
		DataDir:           dataDir,
		WorkspaceRoot:     workspaces,
		// Where the first-time wizard would write. Nothing is there until a
		// test puts it there.
		ConfigPath: filepath.Join(t.TempDir(), "config.json"),
		// The production defaults, so a test that is not about the limits does
		// not have to know they exist.
		MaxSessionsPerUser: 20, MaxConcurrentBuilds: 2, PublicRatePerMinute: 60,
	}
	logins := auth.NewService(st, cipher, cfg.PublicURL)
	gh := &fakeGitHub{user: &github.User{Login: "alice", ID: 42, AvatarURL: "https://example.test/a.png"}}
	docker := newFakeDocker()
	ghRepos := newFakeProvider(provider.GitHub, "alice")
	bbRepos := newFakeProvider(provider.Bitbucket, "alice-bb")
	providers := provider.NewRegistry(ghRepos, bbRepos)
	repos := provider.NewLister(providers, logins, time.Minute)
	cloner := &fakeCloner{}
	editor := &fakeEditor{}
	compose := &fakeCompose{docker: docker}
	vscode := &fakeVSCode{dir: "/vscode-release"}
	defaultImage := &fakeDefaultImage{state: claudex.State{Ref: "hexagon-default:test", Ready: true}}

	// A closure rather than a value, so a test can rebuild the orchestrator
	// without `docker compose`: the manager captures its collaborators, and
	// leaving one out of Deps alone would not reach it.
	newSessions := func(compose session.Compose) *session.Manager {
		return session.NewManager(st, docker, cloner, auth.NewGitCredentialSource(logins, providers), vscode, compose, session.Config{
			WorkspaceRoot:     workspaces,
			ClaudeCredentials: claudeCredentials,
			AnthropicAPIKey:   cfg.AnthropicAPIKey,
			ClaudeLoginDir:    filepath.Join(workspaces, "claude-login"),
			ClaudeAccountsDir: claudeAccountsDir,
			GitUserName:       "Hexagon User",
			GitUserEmail:      "user@example.test",
			ContainerUser:     "1000:1000",
		}, slog.New(slog.DiscardHandler))
	}
	sessions := newSessions(compose)

	oauth, err := auth.NewOAuth(auth.OAuthConfig{
		ClientID:     "client",
		ClientSecret: "secret",
		PublicURL:    cfg.PublicURL,
		TokenURL:     tokenStub.URL,
	})
	if err != nil {
		t.Fatalf("new oauth: %v", err)
	}
	allowlist, err := auth.NewAllowlist(allowedUsers, st, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new allowlist: %v", err)
	}
	gate := auth.NewGate(oauth, allowlist)

	// Every test server starts with the first-time wizard open, because no
	// user has signed in yet. That is the production state too, and it is what
	// makes "every protected route still refuses" testable.
	setup, setupPassword, err := auth.NewSetup()
	if err != nil {
		t.Fatalf("new setup: %v", err)
	}

	deps := Deps{
		Config:         cfg,
		Store:          st,
		Auth:           logins,
		Gate:           gate,
		Setup:          setup,
		GitHub:         gh,
		Providers:      providers,
		Repos:          repos,
		Docker:         docker,
		Sessions:       sessions,
		BaseDockerfile: "FROM scratch\n",
		BaseCompose:    "services: {}\n",
		Editor:         editor,
		DefaultImage:   defaultImage,
		Compose:        compose,
		Frontend:       fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}},
		Log:            slog.New(slog.DiscardHandler),
		// The level the settings page moves. A real one, so a test can read
		// what a save did to it.
		LogLevel: new(slog.LevelVar),
	}

	server := httptest.NewServer(New(deps))
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("new cookie jar: %v", err)
	}
	return &testEnv{
		t:             t,
		server:        server,
		store:         st,
		auth:          logins,
		github:        gh,
		ghRepos:       ghRepos,
		bbRepos:       bbRepos,
		repos:         repos,
		cfg:           cfg,
		gate:          gate,
		setupPassword: setupPassword,
		docker:        docker,
		cloner:        cloner,
		editor:        editor,
		compose:       compose,
		defaultImage:  defaultImage,
		vscode:        vscode,
		deps:          deps,
		newSessions:   newSessions,

		workspaces: workspaces,
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// without rebuilds the server with one optional collaborator left out, so a
// test can see what a server that does not have it serves. The listener is the
// same one, so the client keeps its cookies and its origin.
func (e *testEnv) without(change func(*Deps)) {
	e.t.Helper()
	change(&e.deps)
	e.server.Config.Handler = New(e.deps)
}

// withoutEditor is a server with no Claude Code binary; withoutCompose one with
// no `docker compose`.
func (e *testEnv) withoutEditor() { e.without(func(d *Deps) { d.Editor = nil }) }

func (e *testEnv) withoutCompose() {
	e.without(func(d *Deps) {
		d.Compose = nil
		d.Sessions = e.newSessions(nil)
	})
}

// signIn completes a login so the client holds a session cookie.
// userID is the database id of the signed-in user.
func (e *testEnv) userID() string {
	e.t.Helper()
	user, err := e.store.UserByGitHubID(e.t.Context(), 42)
	if err != nil {
		e.t.Fatalf("look up the signed-in user: %v", err)
	}
	return user.ID
}

func (e *testEnv) signIn() {
	e.t.Helper()
	state := e.startLogin()
	resp := e.do(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape(state), nil)
	if resp.StatusCode != http.StatusFound {
		e.t.Fatalf("sign in: callback status = %d", resp.StatusCode)
	}
	if e.sessionCookie() == nil {
		e.t.Fatal("sign in did not produce a session")
	}
}

func (e *testEnv) do(method, path string, headers map[string]string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, nil)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	// What a browser puts on every request to its own origin, and what the
	// guard on mutating calls demands. A test that is about the header itself
	// overrides it through headers.
	req.Header.Set("Origin", testOrigin)
	for k, v := range headers {
		// An empty value means "send this request without the header", which is
		// the only way to ask for one the default above already set.
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// startLogin performs the redirect to GitHub and returns the state parameter,
// leaving the matching state cookie in the client's jar.
func (e *testEnv) startLogin() string {
	e.t.Helper()
	resp := e.do(http.MethodGet, "/api/auth/login", nil)
	if resp.StatusCode != http.StatusFound {
		e.t.Fatalf("login status = %d, want 302", resp.StatusCode)
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		e.t.Fatalf("parse redirect: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		e.t.Fatal("login redirect carries no state")
	}
	return state
}

func (e *testEnv) sessionCookie() *http.Cookie {
	e.t.Helper()
	u, _ := url.Parse(e.server.URL)
	for _, c := range e.client.Jar.Cookies(u) {
		if c.Name == e.auth.CookieName() {
			return c
		}
	}
	return nil
}

func TestLoginFlowIssuesSession(t *testing.T) {
	env := newTestEnv(t, "alice")

	state := env.startLogin()
	resp := env.do(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape(state), nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Errorf("callback redirect = %q, want /", got)
	}
	if env.sessionCookie() == nil {
		t.Fatal("no session cookie was set")
	}

	me := env.do(http.MethodGet, "/api/auth/me", nil)
	if me.StatusCode != http.StatusOK {
		t.Fatalf("/api/auth/me status = %d, want 200", me.StatusCode)
	}
	body, _ := io.ReadAll(me.Body)
	if !strings.Contains(string(body), `"login":"alice"`) {
		t.Errorf("/api/auth/me body = %s", body)
	}

	// The token the login came with is stored sealed, as the GitHub provider
	// account, and must come back out.
	ctx := context.Background()
	user, err := env.store.UserByGitHubID(ctx, 42)
	if err != nil {
		t.Fatalf("look up user: %v", err)
	}
	account, err := env.store.ProviderAccount(ctx, user.ID, string(provider.GitHub))
	if err != nil {
		t.Fatalf("look up the github account: %v", err)
	}
	if strings.Contains(string(account.SecretEnc), "gho_token") {
		t.Error("the GitHub token is stored in the clear")
	}
	credentials, err := env.auth.Credentials(ctx, user.ID, provider.GitHub)
	if err != nil || credentials.Secret != "gho_token" {
		t.Errorf("Credentials = %q, %v; want the original token", credentials.Secret, err)
	}
}

func TestCallbackRejectsMismatchedState(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.startLogin()

	resp := env.do(http.MethodGet, "/api/auth/callback?code=abc&state=not-the-state", nil)
	if got := resp.Header.Get("Location"); got != "/login?error=invalid_state" {
		t.Errorf("redirect = %q, want the invalid_state login page", got)
	}
	if env.sessionCookie() != nil {
		t.Error("a session was issued despite the state mismatch")
	}
}

// A callback without a preceding /api/auth/login has no state cookie, which is
// what a cross-site forgery attempt looks like.
func TestCallbackRejectsMissingState(t *testing.T) {
	env := newTestEnv(t, "alice")

	resp := env.do(http.MethodGet, "/api/auth/callback?code=abc&state=guessed", nil)
	if got := resp.Header.Get("Location"); got != "/login?error=invalid_state" {
		t.Errorf("redirect = %q, want the invalid_state login page", got)
	}
	if env.sessionCookie() != nil {
		t.Error("a session was issued without a state cookie")
	}
}

func TestCallbackRejectsUserOutsideAllowlist(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.github.user = &github.User{Login: "mallory", ID: 99}

	state := env.startLogin()
	resp := env.do(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape(state), nil)
	if got := resp.Header.Get("Location"); got != "/login?error=not_allowed" {
		t.Errorf("redirect = %q, want the not_allowed login page", got)
	}
	if env.sessionCookie() != nil {
		t.Error("a session was issued for a user outside the allowlist")
	}
	if _, err := env.store.UserByGitHubID(context.Background(), 99); err == nil {
		t.Error("a refused login created a user row")
	}
}

func TestProtectedEndpointsRequireASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	for _, path := range []string{"/api/auth/me"} {
		resp := env.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	env := newTestEnv(t, "alice")

	state := env.startLogin()
	env.do(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape(state), nil)
	if env.do(http.MethodGet, "/api/auth/me", nil).StatusCode != http.StatusOK {
		t.Fatal("session did not work before expiry")
	}

	// Expire it the way time would.
	if _, err := env.store.DB().Exec(`UPDATE user_sessions SET expires_at = ?`,
		time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05.000")); err != nil {
		t.Fatalf("expire session: %v", err)
	}

	if got := env.do(http.MethodGet, "/api/auth/me", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("expired session = %d, want 401", got)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	env := newTestEnv(t, "alice")

	state := env.startLogin()
	env.do(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape(state), nil)

	resp := env.do(http.MethodPost, "/api/auth/logout", map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", resp.StatusCode)
	}
	if got := env.do(http.MethodGet, "/api/auth/me", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("after logout /api/auth/me = %d, want 401", got)
	}
}

func TestStateChangingRequestsAreGuarded(t *testing.T) {
	env := newTestEnv(t, "alice")
	state := env.startLogin()
	env.do(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape(state), nil)

	// A cross-site HTML form can post without a JSON content type.
	resp := env.do(http.MethodPost, "/api/auth/logout", nil)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("logout without a JSON content type = %d, want 415", resp.StatusCode)
	}

	// A browser on another origin announces itself.
	resp = env.do(http.MethodPost, "/api/auth/logout", map[string]string{
		"Content-Type": "application/json",
		"Origin":       "http://evil.example",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("logout from a foreign origin = %d, want 403", resp.StatusCode)
	}

	// Neither header. This used to be accepted, on the reasoning that browsers
	// always send Origin where it matters; SameSite=Lax was the whole defence,
	// and it is a browser default rather than something this server enforces.
	resp = env.do(http.MethodPost, "/api/auth/logout", map[string]string{
		"Content-Type": "application/json",
		"Origin":       "",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("logout with no origin signal = %d, want 403", resp.StatusCode)
	}

	if env.sessionCookie() == nil {
		t.Error("the guarded requests ended the session anyway")
	}

	// Sec-Fetch-Site on its own is enough, which is what keeps the rule from
	// depending on a header browsers leave out of a navigation.
	resp = env.do(http.MethodPost, "/api/auth/logout", map[string]string{
		"Content-Type":   "application/json",
		"Origin":         "",
		"Sec-Fetch-Site": "same-origin",
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("logout with Sec-Fetch-Site: same-origin = %d, want 200", resp.StatusCode)
	}
	if env.sessionCookie() != nil {
		t.Error("the accepted logout did not end the session")
	}
}

func TestUnknownAPIRouteDoesNotFallThroughToTheSPA(t *testing.T) {
	env := newTestEnv(t, "alice")

	resp := env.do(http.MethodGet, "/api/nope", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown API route = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("unknown API route content type = %q, want JSON", ct)
	}
}

// A session outlives the decision that admitted it, so admission is re-checked
// on every request. The user is created directly here because the allowlist is
// fixed when the server is built: a session for someone not on it is the state
// that matters, however it came about.
func TestRequestsFromAUserNoLongerAllowedAre401(t *testing.T) {
	env := newTestEnv(t, "alice")

	bob, err := env.store.UpsertUser(context.Background(), &store.User{GitHubLogin: "bob", GitHubID: 7})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	issued := httptest.NewRecorder()
	if err := env.auth.Issue(context.Background(), issued, bob, time.Time{}); err != nil {
		t.Fatalf("issue session: %v", err)
	}
	cookie := issued.Result().Cookies()[0]

	req, err := http.NewRequest(http.MethodGet, env.server.URL+"/api/auth/me", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("get /api/auth/me: %v", err)
	}
	defer resp.Body.Close()

	// 401 and not 403, so the SPA's existing redirect to /login keeps working
	// and the login itself explains why with not_allowed.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}

	// The row is gone too: the check does not have to run again for this
	// browser, and a stolen cookie is worth nothing either.
	var live int
	if err := env.store.DB().QueryRow(
		`SELECT count(*) FROM user_sessions WHERE user_id = ?`, bob.ID).Scan(&live); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if live != 0 {
		t.Errorf("sessions left for a user no longer allowed = %d, want 0", live)
	}
}
