package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/store"
	"github.com/coder/websocket"
)

// bodyString reads a response body once, for the assertions that check the
// secret never appears in it.
func (e *testEnv) bodyString(resp *http.Response) string {
	e.t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read response body: %v", err)
	}
	return string(data)
}

// createClaudeAccount adds a pasted account through the API and decodes the
// response.
func (e *testEnv) createClaudeAccount(name, kind, secret string) claudeAccountResponse {
	e.t.Helper()
	resp := e.postJSON("/api/claude/accounts", fmt.Sprintf(`{"name":%q,"kind":%q,"secret":%q}`, name, kind, secret))
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("create claude account status = %d, body = %s", resp.StatusCode, e.bodyString(resp))
	}
	var account claudeAccountResponse
	e.decode(resp, &account)
	return account
}

// dialClaudeLogin opens the machine-wide claude login WebSocket with the
// session cookie the client already holds.
func (e *testEnv) dialClaudeLogin(imageID, origin string) (*websocket.Conn, *http.Response, error) {
	return e.dialLoginPath("/api/claude/login/terminal", imageID, origin)
}

// dialClaudeAccountLogin is dialClaudeLogin for one account.
func (e *testEnv) dialClaudeAccountLogin(accountID, imageID, origin string) (*websocket.Conn, *http.Response, error) {
	return e.dialLoginPath("/api/claude/accounts/"+accountID+"/login/terminal", imageID, origin)
}

func (e *testEnv) dialLoginPath(path, imageID, origin string) (*websocket.Conn, *http.Response, error) {
	e.t.Helper()

	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	// An empty id is not an id that fails to match: it is a request that names
	// no image, which is what asks for Hexagon's own.
	target := strings.Replace(e.server.URL, "http://", "ws://", 1) + path
	if imageID != "" {
		target += "?image=" + url.QueryEscape(imageID)
	}
	return websocket.Dial(context.Background(), target, &websocket.DialOptions{
		HTTPClient: e.client,
		HTTPHeader: header,
	})
}

