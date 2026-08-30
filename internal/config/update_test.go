package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func str(s string) *string { return &s }

func TestUpdateKeepsEverySettingItDidNotWrite(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `{
		"addr": "127.0.0.1:9000",
		"github": {"clientId": "old", "apiUrl": "https://ghe.example.test"},
		"claude": {"credentials": ""},
		"limits": {"maxSessionsPerUser": 3}
	}`)

	users := []string{"1234", "bob"}
	if err := Update(path, Patch{
		GitHubClientID:     str("new"),
		GitHubClientSecret: str("shhh"),
		AllowedUsers:       &users,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubClientID != "new" || cfg.GitHubClientSecret != "shhh" {
		t.Errorf("client id/secret = %q/%q, want new/shhh", cfg.GitHubClientID, cfg.GitHubClientSecret)
	}
	if strings.Join(cfg.AllowedUsers, ",") != "1234,bob" {
		t.Errorf("allowed users = %v, want [1234 bob]", cfg.AllowedUsers)
	}
	if cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("addr = %q, want the value the file already had", cfg.Addr)
	}
	if cfg.GitHubAPIURL != "https://ghe.example.test" {
		t.Errorf("github api url = %q, want the value the file already had", cfg.GitHubAPIURL)
	}
	if cfg.MaxSessionsPerUser != 3 {
		t.Errorf("max sessions = %d, want 3, the value the file already had", cfg.MaxSessionsPerUser)
	}
	// The one setting where an explicit empty string is itself a value: writing
	// the file back must not turn "no mount" into the default path.
	if cfg.ClaudeCredentials != "" {
		t.Errorf("claude credentials = %q, want it to stay explicitly empty", cfg.ClaudeCredentials)
	}
}

func TestUpdateAddsNoKeyItWasNotAskedFor(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `{"addr": "127.0.0.1:9000"}`)

	if err := Update(path, Patch{GitHubClientID: str("new")}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(doc) != 2 || doc["addr"] == nil || doc["github"] == nil {
		t.Errorf("keys = %v, want only the one that was there and the one that was written", keys(doc))
	}
	github, _ := doc["github"].(map[string]any)
	if len(github) != 1 || github["clientId"] != "new" {
		t.Errorf("github = %v, want only clientId", github)
	}
}

func TestUpdateCreatesTheFileWithRestrictivePermissions(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "nested", "hexagon", "config.json")

	users := []string{"alice"}
	if err := Update(path, Patch{
		GitHubClientID: str("id"), GitHubClientSecret: str("secret"), AllowedUsers: &users,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600: the file holds the OAuth client secret", perm)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat the directory: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", perm)
	}
	// The point of the permissions is that Load accepts what Update wrote.
	if _, err := Load(path); err != nil {
		t.Errorf("Load of a file Update wrote: %v", err)
	}
}

func TestUpdateRefusesAFileItCannotUnderstand(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `{"nonsense": true}`)

	if err := Update(path, Patch{GitHubClientID: str("new")}); err == nil {
		t.Fatal("Update accepted a file with an unknown key, want a refusal to overwrite it")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(data), "nonsense") {
		t.Error("the file was rewritten anyway, want it left exactly as it was")
	}
}

func TestUpdateDoesNotOutrankTheEnvironment(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `{}`)
	t.Setenv("HEXAGON_GITHUB_CLIENT_ID", "from-the-environment")

	if err := Update(path, Patch{GitHubClientID: str("from-the-wizard")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubClientID != "from-the-environment" {
		t.Errorf("client id = %q, want the environment to go on winning over the file", cfg.GitHubClientID)
	}
}

func TestWritable(t *testing.T) {
	isolate(t)
	dir := t.TempDir()

	if !Writable(filepath.Join(dir, "does", "not", "exist", "config.json")) {
		t.Error("Writable = false for a path under a writable directory, want true")
	}

	if os.Geteuid() == 0 {
		// Root ignores the mode bits, so there is no unwritable directory to
		// test against.
		t.Skip("running as root")
	}
	readonly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if Writable(filepath.Join(readonly, "config.json")) {
		t.Error("Writable = true inside a directory the process cannot write, want false")
	}
	if Writable("") {
		t.Error("Writable = true for no path at all, want false")
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
