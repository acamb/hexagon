package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// isolateConfigLoad keeps the reload that POST /api/setup performs inside the
// test's own directories. Saving goes back through config.Load, which resolves
// the data directory and the secret key from the environment, and a test must
// not reach for the ones the developer actually uses.
func isolateConfigLoad(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HEXAGON_DATA_DIR", t.TempDir())
	for _, key := range []string{
		"HEXAGON_CONFIG", "HEXAGON_GITHUB_CLIENT_ID", "HEXAGON_GITHUB_CLIENT_SECRET",
		"HEXAGON_ALLOWED_USERS", "HEXAGON_PUBLIC_URL", "HEXAGON_ADDR",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func (e *testEnv) setupStatus() setupStatusResponse {
	e.t.Helper()
	var status setupStatusResponse
	e.decode(e.do(http.MethodGet, "/api/setup", nil), &status)
	return status
}

func TestSetupIsRequiredUntilSomebodyHasSignedIn(t *testing.T) {
	env := newTestEnv(t, "alice")

	status := env.setupStatus()
	if !status.Required {
		t.Fatal("required = false on a server nobody has signed in to, want true")
	}
	if status.ConfigPath == "" || status.CallbackURL != "http://127.0.0.1:8080/api/auth/callback" {
		t.Errorf("status = %+v, want the file to write and the callback to register", status)
	}
	if !status.Writable {
		t.Error("writable = false for a path under the test's own directory, want true")
	}

	env.signIn()

	status = env.setupStatus()
	if status.Required {
		t.Error("required = true after a sign-in, want the wizard closed for good")
	}
	// Closed means closed: the path of a configuration file is a fact about the
	// machine, and this route answers anyone who can reach the port.
	if status.ConfigPath != "" || status.CallbackURL != "" || status.ClientID != "" {
		t.Errorf("status = %+v, want nothing but required", status)
	}
}

func TestSetupWritesTheConfigurationFileAndReconfiguresTheRunningServer(t *testing.T) {
	isolateConfigLoad(t)
	env := newTestEnv(t, "alice")
	// A server the wizard has not run on yet: no login, and nothing that can
	// admit anybody.
	env.gate.Set(nil, nil)

	resp := env.do(http.MethodGet, "/api/auth/login", nil)
	if got := resp.Header.Get("Location"); got != "/login?error=not_configured" {
		t.Fatalf("login redirect = %q, want the sign-in page saying the server is not set up", got)
	}

	resp = env.postJSON("/api/setup", `{
		"password": "`+env.setupPassword+`",
		"clientId": "Iv23liExample",
		"clientSecret": "s3cret",
		"allowedUsers": ["1234", " bob ", ""]
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}
	var status setupStatusResponse
	env.decode(resp, &status)
	if status.ClientID != "Iv23liExample" {
		t.Errorf("clientId = %q, want the value that was saved", status.ClientID)
	}
	if strings.Join(status.AllowedUsers, ",") != "1234,bob" {
		t.Errorf("allowedUsers = %v, want the two usable entries, trimmed", status.AllowedUsers)
	}

	// The file is the record, and it is written with the permissions the loader
	// will demand of it at the next start.
	info, err := os.Stat(env.cfg.ConfigPath)
	if err != nil {
		t.Fatalf("stat the configuration file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600: it holds the OAuth client secret", perm)
	}
	data, err := os.ReadFile(env.cfg.ConfigPath)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	doc := struct {
		GitHub struct {
			ClientID     string   `json:"clientId"`
			ClientSecret string   `json:"clientSecret"`
			AllowedUsers []string `json:"allowedUsers"`
		} `json:"github"`
	}{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse the configuration file: %v", err)
	}
	if doc.GitHub.ClientSecret != "s3cret" || doc.GitHub.ClientID != "Iv23liExample" {
		t.Errorf("file github = %+v, want what the wizard was given", doc.GitHub)
	}

	// And the running server picked it up, with no restart.
	if !env.gate.Configured() {
		t.Fatal("the gate is still empty after a successful setup, want the login in place")
	}
	resp = env.do(http.MethodGet, "/api/auth/login", nil)
	if got := resp.Header.Get("Location"); !strings.HasPrefix(got, "https://github.com/login/oauth/authorize?") {
		t.Errorf("login redirect = %q, want the GitHub handshake", got)
	}
	if !strings.Contains(resp.Header.Get("Location"), "client_id=Iv23liExample") {
		t.Errorf("login redirect = %q, want the client id the wizard saved", resp.Header.Get("Location"))
	}
}

func TestSetupRefusesAWrongPassword(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.gate.Set(nil, nil)

	resp := env.postJSON("/api/setup", `{
		"password": "AAAA-BBBB-CCCC-DDDD",
		"clientId": "id", "clientSecret": "secret", "allowedUsers": ["alice"]
	}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if _, err := os.Stat(env.cfg.ConfigPath); !os.IsNotExist(err) {
		t.Error("a configuration file was written by a request that did not know the password")
	}
	if env.gate.Configured() {
		t.Error("the gate was configured by a request that did not know the password")
	}
}

func TestSetupRefusesSettingsItCannotBuildALoginFrom(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.gate.Set(nil, nil)

	for name, body := range map[string]string{
		"no client id":     `{"clientId": "", "clientSecret": "secret", "allowedUsers": ["alice"]}`,
		"no client secret": `{"clientId": "id", "clientSecret": "  ", "allowedUsers": ["alice"]}`,
		"nobody allowed":   `{"clientId": "id", "clientSecret": "secret", "allowedUsers": [" "]}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := env.postJSON("/api/setup", `{"password": "`+env.setupPassword+`", `+body[1:])
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
	if _, err := os.Stat(env.cfg.ConfigPath); !os.IsNotExist(err) {
		t.Error("a configuration file was written from settings that were refused")
	}
}

func TestSetupIsClosedOnceSomebodyHasSignedIn(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	resp := env.postJSON("/api/setup", `{
		"password": "`+env.setupPassword+`",
		"clientId": "id", "clientSecret": "secret", "allowedUsers": ["alice"]
	}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: the wizard closes at the first sign-in", resp.StatusCode)
	}
}

func TestEveryProtectedRouteIsRefusedWhileTheServerIsUnconfigured(t *testing.T) {
	env := newTestEnv(t, "alice")
	// A real session, issued while the server was configured, and then a server
	// with no allowlist to check it against.
	env.signIn()
	env.gate.Set(nil, nil)

	for _, path := range []string{"/api/auth/me", "/api/sessions", "/api/images", "/api/repos", "/api/accounts", "/api/claude"} {
		resp := env.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401 while there is nobody the server can admit", path, resp.StatusCode)
		}
	}
}
