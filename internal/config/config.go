// Package config loads Hexagon's runtime configuration from the environment.
//
// Every setting has a usable default for a single-user setup on a developer
// machine. Anything security-sensitive fails closed: the listen address is
// loopback unless overridden, and generated secrets are written with 0600.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the resolved configuration for one server process.
type Config struct {
	// HTTP
	Addr      string // HEXAGON_ADDR
	PublicURL string // HEXAGON_PUBLIC_URL, used for OAuth callbacks and Origin checks

	// Storage
	DataDir       string // HEXAGON_DATA_DIR
	WorkspaceRoot string // HEXAGON_WORKSPACE_ROOT, parent of every session workspace
	DatabasePath  string // derived: <DataDir>/hexagon.db

	// GitHub OAuth
	GitHubClientID     string   // HEXAGON_GITHUB_CLIENT_ID
	GitHubClientSecret string   // HEXAGON_GITHUB_CLIENT_SECRET
	AllowedUsers       []string // HEXAGON_ALLOWED_USERS, comma separated GitHub logins
	// GitHubAPIURL overrides api.github.com. It is there for GitHub Enterprise,
	// and for pointing a development instance at a stub.
	GitHubAPIURL string // HEXAGON_GITHUB_API_URL

	// Development bypass. When DevUser is set the server skips OAuth and treats
	// every request as that user, authenticating to GitHub with DevGitHubToken.
	DevUser        string // HEXAGON_DEV_USER
	DevGitHubToken string // HEXAGON_GITHUB_TOKEN

	// SecretKey seals GitHub tokens at rest. 32 bytes.
	SecretKey []byte // HEXAGON_SECRET_KEY, else <DataDir>/secret.key

	// Claude Code credentials handed to session containers.
	ClaudeCredentials string // HEXAGON_CLAUDE_CREDENTIALS, empty disables the mount
	AnthropicAPIKey   string // ANTHROPIC_API_KEY

	// Git identity used for clones and for commits made inside containers.
	GitUserName  string // HEXAGON_GIT_USER_NAME
	GitUserEmail string // HEXAGON_GIT_USER_EMAIL

	// Docker
	DockerHost string // DOCKER_HOST, empty means the SDK default
}

const secretKeyLen = 32

// Load reads the environment, creates the data directories and resolves the
// secret key, generating one on first run.
func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}

	dataDir := env("HEXAGON_DATA_DIR", filepath.Join(home, ".local", "share", "hexagon"))
	cfg := &Config{
		Addr:      env("HEXAGON_ADDR", "127.0.0.1:8080"),
		PublicURL: strings.TrimRight(env("HEXAGON_PUBLIC_URL", "http://127.0.0.1:8080"), "/"),

		DataDir:       dataDir,
		WorkspaceRoot: env("HEXAGON_WORKSPACE_ROOT", filepath.Join(dataDir, "workspaces")),
		DatabasePath:  filepath.Join(dataDir, "hexagon.db"),

		GitHubClientID:     os.Getenv("HEXAGON_GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("HEXAGON_GITHUB_CLIENT_SECRET"),
		AllowedUsers:       splitList(os.Getenv("HEXAGON_ALLOWED_USERS")),
		GitHubAPIURL:       os.Getenv("HEXAGON_GITHUB_API_URL"),

		DevUser:        os.Getenv("HEXAGON_DEV_USER"),
		DevGitHubToken: os.Getenv("HEXAGON_GITHUB_TOKEN"),

		ClaudeCredentials: env("HEXAGON_CLAUDE_CREDENTIALS", filepath.Join(home, ".claude", ".credentials.json")),
		AnthropicAPIKey:   os.Getenv("ANTHROPIC_API_KEY"),

		GitUserName:  os.Getenv("HEXAGON_GIT_USER_NAME"),
		GitUserEmail: os.Getenv("HEXAGON_GIT_USER_EMAIL"),

		DockerHost: os.Getenv("DOCKER_HOST"),
	}

	for _, dir := range []string{cfg.DataDir, cfg.WorkspaceRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	cfg.SecretKey, err = loadSecretKey(filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadSecretKey returns the key from HEXAGON_SECRET_KEY, falling back to path
// and generating a fresh key there when the file does not exist yet.
func loadSecretKey(path string) ([]byte, error) {
	if raw := os.Getenv("HEXAGON_SECRET_KEY"); raw != "" {
		key, err := decodeKey(raw)
		if err != nil {
			return nil, fmt.Errorf("HEXAGON_SECRET_KEY: %w", err)
		}
		return key, nil
	}

	switch data, err := os.ReadFile(path); {
	case err == nil:
		key, err := decodeKey(string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return key, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	key := make([]byte, secretKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return key, nil
}

func decodeKey(raw string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	if len(key) != secretKeyLen {
		return nil, fmt.Errorf("want %d bytes, got %d", secretKeyLen, len(key))
	}
	return key, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
