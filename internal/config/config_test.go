package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate makes a test independent of the machine it runs on: a temporary home,
// no XDG configuration directory holding a real config.json, and none of
// Hexagon's variables inherited from the shell that started `go test`. It
// returns the temporary home.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{
		"XDG_CONFIG_HOME", "HEXAGON_CONFIG", "HEXAGON_ADDR", "HEXAGON_PUBLIC_URL",
		"HEXAGON_DATA_DIR", "HEXAGON_WORKSPACE_ROOT", "HEXAGON_SECRET_KEY",
		"HEXAGON_DEBUG", "HEXAGON_GITHUB_CLIENT_ID", "HEXAGON_GITHUB_CLIENT_SECRET",
		"HEXAGON_INSECURE_HTTP", "HEXAGON_ALLOWED_USERS", "HEXAGON_GITHUB_API_URL",
		"HEXAGON_MAX_SESSIONS_PER_USER", "HEXAGON_MAX_CONCURRENT_BUILDS", "HEXAGON_PUBLIC_RATE_PER_MINUTE",
		"HEXAGON_CLAUDE_CREDENTIALS", "ANTHROPIC_API_KEY",
		"HEXAGON_CLAUDE_BINARY", "HEXAGON_CLAUDE_MODEL",
		"HEXAGON_BITBUCKET_API_URL", "HEXAGON_VSCODE_DIR", "HEXAGON_VSCODE_VERSION",
		"HEXAGON_GIT_USER_NAME", "HEXAGON_GIT_USER_EMAIL", "DOCKER_HOST",
	} {
		// t.Setenv registers the restore; os.Unsetenv then leaves the variable
		// absent rather than empty, which for the Claude credentials is a
		// different thing entirely.
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	return home
}

// writeConfig puts body in a configuration file with the permissions Hexagon
// insists on, and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadGeneratesAndReusesSecretKey(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	t.Setenv("HEXAGON_DATA_DIR", dir)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.SecretKey) != secretKeyLen {
		t.Fatalf("secret key length = %d, want %d", len(cfg.SecretKey), secretKeyLen)
	}

	info, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("stat secret.key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("secret.key permissions = %04o, want 0600", got)
	}

	// A second run must reuse the key, otherwise every restart would invalidate
	// the stored GitHub tokens.
	again, err := Load("")
	if err != nil {
		t.Fatalf("Load again: %v", err)
	}
	if string(again.SecretKey) != string(cfg.SecretKey) {
		t.Error("secret key changed across restarts")
	}
}

func TestLoadRejectsMalformedSecretKey(t *testing.T) {
	isolate(t)
	t.Setenv("HEXAGON_DATA_DIR", t.TempDir())
	t.Setenv("HEXAGON_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte("too short")))

	if _, err := Load(""); err == nil {
		t.Fatal("Load accepted a secret key of the wrong length")
	}
}

func TestLoadDefaults(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	t.Setenv("HEXAGON_DATA_DIR", dir)
	t.Setenv("HEXAGON_ALLOWED_USERS", " alice , , bob ")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q, want the loopback default", cfg.Addr)
	}
	if want := filepath.Join(dir, "workspaces"); cfg.WorkspaceRoot != want {
		t.Errorf("WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, want)
	}
	if got := cfg.AllowedUsers; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("AllowedUsers = %q, want [alice bob]", got)
	}
	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile = %q, want empty when there is no file", cfg.ConfigFile)
	}
}

