package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/andrea/hexagon/internal/config"
)

// newSettingsEnv builds a signed-in server whose running configuration is the
// one its configuration file resolves to. That is the state a real process
// starts in, and the only one where "nothing is waiting for a restart" is the
// honest answer to begin with.
func newSettingsEnv(t *testing.T) *testEnv {
	t.Helper()
	isolateConfigLoad(t)
	env := newTestEnv(t, "alice")
	env.signIn()

	// The login the test server was wired with, written where it would really
	// have come from. Without it the file resolves to a configuration with no
	// GitHub application in it, which is a server the settings page is right to
	// refuse to save over.
	if err := os.WriteFile(env.cfg.ConfigPath, []byte(
		`{"github": {"clientId": "client", "clientSecret": "secret", "allowedUsers": ["alice"]}}`,
	), 0o600); err != nil {
		t.Fatalf("write the configuration file: %v", err)
	}

	resolved, err := config.Resolve(env.cfg.ConfigPath)
	if err != nil {
		t.Fatalf("resolve the configuration: %v", err)
	}
	path := env.cfg.ConfigPath
	*env.cfg = *resolved
	env.cfg.ConfigPath = path
	return env
}

func (e *testEnv) settings() settingsResponse {
	e.t.Helper()
	var settings settingsResponse
	e.decode(e.do(http.MethodGet, "/api/settings", nil), &settings)
	return settings
}

func TestSettingsNeedASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	for _, call := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPut, `{"publicUrl": "http://example.test"}`},
	} {
		resp := env.sendJSON(call.method, "/api/settings", call.body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s /api/settings = %d, want 401 without a session", call.method, resp.StatusCode)
		}
	}
}

func TestSettingsReportTheRunningServerBesideItsFile(t *testing.T) {
	env := newSettingsEnv(t)

	settings := env.settings()
	if settings.ConfigPath != env.cfg.ConfigPath || !settings.Writable {
		t.Errorf("configPath/writable = %q/%v, want the file this server would write",
			settings.ConfigPath, settings.Writable)
	}
	if settings.RestartRequired {
		t.Error("restartRequired = true on a server running exactly what its file says, want false")
	}
	// The gate is the live truth for the settings a save applies without a
	// restart, and the read-only pair is reported for display.
	if settings.Running.GitHub.ClientID != "client" {
		t.Errorf("running client id = %q, want the one the gate holds", settings.Running.GitHub.ClientID)
	}
	if settings.Running.Addr == "" || settings.Running.SecretKeySource == "" {
		t.Errorf("addr/secretKeySource = %q/%q, want both shown",
			settings.Running.Addr, settings.Running.SecretKeySource)
	}
	if settings.Running.CallbackURL != "http://127.0.0.1:8080/api/auth/callback" {
		t.Errorf("callbackUrl = %q, want the one to register on GitHub", settings.Running.CallbackURL)
	}
}

func TestSettingsNeverSendASecretToTheBrowser(t *testing.T) {
	env := newSettingsEnv(t)

	resp := env.sendJSON(http.MethodPut, "/api/settings", `{
		"github": {"clientSecret": "s3cret-from-the-form"},
		"claude": {"anthropicApiKey": "sk-ant-example"}
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}
	body := env.bodyString(resp)
	if strings.Contains(body, "s3cret-from-the-form") || strings.Contains(body, "sk-ant-example") {
		t.Errorf("a secret came back in the response: %s", body)
	}

	settings := env.settings()
	if !settings.ClientSecretSet || !settings.AnthropicAPIKeySet {
		t.Errorf("clientSecretSet/anthropicApiKeySet = %v/%v, want both reported as present",
			settings.ClientSecretSet, settings.AnthropicAPIKeySet)
	}

	// And an empty field is the form saying it has nothing to offer, not a
	// request to clear what is stored.
	if resp := env.sendJSON(http.MethodPut, "/api/settings", `{"github": {"clientSecret": ""}}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}
	if cfg, err := config.Resolve(env.cfg.ConfigPath); err != nil {
		t.Fatalf("resolve: %v", err)
	} else if cfg.GitHubClientSecret != "s3cret-from-the-form" {
		t.Errorf("client secret = %q, want the stored one kept by an empty field", cfg.GitHubClientSecret)
	}
}

