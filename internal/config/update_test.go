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
	cfg, err := Update(path, Patch{
		GitHubClientID:     str("new"),
		GitHubClientSecret: str("shhh"),
		AllowedUsers:       &users,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	// What Update returns is what the next start resolves, so the assertions
	// below are about both at once.
	if reloaded, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	} else if reloaded.GitHubClientSecret != cfg.GitHubClientSecret {
		t.Error("Update returned a configuration the file does not produce")
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

	if _, err := Update(path, Patch{GitHubClientID: str("new")}); err != nil {
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
	if _, err := Update(path, Patch{
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

	if _, err := Update(path, Patch{GitHubClientID: str("new")}); err == nil {
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

	if _, err := Update(path, Patch{GitHubClientID: str("from-the-wizard")}); err != nil {
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

	// The layout the packages install, and the one a file-permission check gets
	// wrong: the service owns a 0600 configuration file inside a directory it
	// cannot write. Update replaces that file by renaming another one over it,
	// so the directory is what decides.
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	config := filepath.Join(locked, "config.json")
	if err := os.WriteFile(config, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Put the mode back, or the temporary directory cannot be cleaned up.
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if Writable(config) {
		t.Error("Writable = true for a file whose directory cannot be written, want false: the save would fail")
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestUpdateRemovesTheKeyOfASettingItWasAskedToClear(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `{
		"github": {"clientId": "old", "apiUrl": "https://ghe.example.test"},
		"git": {"userName": "Someone"}
	}`)

	if _, err := Update(path, Patch{GitHubAPIURL: str(""), GitUserName: str("")}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	doc := readBack(t, path)
	github, _ := doc["github"].(map[string]any)
	if _, ok := github["apiUrl"]; ok {
		t.Error("github.apiUrl is still there, want a cleared setting to lose its key rather than gain an empty one")
	}
	if github["clientId"] != "old" {
		t.Errorf("github.clientId = %v, want its neighbour left alone", github["clientId"])
	}
	// git held nothing else, so the group has nothing left to say.
	if _, ok := doc["git"]; ok {
		t.Errorf("git = %v, want an emptied group dropped with its last setting", doc["git"])
	}
}

func TestUpdateTellsAnEmptyClaudeCredentialFromTheDefault(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `{}`)

	// Empty is a choice here — mount nothing — so it is written rather than
	// removed.
	if _, err := Update(path, Patch{ClaudeCredentials: str("")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	claude, _ := readBack(t, path)["claude"].(map[string]any)
	if value, ok := claude["credentials"]; !ok || value != "" {
		t.Errorf("claude.credentials = %v, present = %v, want an explicit empty string", value, ok)
	}
	if cfg, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	} else if cfg.ClaudeCredentials != "" {
		t.Errorf("claude credentials = %q, want no mount at all", cfg.ClaudeCredentials)
	}

	// And removing the key is the other request: back to the default path.
	if _, err := Update(path, Patch{ClaudeCredentialsDefault: true}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, ok := readBack(t, path)["claude"]; ok {
		t.Error("claude is still there, want the key removed and its emptied group with it")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.HasSuffix(cfg.ClaudeCredentials, filepath.Join(".claude", ".credentials.json")) {
		t.Errorf("claude credentials = %q, want the default path back", cfg.ClaudeCredentials)
	}
}

func TestUpdateRefusesAChangeTheServerCouldNotStartFrom(t *testing.T) {
	isolate(t)
	// An address the network can reach, which is only allowed with an https
	// public URL. Taking that URL back to http is the shape of mistake that
	// would leave a server refusing to boot from its own settings page.
	path := writeConfig(t, `{"addr": "0.0.0.0:8080", "publicUrl": "https://hexagon.example.test"}`)

	if _, err := Update(path, Patch{PublicURL: str("http://hexagon.example.test")}); err == nil {
		t.Fatal("Update accepted settings Load refuses, want the candidate checked before the rename")
	}

	if doc := readBack(t, path); doc["publicUrl"] != "https://hexagon.example.test" {
		t.Errorf("publicUrl = %v, want the file left exactly as it was", doc["publicUrl"])
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".config-") {
			t.Errorf("%s survived a rejected save, want the candidate removed", entry.Name())
		}
	}
}

func TestCheckResolvesWithoutWritingAnything(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `{"github": {"clientId": "old"}}`)

	cfg, err := Check(path, Patch{GitHubClientID: str("new")})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if cfg.GitHubClientID != "new" {
		t.Errorf("client id = %q, want the value the file would have", cfg.GitHubClientID)
	}
	if cfg.ConfigPath != path || cfg.ConfigFile != path {
		t.Errorf("config path/file = %q/%q, want %q: the candidate file is nobody else's business",
			cfg.ConfigPath, cfg.ConfigFile, path)
	}

	github, _ := readBack(t, path)["github"].(map[string]any)
	if github["clientId"] != "old" {
		t.Errorf("github.clientId = %v, want the file untouched by a Check", github["clientId"])
	}
}

func readBack(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}