func TestClaudeStatusReflectsWhatIsStoredAndNeverTheSecret(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var status claudeStatusResponse
	env.decode(env.do(http.MethodGet, "/api/claude", nil), &status)
	if status.Effective != "none" {
		t.Errorf("effective = %q, want none", status.Effective)
	}
	if !status.CanLogin {
		t.Error("canLogin = false, want true: the test config names a credentials path")
	}
	if !status.CanVerify {
		t.Error("canVerify = false, want true: the test config has a fake editor")
	}

	resp := env.postJSON("/api/claude/accounts", `{"name":"work","kind":"api_key","secret":"sk-ant-secret"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create account status = %d, want 201", resp.StatusCode)
	}
	body := env.bodyString(resp)
	if strings.Contains(body, "sk-ant-secret") {
		t.Errorf("the response body carried the secret: %s", body)
	}

	env.decode(env.do(http.MethodGet, "/api/claude", nil), &status)
	if status.Effective != "account" {
		t.Errorf("effective = %q, want account", status.Effective)
	}

	var accounts []claudeAccountResponse
	env.decode(env.do(http.MethodGet, "/api/claude/accounts", nil), &accounts)
	if len(accounts) != 1 || accounts[0].Kind != claudex.KindAPIKey || !accounts[0].IsDefault {
		t.Errorf("accounts = %+v, want a single default api_key account", accounts)
	}
	if strings.Contains(env.bodyString(env.do(http.MethodGet, "/api/claude/accounts", nil)), "sk-ant-secret") {
		t.Error("the account listing carried the secret")
	}

	if len(env.editor.checked) != 1 || env.editor.checked[0].Secret != "sk-ant-secret" {
		t.Errorf("checked = %+v, want the credential to have been verified", env.editor.checked)
	}
}

func TestCreateClaudeAccountRejectsBadInput(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	cases := []struct {
		name string
		body string
	}{
		{"no name", `{"name":"","kind":"api_key","secret":"x"}`},
		{"unknown kind", `{"name":"x","kind":"bearer","secret":"x"}`},
		{"empty secret", `{"name":"x","kind":"api_key","secret":""}`},
		{"login account with a secret", `{"name":"x","kind":"login","secret":"x"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := env.postJSON("/api/claude/accounts", c.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestCreateClaudeAccountSurfacesTheCLIsOwnRejection(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.editor.checkErr = fmt.Errorf("claude code failed: not logged in")

	resp := env.postJSON("/api/claude/accounts", `{"name":"work","kind":"api_key","secret":"bad"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body := env.bodyString(resp)
	if !strings.Contains(body, "not logged in") {
		t.Errorf("body = %q, want the CLI's own message", body)
	}

	// A rejected credential must not be stored.
	var accounts []claudeAccountResponse
	env.decode(env.do(http.MethodGet, "/api/claude/accounts", nil), &accounts)
	if len(accounts) != 0 {
		t.Errorf("accounts = %+v, want none: the check failed", accounts)
	}
}

// The first account created becomes the default without being asked; the
// second does not, until it is made one explicitly.
func TestFirstClaudeAccountBecomesTheDefault(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	first := env.createClaudeAccount("first", claudex.KindAPIKey, "sk-1")
	if !first.IsDefault {
		t.Errorf("first account default = false, want true")
	}
	second := env.createClaudeAccount("second", claudex.KindOAuthToken, "oauth-2")
	if second.IsDefault {
		t.Errorf("second account default = true, want false")
	}

	resp := env.sendJSON(http.MethodPatch, "/api/claude/accounts/"+second.ID, `{"default":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("make default status = %d, want 200", resp.StatusCode)
	}

	var accounts []claudeAccountResponse
	env.decode(env.do(http.MethodGet, "/api/claude/accounts", nil), &accounts)
	for _, a := range accounts {
		if a.ID == first.ID && a.IsDefault {
			t.Error("the old default is still marked default")
		}
		if a.ID == second.ID && !a.IsDefault {
			t.Error("the new default was not applied")
		}
	}
}

func TestUpdateClaudeAccountRenamesAndReplacesTheSecret(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	account := env.createClaudeAccount("work", claudex.KindAPIKey, "sk-1")

	resp := env.sendJSON(http.MethodPatch, "/api/claude/accounts/"+account.ID, `{"name":"renamed"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename status = %d, want 200", resp.StatusCode)
	}
	var renamed claudeAccountResponse
	env.decode(resp, &renamed)
	if renamed.Name != "renamed" {
		t.Errorf("name = %q, want renamed", renamed.Name)
	}

	resp = env.sendJSON(http.MethodPatch, "/api/claude/accounts/"+account.ID, `{"secret":"sk-2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replace secret status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(env.bodyString(resp), "sk-2") {
		t.Error("the response carried the new secret")
	}

	env.editor.mu.Lock()
	last := env.editor.checked[len(env.editor.checked)-1]
	env.editor.mu.Unlock()
	if last.Secret != "sk-2" {
		t.Errorf("checked secret = %q, want the replacement to have been verified", last.Secret)
	}
}

func TestRenameClaudeAccountToAnExistingNameConflicts(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.createClaudeAccount("work", claudex.KindAPIKey, "sk-1")
	other := env.createClaudeAccount("personal", claudex.KindAPIKey, "sk-2")

	resp := env.sendJSON(http.MethodPatch, "/api/claude/accounts/"+other.ID, `{"name":"work"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestDeleteClaudeAccountThenNotFound(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	account := env.createClaudeAccount("work", claudex.KindAPIKey, "sk-ant-secret")

	if got := env.do(http.MethodDelete, "/api/claude/accounts/"+account.ID, map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusNoContent {
		t.Errorf("first delete = %d, want 204", got)
	}
	if got := env.do(http.MethodDelete, "/api/claude/accounts/"+account.ID, map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", got)
	}
}

// A claude account is a resource a session still depends on, exactly as an
// image is: deleting it is refused rather than leaving a container that
// authenticates as an account nothing admits to any more.
func TestDeleteClaudeAccountInUseByASessionConflicts(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	account := env.createClaudeAccount("work", claudex.KindAPIKey, "sk-ant-secret")
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q,"claudeAccountId":%q}`, image.ID, account.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	resp := env.do(http.MethodDelete, "/api/claude/accounts/"+account.ID, map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestCreateSessionRejectsAnUnknownClaudeAccount(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	resp := env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q,"claudeAccountId":"nope"}`, image.ID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// A session created naming an explicit account carries that account's
// credential, whether or not it is the default.
func TestCreateSessionWithAnExplicitClaudeAccount(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.createClaudeAccount("default", claudex.KindAPIKey, "sk-default")
	other := env.createClaudeAccount("other", claudex.KindOAuthToken, "oauth-other")
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q,"claudeAccountId":%q}`, image.ID, other.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	if created.ClaudeAccountID != other.ID {
		t.Errorf("claudeAccountId = %q, want %q", created.ClaudeAccountID, other.ID)
	}
	found := false
	for _, spec := range env.docker.containerSpecs() {
		if contains(spec.Env, "CLAUDE_CODE_OAUTH_TOKEN=oauth-other") {
			found = true
		}
		if contains(spec.Env, "ANTHROPIC_API_KEY=sk-default") {
			t.Error("the default account's credential leaked into a session naming another account")
		}
	}
	if !found {
		t.Error("no session container carried the named account's credential")
	}
}

func TestClaudeLoginTerminalBindsTheCredentialsDirectoryAndCarriesNoCredential(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	conn, _, err := env.dialClaudeLogin(image.ID, testOrigin)
	if err != nil {
		t.Fatalf("dial claude login: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	env.docker.containerSide(t)

	specs := env.docker.containerSpecs()
	var spec dockerx.ContainerSpec
	found := false
	for _, s := range specs {
		if s.Labels[dockerx.LabelRole] == "claude-login" {
			spec, found = s, true
		}
	}
	if !found {
		t.Fatalf("no login container was created; specs = %+v", specs)
	}
	if spec.Labels[dockerx.LabelManaged] != "" {
		t.Error("the login container carries hexagon.managed, so it would be reported as an orphan session container")
	}
	wantBindSuffix := ":" + dockerx.AgentHome + "/.claude"
	boundClaudeDir := false
	for _, b := range spec.Binds {
		if strings.HasSuffix(b, wantBindSuffix) {
			boundClaudeDir = true
		}
	}
	if !boundClaudeDir {
		t.Errorf("binds = %v, want one mounted at %s", spec.Binds, wantBindSuffix)
	}
	for _, e := range spec.Env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") || strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Errorf("login container env carried an anthropic credential: %v", spec.Env)
		}
	}

	requests, _ := env.docker.execs()
	if len(requests) != 1 {
		t.Fatalf("made %d exec requests, want 1", len(requests))
	}
	if got := strings.Join(requests[0].Cmd, " "); !strings.Contains(got, "tmux -u new-session -A -D -s login") {
		t.Errorf("exec command = %q, want the login tmux session", got)
	}
}

func TestClaudeLoginTerminalRefusesAnImageThatIsNotReady(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	// The row is inserted rather than created through the API: the fake pull
	// finishes as soon as it starts, so posting an image and dialling before it
	// goes ready is a race, and the race would decide the test rather than the
	// handler's check.
	_, err := env.store.DB().Exec(`
		INSERT INTO images (id, user_id, name, source_type, dockerfile, registry_ref, image_ref,
			status, build_log, error, created_at)
		VALUES ('img-building', ?, 'building', 'registry', '', 'busybox', '', 'building', '', '',
			'2026-01-01 00:00:00.000')`, env.userID())
	if err != nil {
		t.Fatalf("insert image: %v", err)
	}

	_, resp, err := env.dialClaudeLogin("img-building", testOrigin)
	if err == nil {
		t.Fatal("the handshake succeeded against a non-ready image")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %v, want 409", statusOf(resp))
	}
}

// A fresh install has no images, and the login has to run in one. Naming none
// gets Hexagon's own, which is the difference between a dialog with a way
// forward and one with an empty menu.
func TestClaudeLoginRunsInTheDefaultImageWhenNoneIsNamed(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	conn, _, err := env.dialClaudeLogin("", testOrigin)
	if err != nil {
		t.Fatalf("dial claude login: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	env.docker.containerSide(t)

	for _, spec := range env.docker.containerSpecs() {
		if spec.Labels[dockerx.LabelRole] == "claude-login" {
			if spec.Image != "hexagon-default:test" {
				t.Errorf("image = %q, want the default image", spec.Image)
			}
			return
		}
	}
	t.Fatal("no login container was created")
}

// The image is built once, in the background, and a login that arrives first is
// told to wait rather than being held on a socket for as long as a build.
func TestClaudeLoginSaysTheDefaultImageIsNotReadyYet(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.defaultImage.set(claudex.State{Ref: "hexagon-default:test", Building: true})

	_, resp, err := env.dialClaudeLogin("", testOrigin)
	if err == nil {
		t.Fatal("the handshake succeeded with no image to run in")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %v, want 409", statusOf(resp))
	}

	// And a build that failed says why, rather than "not yet" forever.
	env.defaultImage.set(claudex.State{Ref: "hexagon-default:test", Error: "no space left on device"})
	_, resp, err = env.dialClaudeLogin("", testOrigin)
	if err == nil {
		t.Fatal("the handshake succeeded after a failed build")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %v, want 503", statusOf(resp))
	}
}

// A login account's own browser login writes into its own directory rather
// than the machine-wide one, so two accounts can be signed in to at once.
func TestClaudeAccountLoginTerminalBindsTheAccountsOwnDirectory(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	account := env.createClaudeAccount("subscription", store.ClaudeAccountKindLogin, "")
	image := env.readyImage("base")

	conn, _, err := env.dialClaudeAccountLogin(account.ID, image.ID, testOrigin)
	if err != nil {
		t.Fatalf("dial claude account login: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	env.docker.containerSide(t)

	specs := env.docker.containerSpecs()
	var spec dockerx.ContainerSpec
	found := false
	for _, s := range specs {
		if s.Labels[dockerx.LabelRole] == "claude-login" {
			spec, found = s, true
		}
	}
	if !found {
		t.Fatalf("no login container was created; specs = %+v", specs)
	}
	wantBindSuffix := ":" + dockerx.AgentHome + "/.claude"
	boundOwnDir := false
	for _, b := range spec.Binds {
		if strings.HasPrefix(b, env.cfg.ClaudeAccountsDir+"/"+account.ID) && strings.HasSuffix(b, wantBindSuffix) {
			boundOwnDir = true
		}
	}
	if !boundOwnDir {
		t.Errorf("binds = %v, want the account's own directory mounted at %s", spec.Binds, wantBindSuffix)
	}
}

// Only a login account has a browser login to run.
func TestClaudeAccountLoginTerminalRefusesAPastedAccount(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	account := env.createClaudeAccount("pasted", claudex.KindAPIKey, "sk-1")

	_, resp, err := env.dialClaudeAccountLogin(account.ID, "", testOrigin)
	if err == nil {
		t.Fatal("the handshake succeeded against a pasted account")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %v, want 409", statusOf(resp))
	}
}

// The editor authenticates the way a session container does, and for the same
// reason: a user who configured Claude once should not find that half of Hexagon
// signed in and the other half not.
func TestAskingClaudeUsesTheSameCredentialASessionWould(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	// Nothing configured and no key configured: the credential is empty, which
	// leaves the login on this machine — the file both a session and the editor
	// fall back to.
	env.postJSON("/api/images/source", `{"kind":"dockerfile","content":"FROM busybox","instruction":"add curl"}`)

	// A key in the server's configuration is the next thing a session would
	// use, so it is what the editor gets too. Like the session manager's other
	// configuration snapshots, it takes a restart to change — simulated here by
	// rebuilding the orchestrator, exactly as the settings page's own
	// restartRequired flag says it must.
	env.cfg.AnthropicAPIKey = "sk-from-the-configuration"
	env.without(func(d *Deps) { d.Sessions = env.newSessions(env.compose) })
	env.postJSON("/api/images/source", `{"kind":"dockerfile","content":"FROM busybox","instruction":"add curl"}`)

	// And a claude account outranks both.
	env.createClaudeAccount("work", claudex.KindAPIKey, "sk-account")
	env.postJSON("/api/images/source", `{"kind":"dockerfile","content":"FROM busybox","instruction":"add curl"}`)

	env.editor.mu.Lock()
	defer env.editor.mu.Unlock()
	if len(env.editor.edited) != 3 {
		t.Fatalf("edited = %+v, want three calls", env.editor.edited)
	}
	if env.editor.edited[0].Secret != "" {
		t.Errorf("secret = %q, want none: nothing was configured yet",
			env.editor.edited[0].Secret)
	}
	if env.editor.edited[1].Secret != "sk-from-the-configuration" {
		t.Errorf("secret = %q, want the key the server was configured with",
			env.editor.edited[1].Secret)
	}
	if env.editor.edited[2].Secret != "sk-account" {
		t.Errorf("secret = %q, want the account to outrank the configured key",
			env.editor.edited[2].Secret)
	}
}

// The Accounts page has to be able to say what it is waiting for, and whether
// asking Claude anything on this server goes through a container.
func TestClaudeStatusReportsTheDefaultImageAndWhereTheCLIRuns(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var status claudeStatusResponse
	env.decode(env.do(http.MethodGet, "/api/claude", nil), &status)
	if status.DefaultImage == nil || !status.DefaultImage.Ready {
		t.Errorf("defaultImage = %+v, want the ready image the fake reports", status.DefaultImage)
	}
	if status.InContainer {
		t.Error("inContainer = true, want false: the test server has an editor of its own")
	}
}

func TestStopClaudeLoginRemovesTheContainer(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")

	conn, _, err := env.dialClaudeLogin(image.ID, testOrigin)
	if err != nil {
		t.Fatalf("dial claude login: %v", err)
	}
	env.docker.containerSide(t)
	conn.Close(websocket.StatusNormalClosure, "")

	if got := env.do(http.MethodDelete, "/api/claude/login", map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusNoContent {
		t.Errorf("stop login = %d, want 204", got)
	}
	// Docker resolves either a name or an id; the login container is removed by
	// its deterministic name.
	if !contains(env.docker.removedContainers, "hexagon-claude-login-"+env.userID()) {
		t.Errorf("removed containers = %v, want the login container among them", env.docker.removedContainers)
	}
}

func TestClaudeEndpointsRequireASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	if got := env.do(http.MethodGet, "/api/claude", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("GET /api/claude = %d, want 401", got)
	}
	if got := env.do(http.MethodGet, "/api/claude/accounts", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("GET /api/claude/accounts = %d, want 401", got)
	}
	if got := env.postJSON("/api/claude/accounts", `{"name":"x","kind":"api_key","secret":"x"}`).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("POST /api/claude/accounts = %d, want 401", got)
	}
	if got := env.sendJSON(http.MethodPatch, "/api/claude/accounts/x", `{"name":"y"}`).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("PATCH /api/claude/accounts/x = %d, want 401", got)
	}
	if got := env.do(http.MethodDelete, "/api/claude/accounts/x", map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("DELETE /api/claude/accounts/x = %d, want 401", got)
	}
	if got := env.do(http.MethodDelete, "/api/claude/login", map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("DELETE /api/claude/login = %d, want 401", got)
	}
	_, resp, err := env.dialClaudeLogin("whatever", testOrigin)
	if err == nil {
		t.Fatal("an unauthenticated login handshake succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/claude/login/terminal = %v, want 401", statusOf(resp))
	}
}

// Changing a session's claude account is only safe while it is stopped,
// exactly as changing its published ports is: a container keeps the
// credential it was created with, so honouring a change means rebuilding it.
func TestSessionClaudeAccountIsChangedWhileItIsStopped(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.createClaudeAccount("first", claudex.KindAPIKey, "sk-first")
	second := env.createClaudeAccount("second", claudex.KindOAuthToken, "oauth-second")
	json := map[string]string{"Content-Type": "application/json"}

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(`{"imageId":%q}`, image.ID)), &created)
	running := env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	if running.ClaudeAccountID != "" {
		t.Fatalf("claudeAccountId = %q, want empty: none was named", running.ClaudeAccountID)
	}
	before, _ := env.containerOf(running.ID)

	// While it runs the change is refused, and the container is untouched.
	resp := env.sendJSON(http.MethodPut, "/api/sessions/"+running.ID+"/claude-account",
		fmt.Sprintf(`{"claudeAccountId":%q}`, second.ID))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d for a running session, want 409", resp.StatusCode)
	}

	env.do(http.MethodPost, "/api/sessions/"+running.ID+"/stop", json)

	var changed sessionResponse
	resp = env.sendJSON(http.MethodPut, "/api/sessions/"+running.ID+"/claude-account",
		fmt.Sprintf(`{"claudeAccountId":%q}`, second.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	env.decode(resp, &changed)
	if changed.ClaudeAccountID != second.ID {
		t.Errorf("claudeAccountId = %q, want %q", changed.ClaudeAccountID, second.ID)
	}

	after, spec := env.containerOf(running.ID)
	if after == before {
		t.Fatalf("container %s was reused, want it rebuilt for the new account", before)
	}
	if !contains(spec.Env, "CLAUDE_CODE_OAUTH_TOKEN=oauth-second") {
		t.Errorf("env = %v, want the newly named account's credential", spec.Env)
	}
	if contains(spec.Env, "ANTHROPIC_API_KEY=sk-first") {
		t.Error("the rebuilt container still carried the first account's credential")
	}
}

// A request that names what a session already carries rebuilds nothing:
// throwing a container away for a request that changes nothing would be
// destructive for no reason.
func TestSessionClaudeAccountChangeToTheSameAccountRebuildsNothing(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	account := env.createClaudeAccount("work", claudex.KindAPIKey, "sk-work")
	json := map[string]string{"Content-Type": "application/json"}

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"imageId":%q,"claudeAccountId":%q}`, image.ID, account.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)
	env.do(http.MethodPost, "/api/sessions/"+created.ID+"/stop", json)
	before, _ := env.containerOf(created.ID)

	resp := env.sendJSON(http.MethodPut, "/api/sessions/"+created.ID+"/claude-account",
		fmt.Sprintf(`{"claudeAccountId":%q}`, account.ID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if after, _ := env.containerOf(created.ID); after != before {
		t.Errorf("container %s was rebuilt for a request that changed nothing", after)
	}
}

func TestClaudeAccountAffectsSessionsCreatedAfterwards(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	// No account configured yet: nothing to fall back to either, since the test
	// config leaves AnthropicAPIKey empty.
	env.postJSON("/api/sessions", fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID))

	env.createClaudeAccount("work", claudex.KindOAuthToken, "oauth-secret")

	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	found := false
	for _, spec := range env.docker.containerSpecs() {
		if contains(spec.Env, "CLAUDE_CODE_OAUTH_TOKEN=oauth-secret") {
			found = true
		}
	}
	if !found {
		t.Error("no session container carried the default claude account")
	}
}

// An image prune, here or from a terminal, can take the default image away
// after it was built. The login that discovers it gone is the one that orders
// it built again: the user has no button for an image that is not on their
// Images page, and used to be left with the daemon's "No such image".
func TestClaudeLoginRebuildsTheDefaultImageWhenItIsGone(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.docker.createErr = dockerx.ErrImageNotFound

	_, resp, err := env.dialClaudeLogin("", testOrigin)
	if err == nil {
		t.Fatal("the handshake succeeded with no image to run in")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %v, want 409", statusOf(resp))
	}
	if got := env.defaultImage.invalidations(); got != 1 {
		t.Errorf("invalidations = %d, want 1: a missing default image has to be built again", got)
	}
}
