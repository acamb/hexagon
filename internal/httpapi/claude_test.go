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

// dialClaudeLogin opens the claude login WebSocket with the session cookie the
// client already holds.
func (e *testEnv) dialClaudeLogin(imageID, origin string) (*websocket.Conn, *http.Response, error) {
	e.t.Helper()

	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	// An empty id is not an id that fails to match: it is a request that names
	// no image, which is what asks for Hexagon's own.
	target := strings.Replace(e.server.URL, "http://", "ws://", 1) + "/api/claude/login/terminal"
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
	if status.Credential != nil {
		t.Errorf("credential = %+v, want none stored yet", status.Credential)
	}
	if status.Effective != "none" {
		t.Errorf("effective = %q, want none", status.Effective)
	}
	if !status.CanLogin {
		t.Error("canLogin = false, want true: the test config names a credentials path")
	}
	if !status.CanVerify {
		t.Error("canVerify = false, want true: the test config has a fake editor")
	}

	resp := env.sendJSON(http.MethodPut, "/api/claude/credential", `{"kind":"api_key","secret":"sk-ant-secret"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set credential status = %d, want 200", resp.StatusCode)
	}
	body := env.bodyString(resp)
	if strings.Contains(body, "sk-ant-secret") {
		t.Errorf("the response body carried the secret: %s", body)
	}

	env.decode(env.do(http.MethodGet, "/api/claude", nil), &status)
	if status.Credential == nil || status.Credential.Kind != claudex.KindAPIKey {
		t.Errorf("credential = %+v, want a stored api_key", status.Credential)
	}
	if status.Effective != "credential" {
		t.Errorf("effective = %q, want credential", status.Effective)
	}

	if len(env.editor.checked) != 1 || env.editor.checked[0].Secret != "sk-ant-secret" {
		t.Errorf("checked = %+v, want the credential to have been verified", env.editor.checked)
	}
}

func TestSetClaudeCredentialRejectsBadInput(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	cases := []struct {
		name string
		body string
	}{
		{"unknown kind", `{"kind":"bearer","secret":"x"}`},
		{"empty secret", `{"kind":"api_key","secret":""}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := env.sendJSON(http.MethodPut, "/api/claude/credential", c.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestSetClaudeCredentialSurfacesTheCLIsOwnRejection(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.editor.checkErr = fmt.Errorf("claude code failed: not logged in")

	resp := env.sendJSON(http.MethodPut, "/api/claude/credential", `{"kind":"api_key","secret":"bad"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body := env.bodyString(resp)
	if !strings.Contains(body, "not logged in") {
		t.Errorf("body = %q, want the CLI's own message", body)
	}

	// A rejected credential must not be stored.
	var status claudeStatusResponse
	env.decode(env.do(http.MethodGet, "/api/claude", nil), &status)
	if status.Credential != nil {
		t.Errorf("credential = %+v, want none: the check failed", status.Credential)
	}
}

func TestForgetClaudeCredentialThenNotFound(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.sendJSON(http.MethodPut, "/api/claude/credential", `{"kind":"api_key","secret":"sk-ant-secret"}`)

	if got := env.do(http.MethodDelete, "/api/claude/credential", map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusNoContent {
		t.Errorf("first delete = %d, want 204", got)
	}
	if got := env.do(http.MethodDelete, "/api/claude/credential", map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", got)
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

// The editor authenticates the way a session container does, and for the same
// reason: a user who configured Claude once should not find that half of Hexagon
// signed in and the other half not.
func TestAskingClaudeUsesTheSameCredentialASessionWould(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	// Nothing pasted and no key configured: the credential is empty, which
	// leaves the login on this machine — the file both a session and the editor
	// fall back to.
	env.postJSON("/api/images/source", `{"kind":"dockerfile","content":"FROM busybox","instruction":"add curl"}`)

	// A key in the server's configuration is the next thing a session would
	// use, so it is what the editor gets too.
	env.cfg.AnthropicAPIKey = "sk-from-the-configuration"
	env.postJSON("/api/images/source", `{"kind":"dockerfile","content":"FROM busybox","instruction":"add curl"}`)

	// And a pasted credential outranks both.
	env.editor.mu.Lock()
	env.editor.checkErr = nil
	env.editor.mu.Unlock()
	env.sendJSON(http.MethodPut, "/api/claude/credential", `{"kind":"api_key","secret":"sk-pasted"}`)
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
	if env.editor.edited[2].Secret != "sk-pasted" {
		t.Errorf("secret = %q, want the pasted credential to outrank the configured key",
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
	if got := env.sendJSON(http.MethodPut, "/api/claude/credential", `{"kind":"api_key","secret":"x"}`).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("PUT /api/claude/credential = %d, want 401", got)
	}
	if got := env.do(http.MethodDelete, "/api/claude/credential", map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("DELETE /api/claude/credential = %d, want 401", got)
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

func TestClaudeCredentialAffectsSessionsCreatedAfterwards(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")

	// No credential stored yet: nothing to fall back to either, since the test
	// config leaves AnthropicAPIKey empty.
	env.postJSON("/api/sessions", fmt.Sprintf(`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID))

	env.sendJSON(http.MethodPut, "/api/claude/credential", `{"kind":"oauth_token","secret":"oauth-secret"}`)

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
		t.Error("no session container carried the stored claude credential")
	}
}