func TestSettingsApplyTheAllowlistWithoutARestart(t *testing.T) {
	env := newSettingsEnv(t)

	resp := env.sendJSON(http.MethodPut, "/api/settings", `{"github": {"allowedUsers": ["alice", " bob ", ""]}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}
	var settings settingsResponse
	env.decode(resp, &settings)
	if strings.Join(settings.Running.GitHub.AllowedUsers, ",") != "alice,bob" {
		t.Errorf("running allowedUsers = %v, want the two usable entries, trimmed", settings.Running.GitHub.AllowedUsers)
	}
	if settings.RestartRequired {
		t.Error("restartRequired = true after changing only settings the gate holds, want false")
	}
	if entries := env.gate.Allowlist().Entries(); strings.Join(entries, ",") != "alice,bob" {
		t.Errorf("gate allowlist = %v, want the saved list in place with no restart", entries)
	}

	// Taking the caller out of it is a different matter, and the whole file has
	// to survive the refusal.
	resp = env.sendJSON(http.MethodPut, "/api/settings", `{"github": {"allowedUsers": ["carol"]}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a save must not sign the caller out with no way back in", resp.StatusCode)
	}
	if entries := env.gate.Allowlist().Entries(); strings.Join(entries, ",") != "alice,bob" {
		t.Errorf("gate allowlist = %v, want the refused list never applied", entries)
	}
	if resp := env.do(http.MethodGet, "/api/auth/me", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/auth/me = %d, want the caller still signed in", resp.StatusCode)
	}
}

func TestSettingsKeepAPublicURLChangeWaitingForARestart(t *testing.T) {
	env := newSettingsEnv(t)
	before := env.gate.OAuth().RedirectURI()

	resp := env.sendJSON(http.MethodPut, "/api/settings", `{"publicUrl": "https://hexagon.example.test"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}
	var settings settingsResponse
	env.decode(resp, &settings)

	if !settings.RestartRequired {
		t.Error("restartRequired = false after changing the public URL, want true")
	}
	if settings.Saved.PublicURL != "https://hexagon.example.test" {
		t.Errorf("saved publicUrl = %q, want the value that was saved", settings.Saved.PublicURL)
	}
	if settings.Running.PublicURL != "http://127.0.0.1:8080" {
		t.Errorf("running publicUrl = %q, want the one this process is still serving", settings.Running.PublicURL)
	}
	// The redirect_uri must not move on its own: the origin check, the cookie
	// Secure flag and the HSTS header were all captured at startup, and a
	// handshake sent to the new origin would come back to a process that
	// rejects it.
	if got := env.gate.OAuth().RedirectURI(); got != before {
		t.Errorf("redirect uri = %q, want %q until the server is restarted", got, before)
	}
}

func TestSettingsRefuseValuesTheNextStartWouldNotLoad(t *testing.T) {
	env := newSettingsEnv(t)

	for name, body := range map[string]string{
		"a public URL that is not an origin": `{"publicUrl": "not a url at all"}`,
		"a public URL with a path":           `{"publicUrl": "https://example.test/hexagon"}`,
		"a limit that bounds nothing":        `{"limits": {"maxSessionsPerUser": -1}}`,
		"no client id left":                  `{"github": {"clientId": ""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := env.sendJSON(http.MethodPut, "/api/settings", body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", resp.StatusCode, env.bodyString(resp))
			}
		})
	}
	after, err := os.ReadFile(env.cfg.ConfigPath)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	if strings.Contains(string(after), "example.test") || strings.Contains(string(after), "maxSessionsPerUser") {
		t.Errorf("the file took a change that was refused: %s", after)
	}
}

func TestSettingsCannotChangeTheAddressOrTheSecretKey(t *testing.T) {
	env := newSettingsEnv(t)

	// Neither is a field of the request, so this is the whole of the check:
	// there is nothing to ignore, and the file gains nothing.
	resp := env.sendJSON(http.MethodPut, "/api/settings", `{
		"addr": "0.0.0.0:9999", "secretKey": "AAAA", "git": {"userName": "Someone"}
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}

	data, err := os.ReadFile(env.cfg.ConfigPath)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse the configuration file: %v", err)
	}
	if _, ok := doc["addr"]; ok {
		t.Error("addr was written, want a setting this API cannot express to stay out of the file")
	}
	if _, ok := doc["secretKey"]; ok {
		t.Error("secretKey was written, want it left to a file edit and a restart")
	}
	if git, _ := doc["git"].(map[string]any); git["userName"] != "Someone" {
		t.Errorf("git = %v, want the settings the request could express saved", doc["git"])
	}
}

func TestSettingsNoticeAFileEditedByHand(t *testing.T) {
	env := newSettingsEnv(t)

	if err := os.WriteFile(env.cfg.ConfigPath, []byte(`{"limits": {"maxConcurrentBuilds": 7}}`), 0o600); err != nil {
		t.Fatalf("write the configuration file: %v", err)
	}

	settings := env.settings()
	if !settings.RestartRequired {
		t.Error("restartRequired = false for a file edited behind the server's back, want true")
	}
	if settings.Saved.Limits.MaxConcurrentBuilds != 7 || settings.Running.Limits.MaxConcurrentBuilds != 2 {
		t.Errorf("maxConcurrentBuilds saved/running = %d/%d, want 7/2",
			settings.Saved.Limits.MaxConcurrentBuilds, settings.Running.Limits.MaxConcurrentBuilds)
	}
}

func TestSettingsNameWhatTheEnvironmentIsSupplying(t *testing.T) {
	env := newSettingsEnv(t)
	t.Setenv("HEXAGON_GIT_USER_NAME", "From The Environment")

	settings := env.settings()
	if !strings.Contains(strings.Join(settings.FromEnvironment, ","), "git.userName") {
		t.Errorf("fromEnvironment = %v, want the setting a variable is supplying named", settings.FromEnvironment)
	}
}