// The point of the file is that the server can start with nothing in the
// environment at all, so every setting has to be reachable from it.
func TestLoadReadsEverySettingFromTheFile(t *testing.T) {
	isolate(t)
	dataDir := t.TempDir()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, secretKeyLen))
	path := writeConfig(t, `{
		"addr": "0.0.0.0:9000",
		"publicUrl": "https://hexagon.example/",
		"insecureHttp": true,
		"dataDir": "`+dataDir+`",
		"workspaceRoot": "`+filepath.Join(dataDir, "ws")+`",
		"secretKey": "`+key+`",
		"debug": true,
		"github": {
			"clientId": "Iv23li",
			"clientSecret": "shh",
			"allowedUsers": ["alice", " bob "],
			"apiUrl": "https://ghe.example/api/v3"
		},
		"claude": {"credentials": "/etc/creds.json", "anthropicApiKey": "sk-ant"},
		"git": {"userName": "Andrea", "userEmail": "andrea@example.com"},
		"docker": {"host": "tcp://127.0.0.1:2375"},
		"limits": {"maxSessionsPerUser": 5, "maxConcurrentBuilds": 1, "publicRatePerMinute": 10}
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, c := range []struct {
		field string
		got   string
		want  string
	}{
		{"ConfigFile", cfg.ConfigFile, path},
		{"Addr", cfg.Addr, "0.0.0.0:9000"},
		{"PublicURL", cfg.PublicURL, "https://hexagon.example"},
		{"DataDir", cfg.DataDir, dataDir},
		{"WorkspaceRoot", cfg.WorkspaceRoot, filepath.Join(dataDir, "ws")},
		{"DatabasePath", cfg.DatabasePath, filepath.Join(dataDir, "hexagon.db")},
		{"GitHubClientID", cfg.GitHubClientID, "Iv23li"},
		{"GitHubClientSecret", cfg.GitHubClientSecret, "shh"},
		{"GitHubAPIURL", cfg.GitHubAPIURL, "https://ghe.example/api/v3"},
		{"ClaudeCredentials", cfg.ClaudeCredentials, "/etc/creds.json"},
		{"AnthropicAPIKey", cfg.AnthropicAPIKey, "sk-ant"},
		{"GitUserName", cfg.GitUserName, "Andrea"},
		{"GitUserEmail", cfg.GitUserEmail, "andrea@example.com"},
		{"DockerHost", cfg.DockerHost, "tcp://127.0.0.1:2375"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if got := cfg.AllowedUsers; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("AllowedUsers = %q, want [alice bob]", got)
	}
	if !cfg.Debug {
		t.Error("Debug = false, want the file's true")
	}
	if !cfg.InsecureHTTP {
		t.Error("InsecureHTTP = false, want the file's true")
	}
	for _, c := range []struct {
		field string
		got   int
		want  int
	}{
		{"MaxSessionsPerUser", cfg.MaxSessionsPerUser, 5},
		{"MaxConcurrentBuilds", cfg.MaxConcurrentBuilds, 1},
		{"PublicRatePerMinute", cfg.PublicRatePerMinute, 10},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.field, c.got, c.want)
		}
	}
	if base64.StdEncoding.EncodeToString(cfg.SecretKey) != key {
		t.Error("SecretKey is not the one the file supplied")
	}
}

// The environment stays the last word, so `make dev` and a one-off override
// still work on a machine that has a configuration file.
func TestEnvironmentOverridesFile(t *testing.T) {
	isolate(t)
	dataDir := t.TempDir()
	path := writeConfig(t, `{
		"addr": "0.0.0.0:9000",
		"dataDir": "`+dataDir+`",
		"github": {"allowedUsers": ["alice"]}
	}`)
	t.Setenv("HEXAGON_ADDR", "127.0.0.1:7777")
	t.Setenv("HEXAGON_ALLOWED_USERS", "bob")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:7777" {
		t.Errorf("Addr = %q, want the environment's value", cfg.Addr)
	}
	if got := cfg.AllowedUsers; len(got) != 1 || got[0] != "bob" {
		t.Errorf("AllowedUsers = %q, want the environment's [bob]", got)
	}
}

func TestLoadFindsTheFileAtTheDefaultLocation(t *testing.T) {
	isolate(t)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	dir := filepath.Join(configHome, "hexagon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"addr": "127.0.0.1:9999"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConfigFile != path {
		t.Errorf("ConfigFile = %q, want %q", cfg.ConfigFile, path)
	}
	if cfg.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q, want the default file's value", cfg.Addr)
	}
}

// A file nobody asked for and that is not there is not an error; one that was
// named is, because otherwise the server starts configured by accident.
func TestLoadRejectsOnlyAFileThatWasAskedFor(t *testing.T) {
	isolate(t)
	t.Setenv("HEXAGON_DATA_DIR", t.TempDir())
	missing := filepath.Join(t.TempDir(), "nowhere.json")

	if _, err := Load(""); err != nil {
		t.Fatalf("Load with no file anywhere: %v", err)
	}
	if _, err := Load(missing); err == nil {
		t.Error("Load accepted a -config path that does not exist")
	}

	t.Setenv("HEXAGON_CONFIG", missing)
	if _, err := Load(""); err == nil {
		t.Error("Load accepted a HEXAGON_CONFIG that does not exist")
	}
}

func TestLoadRejectsAnUnknownKey(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `{"addr": "127.0.0.1:9000", "allowedUsers": ["alice"]}`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a key it does not know: a misspelled allowlist would be silently empty")
	}
	if !strings.Contains(err.Error(), "allowedUsers") {
		t.Errorf("error = %q, want it to name the offending key", err)
	}
}

func TestLoadRejectsAFileOthersCanRead(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `{"addr": "127.0.0.1:9000"}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a world-readable file holding secrets")
	}
	if !strings.Contains(err.Error(), "chmod") {
		t.Errorf("error = %q, want it to say how to fix the permissions", err)
	}
}

