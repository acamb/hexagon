package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/github"
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
	repos  *fakeRepos
	docker *fakeDocker
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

	cfg := &config.Config{Addr: "127.0.0.1:0", PublicURL: "http://127.0.0.1:8080"}
	sessions := auth.NewService(st, cipher, cfg.PublicURL)
	gh := &fakeGitHub{user: &github.User{Login: "alice", ID: 42, AvatarURL: "https://example.test/a.png"}}
	docker := newFakeDocker()
	repos := &fakeRepos{}

	oauth, err := auth.NewOAuth(auth.OAuthConfig{
		ClientID:     "client",
		ClientSecret: "secret",
		PublicURL:    cfg.PublicURL,
		AllowedUsers: allowedUsers,
		TokenURL:     tokenStub.URL,
	})
	if err != nil {
		t.Fatalf("new oauth: %v", err)
	}

	handler := New(Deps{
		Config:         cfg,
		Store:          st,
		Auth:           sessions,
		OAuth:          oauth,
		GitHub:         gh,
		Repos:          repos,
		Docker:         docker,
		BaseDockerfile: "FROM scratch\n",
		Frontend:       fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}},
		Log:            slog.New(slog.DiscardHandler),
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("new cookie jar: %v", err)
	}
	return &testEnv{
		t:      t,
		server: server,
		store:  st,
		auth:   sessions,
		github: gh,
		repos:  repos,
		docker: docker,
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// signIn completes a login so the client holds a session cookie.
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
	for k, v := range headers {
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
		if c.Name == auth.SessionCookie {
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

	// The GitHub token must be stored sealed, and must come back out.
	user, err := env.store.UserByGitHubID(context.Background(), 42)
	if err != nil {
		t.Fatalf("look up user: %v", err)
	}
	if strings.Contains(string(user.GitHubTokenEnc), "gho_token") {
		t.Error("the GitHub token is stored in the clear")
	}
	token, err := env.auth.GitHubToken(user)
	if err != nil || token != "gho_token" {
		t.Errorf("GitHubToken = %q, %v; want the original token", token, err)
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

	if env.sessionCookie() == nil {
		t.Error("the guarded requests ended the session anyway")
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
