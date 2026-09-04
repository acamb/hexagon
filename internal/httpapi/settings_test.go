package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/store"
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
	//
	// The git identity is here for the same reason: the session manager was
	// built with one, and it reports the identity it holds as the running
	// value, so a file that omitted it would describe a server waiting for a
	// restart it does not need.
	if err := os.WriteFile(env.cfg.ConfigPath, []byte(
		`{"github": {"clientId": "client", "clientSecret": "secret", "allowedUsers": ["alice"]},`+
			`"git": {"userName": "Hexagon User", "userEmail": "user@example.test"}}`,
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

// The git identity is read only when a session is provisioned, so the manager
// can be handed a new one while the server runs. Before that it was the startup
// snapshot, and a session created right after saving still committed as whoever
// the process had been started as — usually nobody.
func TestSettingsApplyTheGitIdentityWithoutARestart(t *testing.T) {
	env := newSettingsEnv(t)

	resp := env.sendJSON(http.MethodPut, "/api/settings",
		`{"git": {"userName": "Ada Lovelace", "userEmail": "ada@example.test"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}
	var settings settingsResponse
	env.decode(resp, &settings)

	if settings.Running.Git.UserName != "Ada Lovelace" || settings.Running.Git.UserEmail != "ada@example.test" {
		t.Errorf("running git identity = %q <%s>, want the saved one in force now",
			settings.Running.Git.UserName, settings.Running.Git.UserEmail)
	}
	if settings.RestartRequired {
		t.Error("restartRequired = true after changing only the git identity, want false")
	}

	// What the change is actually for: the next session.
	image := env.readyImage("base")
	env.offerRepo("acme/widgets", "main")
	var created sessionResponse
	env.decode(env.postJSON("/api/sessions", fmt.Sprintf(
		`{"repoFullName":"acme/widgets","imageId":%q}`, image.ID)), &created)
	env.waitForSessionStatus(created.ID, store.SessionStatusRunning)

	clones := env.cloner.clones()
	if len(clones) != 1 {
		t.Fatalf("made %d clones, want 1", len(clones))
	}
	if clones[0].UserName != "Ada Lovelace" || clones[0].UserEmail != "ada@example.test" {
		t.Errorf("clone identity = %q <%s>, want the identity saved a moment ago",
			clones[0].UserName, clones[0].UserEmail)
	}
	containerEnv := env.containerEnv()
	if containerEnv["GIT_AUTHOR_NAME"] != "Ada Lovelace" || containerEnv["GIT_COMMITTER_EMAIL"] != "ada@example.test" {
		t.Errorf("container git environment = %q / %q, want the identity saved a moment ago",
			containerEnv["GIT_AUTHOR_NAME"], containerEnv["GIT_COMMITTER_EMAIL"])
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
	// By the keys the refused bodies would have added, not by their values: the
	// file legitimately holds an example.test address of its own.
	if strings.Contains(string(after), "publicUrl") || strings.Contains(string(after), "maxSessionsPerUser") {
		t.Errorf("the file took a change that was refused: %s", after)
	}
}

func TestSettingsCannotChangeTheSecretKey(t *testing.T) {
	env := newSettingsEnv(t)

	// It is not a field of the request, so this is the whole of the check:
	// there is nothing to ignore, and the file gains nothing.
	resp := env.sendJSON(http.MethodPut, "/api/settings", `{
		"secretKey": "AAAA", "git": {"userName": "Someone"}
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}

	doc := env.configDocument(t)
	if _, ok := doc["secretKey"]; ok {
		t.Error("secretKey was written, want it left to a file edit and a restart")
	}
	if git, _ := doc["git"].(map[string]any); git["userName"] != "Someone" {
		t.Errorf("git = %v, want the settings the request could express saved", doc["git"])
	}
}

// The listen address is editable, and it is the one setting whose mistake this
// page cannot undo after a restart, so it is written only once it parses.
func TestSettingsSavesTheListenAddress(t *testing.T) {
	env := newSettingsEnv(t)

	resp := env.sendJSON(http.MethodPut, "/api/settings", `{"addr": "127.0.0.1:9999"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}
	if doc := env.configDocument(t); doc["addr"] != "127.0.0.1:9999" {
		t.Errorf("addr = %v, want the address that was sent", doc["addr"])
	}

	// Saved is not applied: the address is read when the process starts, which
	// is exactly what leaves room to correct a wrong one.
	var settings settingsResponse
	env.decode(resp, &settings)
	if !settings.RestartRequired {
		t.Error("restartRequired = false after changing the address the server is bound to")
	}
	if settings.Running.Addr == "127.0.0.1:9999" {
		t.Error("the running address changed without a restart")
	}
}

func TestSettingsRefusesAnUnusableListenAddress(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1",         // no port
		"127.0.0.1:http",    // a name where a number belongs
		"127.0.0.1:0",       // a port the server would not be found on
		"127.0.0.1:70000",   // out of range
		"what is this:8080", // neither an address nor a host name
	} {
		t.Run(addr, func(t *testing.T) {
			env := newSettingsEnv(t)

			resp := env.sendJSON(http.MethodPut, "/api/settings", `{"addr": `+quote(addr)+`}`)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d for %q, want 400", resp.StatusCode, addr)
			}
			if _, ok := env.configDocument(t)["addr"]; ok {
				t.Error("a refused address was written to the file anyway")
			}
		})
	}
}

// The pair checkTransport guards: an address the network can reach, in
// plaintext, is refused before the file is touched — so the page cannot save a
// configuration the next start would refuse.
func TestSettingsRefusesAPublicAddressWithoutHTTPS(t *testing.T) {
	env := newSettingsEnv(t)

	resp := env.sendJSON(http.MethodPut, "/api/settings",
		`{"addr": "0.0.0.0:8080", "publicUrl": "http://hexagon.example", "insecureHttp": false}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, env.bodyString(resp))
	}
	// The refusal has to be the transport one, and it has to name both values,
	// since neither is wrong on its own.
	var body map[string]string
	env.decode(resp, &body)
	for _, want := range []string{"0.0.0.0:8080", "http://hexagon.example", "insecureHttp"} {
		if !strings.Contains(body["error"], want) {
			t.Errorf("error = %q, want it to mention %q", body["error"], want)
		}
	}
	if _, ok := env.configDocument(t)["addr"]; ok {
		t.Error("the address was written even though the configuration was refused")
	}

	// Said out loud, it is allowed: that is what the flag is for.
	resp = env.sendJSON(http.MethodPut, "/api/settings",
		`{"addr": "0.0.0.0:8080", "publicUrl": "http://hexagon.example", "insecureHttp": true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}
	if doc := env.configDocument(t); doc["addr"] != "0.0.0.0:8080" {
		t.Errorf("addr = %v, want it saved once the flag says so", doc["addr"])
	}
}

// configDocument is the configuration file as it is on disk.
func (e *testEnv) configDocument(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(e.cfg.ConfigPath)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse the configuration file: %v", err)
	}
	return doc
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

// Debug logging is turned on to watch something that is going wrong now, so a
// save that only took effect at the next restart would arrive after the run it
// was meant to explain.
func TestSettingsApplyDebugLoggingWithoutARestart(t *testing.T) {
	env := newSettingsEnv(t)
	if env.deps.LogLevel.Level() != slog.LevelInfo {
		t.Fatalf("level = %v before the save, want info", env.deps.LogLevel.Level())
	}

	resp := env.sendJSON(http.MethodPut, "/api/settings", `{"debug": true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, env.bodyString(resp))
	}
	var settings settingsResponse
	env.decode(resp, &settings)

	if env.deps.LogLevel.Level() != slog.LevelDebug {
		t.Errorf("level = %v, want debug as soon as it is saved", env.deps.LogLevel.Level())
	}
	if !settings.Running.Debug || !settings.Saved.Debug {
		t.Errorf("running/saved debug = %v/%v, want both true", settings.Running.Debug, settings.Saved.Debug)
	}
	if settings.RestartRequired {
		t.Error("restartRequired = true after changing only debug logging, want false")
	}

	// And back again: a level that could only be raised would be worse than one
	// that needed a restart.
	env.decode(env.sendJSON(http.MethodPut, "/api/settings", `{"debug": false}`), &settings)
	if env.deps.LogLevel.Level() != slog.LevelInfo {
		t.Errorf("level = %v after turning it off, want info", env.deps.LogLevel.Level())
	}
	if settings.Running.Debug {
		t.Error("running debug = true after turning it off")
	}
}