func TestLoadExpandsHomeInPaths(t *testing.T) {
	home := isolate(t)
	path := writeConfig(t, `{
		"dataDir": "~/data",
		"workspaceRoot": "~/work",
		"claude": {"credentials": "~/creds.json"}
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, "data"); cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
	if want := filepath.Join(home, "work"); cfg.WorkspaceRoot != want {
		t.Errorf("WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, want)
	}
	if want := filepath.Join(home, "creds.json"); cfg.ClaudeCredentials != want {
		t.Errorf("ClaudeCredentials = %q, want %q", cfg.ClaudeCredentials, want)
	}
}

// Empty is a value for this one setting: it means no credentials mount at all.
func TestClaudeCredentialsCanBeSetToEmpty(t *testing.T) {
	home := isolate(t)
	t.Setenv("HEXAGON_DATA_DIR", t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, ".claude", ".credentials.json"); cfg.ClaudeCredentials != want {
		t.Fatalf("ClaudeCredentials = %q, want the default %q", cfg.ClaudeCredentials, want)
	}

	fromFile := writeConfig(t, `{"claude": {"credentials": ""}}`)
	cfg, err = Load(fromFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClaudeCredentials != "" {
		t.Errorf("ClaudeCredentials = %q, want empty: the file disabled the mount", cfg.ClaudeCredentials)
	}

	t.Setenv("HEXAGON_CLAUDE_CREDENTIALS", "")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClaudeCredentials != "" {
		t.Errorf("ClaudeCredentials = %q, want empty: the variable disabled the mount", cfg.ClaudeCredentials)
	}
}

// Whoever reaches the port controls the Docker socket, so an instance the
// network can reach has to be behind https before it starts at all.
func TestLoadRefusesAPublicAddressWithoutHTTPS(t *testing.T) {
	for _, c := range []struct {
		name      string
		addr      string
		publicURL string
		insecure  string
		wantErr   bool
	}{
		{name: "the default: loopback over http", addr: "127.0.0.1:8080", publicURL: "http://127.0.0.1:8080"},
		{name: "loopback behind a TLS proxy", addr: "127.0.0.1:8080", publicURL: "https://hexagon.example"},
		{name: "every interface, https", addr: "0.0.0.0:8080", publicURL: "https://hexagon.example"},
		{name: "every interface, plaintext", addr: "0.0.0.0:8080", publicURL: "http://hexagon.example", wantErr: true},
		{name: "no host at all, plaintext", addr: ":8080", publicURL: "http://hexagon.example", wantErr: true},
		{name: "plaintext said out loud", addr: "0.0.0.0:8080", publicURL: "http://hexagon.example", insecure: "1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("HEXAGON_DATA_DIR", t.TempDir())
			t.Setenv("HEXAGON_ADDR", c.addr)
			t.Setenv("HEXAGON_PUBLIC_URL", c.publicURL)
			if c.insecure != "" {
				t.Setenv("HEXAGON_INSECURE_HTTP", c.insecure)
			}

			cfg, err := Load("")
			switch {
			case c.wantErr && err == nil:
				t.Fatal("Load accepted a public address in plaintext")
			case c.wantErr:
				// The message has to name both values, since neither is wrong
				// on its own.
				if !strings.Contains(err.Error(), c.addr) || !strings.Contains(err.Error(), c.publicURL) {
					t.Errorf("error = %q, want it to name both the address and the public URL", err)
				}
			case err != nil:
				t.Fatalf("Load: %v", err)
			case cfg.InsecureHTTP != (c.insecure != ""):
				t.Errorf("InsecureHTTP = %v, want %v", cfg.InsecureHTTP, c.insecure != "")
			}
		})
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	loopback := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "127.0.0.1"}
	for _, addr := range loopback {
		if !isLoopbackAddr(addr) {
			t.Errorf("isLoopbackAddr(%q) = false, want true", addr)
		}
	}

	// An empty host means every interface, which is the case that matters here.
	exposed := []string{":8080", "0.0.0.0:8080", "192.168.1.10:8080", "[::]:8080", "example.com:8080"}
	for _, addr := range exposed {
		if isLoopbackAddr(addr) {
			t.Errorf("isLoopbackAddr(%q) = true, want false", addr)
		}
	}
}

// Each limit bounds something, so an unusable value has to stop the server
// rather than quietly become the default.
func TestLoadRejectsAnUnusableLimit(t *testing.T) {
	for name, value := range map[string]string{
		"not a number": "many",
		"negative":     "-1",
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			t.Setenv("HEXAGON_DATA_DIR", t.TempDir())
			t.Setenv("HEXAGON_MAX_SESSIONS_PER_USER", value)

			if _, err := Load(""); err == nil {
				t.Errorf("Load accepted HEXAGON_MAX_SESSIONS_PER_USER=%q", value)
			}
		})
	}
}

func TestLoadDefaultsTheLimits(t *testing.T) {
	isolate(t)
	t.Setenv("HEXAGON_DATA_DIR", t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range []struct {
		field string
		got   int
		want  int
	}{
		{"MaxSessionsPerUser", cfg.MaxSessionsPerUser, 20},
		{"MaxConcurrentBuilds", cfg.MaxConcurrentBuilds, 2},
		{"PublicRatePerMinute", cfg.PublicRatePerMinute, 60},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want the default %d", c.field, c.got, c.want)
		}
	}
}
