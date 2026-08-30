// Package config loads Hexagon's runtime configuration from a JSON file and the
// environment.
//
// Every setting has a usable default for a single-user setup on a developer
// machine, and can be given either in the configuration file or as an
// environment variable: defaults first, then the file, then the environment,
// which wins so that a one-off override stays a one-off override. Anything
// security-sensitive fails closed: the listen address is loopback unless
// overridden, generated secrets are written with 0600, and the configuration
// file is rejected when it is readable by anyone else.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/andrea/hexagon/internal/codeserver"
)

// Config holds the resolved configuration for one server process.
type Config struct {
	// ConfigFile is the file the settings were read from, empty when none was
	// found. It is here so the process can report where it was configured from.
	ConfigFile string
	// ConfigPath is where a configuration file belongs for this process,
	// whether or not one is there yet. It is what the first-time wizard writes,
	// so it has to be known even on the run that finds no file at all.
	ConfigPath string

	// HTTP
	Addr      string // HEXAGON_ADDR, addr
	PublicURL string // HEXAGON_PUBLIC_URL, publicUrl: used for OAuth callbacks and Origin checks
	// InsecureHTTP lifts the refusal to serve a non-loopback address without
	// https. "http on a private network" is a real deployment; this is how it
	// says so out loud.
	InsecureHTTP bool // HEXAGON_INSECURE_HTTP, insecureHttp

	// Storage
	DataDir       string // HEXAGON_DATA_DIR, dataDir
	WorkspaceRoot string // HEXAGON_WORKSPACE_ROOT, workspaceRoot: parent of every session workspace
	DatabasePath  string // derived: <DataDir>/hexagon.db

	// GitHub OAuth
	GitHubClientID     string   // HEXAGON_GITHUB_CLIENT_ID, github.clientId
	GitHubClientSecret string   // HEXAGON_GITHUB_CLIENT_SECRET, github.clientSecret
	AllowedUsers       []string // HEXAGON_ALLOWED_USERS, github.allowedUsers
	// GitHubAPIURL overrides api.github.com. It is there for GitHub Enterprise,
	// and for pointing a development instance at a stub.
	GitHubAPIURL string // HEXAGON_GITHUB_API_URL, github.apiUrl

	// BitbucketAPIURL overrides api.bitbucket.org, for the same reasons.
	BitbucketAPIURL string // HEXAGON_BITBUCKET_API_URL, bitbucket.apiUrl

	// SecretKey seals GitHub tokens at rest. 32 bytes.
	SecretKey []byte // HEXAGON_SECRET_KEY, secretKey, else <DataDir>/secret.key
	// SecretKeySource names where that key came from — the variable, the
	// configuration file, or the generated file under DataDir. It is what the
	// settings page shows in place of a key it must never display, and it is a
	// string rather than an enum because the third case is a path.
	SecretKeySource string

	// Claude Code credentials handed to session containers.
	ClaudeCredentials string // HEXAGON_CLAUDE_CREDENTIALS, claude.credentials: empty disables the mount
	AnthropicAPIKey   string // ANTHROPIC_API_KEY, claude.anthropicApiKey
	// ClaudeBinary runs `claude -p` on the host, for editing a Dockerfile from
	// the Images page. Empty means the PATH, then ~/.local/bin/claude.
	ClaudeBinary string // HEXAGON_CLAUDE_BINARY, claude.binary
	ClaudeModel  string // HEXAGON_CLAUDE_MODEL, claude.model: empty leaves the choice to the CLI

	// Git identity used for clones and for commits made inside containers.
	GitUserName  string // HEXAGON_GIT_USER_NAME, git.userName
	GitUserEmail string // HEXAGON_GIT_USER_EMAIL, git.userEmail

	// VS Code integration: one code-server release kept on the host and bind
	// mounted into every session created with it.
	VSCodeDir     string // HEXAGON_VSCODE_DIR, vscode.dir
	VSCodeVersion string // HEXAGON_VSCODE_VERSION, vscode.version: release fetched when VSCodeDir is empty

	// Docker
	DockerHost string // DOCKER_HOST, docker.host: empty means the SDK default
	// DockerCLI runs `docker compose` for sessions whose image carries one.
	// Empty means `docker` on the PATH.
	DockerCLI string // HEXAGON_DOCKER_CLI, docker.cli

	// Limits. They are settings rather than constants because the right numbers
	// depend on the machine: an operator with room to spare will want different
	// ones.
	MaxSessionsPerUser  int // HEXAGON_MAX_SESSIONS_PER_USER, limits.maxSessionsPerUser
	MaxConcurrentBuilds int // HEXAGON_MAX_CONCURRENT_BUILDS, limits.maxConcurrentBuilds
	PublicRatePerMinute int // HEXAGON_PUBLIC_RATE_PER_MINUTE, limits.publicRatePerMinute

	// Debug turns on debug level logging.
	Debug bool // HEXAGON_DEBUG, debug
}

const secretKeyLen = 32

// file mirrors the configuration file. Keys are camelCase, as in the HTTP API,
// and grouped the way Config is. A zero value means "not set", so the layer
// below shows through; claude.credentials is a pointer because for that one
// setting an explicit empty string is itself a value.
type file struct {
	Addr          string `json:"addr"`
	PublicURL     string `json:"publicUrl"`
	InsecureHTTP  bool   `json:"insecureHttp"`
	DataDir       string `json:"dataDir"`
	WorkspaceRoot string `json:"workspaceRoot"`
	SecretKey     string `json:"secretKey"`
	Debug         bool   `json:"debug"`

	GitHub struct {
		ClientID     string   `json:"clientId"`
		ClientSecret string   `json:"clientSecret"`
		AllowedUsers []string `json:"allowedUsers"`
		APIURL       string   `json:"apiUrl"`
	} `json:"github"`

	Bitbucket struct {
		APIURL string `json:"apiUrl"`
	} `json:"bitbucket"`

	Claude struct {
		Credentials     *string `json:"credentials"`
		AnthropicAPIKey string  `json:"anthropicApiKey"`
		Binary          string  `json:"binary"`
		Model           string  `json:"model"`
	} `json:"claude"`

	Git struct {
		UserName  string `json:"userName"`
		UserEmail string `json:"userEmail"`
	} `json:"git"`

	VSCode struct {
		Dir     string `json:"dir"`
		Version string `json:"version"`
	} `json:"vscode"`

	Docker struct {
		Host string `json:"host"`
		CLI  string `json:"cli"`
	} `json:"docker"`

	Limits struct {
		MaxSessionsPerUser  int `json:"maxSessionsPerUser"`
		MaxConcurrentBuilds int `json:"maxConcurrentBuilds"`
		PublicRatePerMinute int `json:"publicRatePerMinute"`
	} `json:"limits"`
}

// Load resolves the configuration, creates the data directories and resolves
// the secret key, generating one on first run.
//
// path is the file named on the command line; when it is empty the file is
// looked for at HEXAGON_CONFIG and then at the default location, and running
// without one is fine.
func Load(path string) (*Config, error) {
	path, explicit, err := resolvePath(path)
	if err != nil {
		return nil, err
	}
	return load(path, explicit)
}

// Resolve is Load for a file that is allowed not to be there yet: it reports
// what the configuration file at path would produce, and on a machine that has
// never had one the answer is the defaults and the environment rather than an
// error.
//
// Load goes on refusing a path that was named on the command line and is not
// there, because ignoring a path someone asked for starts a server configured by
// accident. The difference is that here the path is not being asked for, it is
// being reported on: this is what the settings page compares the running process
// against.
func Resolve(path string) (*Config, error) {
	return load(path, false)
}

func load(path string, explicit bool) (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}

	f, found, err := loadFile(path, explicit)
	if err != nil {
		return nil, err
	}
	from := ""
	if found {
		from = path
	}

	maxSessions, err := pickInt("HEXAGON_MAX_SESSIONS_PER_USER", f.Limits.MaxSessionsPerUser, 20)
	if err != nil {
		return nil, err
	}
	maxBuilds, err := pickInt("HEXAGON_MAX_CONCURRENT_BUILDS", f.Limits.MaxConcurrentBuilds, 2)
	if err != nil {
		return nil, err
	}
	publicRate, err := pickInt("HEXAGON_PUBLIC_RATE_PER_MINUTE", f.Limits.PublicRatePerMinute, 60)
	if err != nil {
		return nil, err
	}

	dataDir := expandHome(home, pick("HEXAGON_DATA_DIR", f.DataDir, filepath.Join(home, ".local", "share", "hexagon")))
	cfg := &Config{
		ConfigFile: from,
		ConfigPath: path,

		Addr:         pick("HEXAGON_ADDR", f.Addr, "127.0.0.1:8080"),
		PublicURL:    strings.TrimRight(pick("HEXAGON_PUBLIC_URL", f.PublicURL, "http://127.0.0.1:8080"), "/"),
		InsecureHTTP: pickBool("HEXAGON_INSECURE_HTTP", f.InsecureHTTP),

		DataDir:       dataDir,
		WorkspaceRoot: expandHome(home, pick("HEXAGON_WORKSPACE_ROOT", f.WorkspaceRoot, filepath.Join(dataDir, "workspaces"))),
		DatabasePath:  filepath.Join(dataDir, "hexagon.db"),

		GitHubClientID:     pick("HEXAGON_GITHUB_CLIENT_ID", f.GitHub.ClientID, ""),
		GitHubClientSecret: pick("HEXAGON_GITHUB_CLIENT_SECRET", f.GitHub.ClientSecret, ""),
		AllowedUsers:       allowedUsers(f.GitHub.AllowedUsers),
		GitHubAPIURL:       pick("HEXAGON_GITHUB_API_URL", f.GitHub.APIURL, ""),

		BitbucketAPIURL: pick("HEXAGON_BITBUCKET_API_URL", f.Bitbucket.APIURL, ""),

		ClaudeCredentials: expandHome(home, claudeCredentials(f, home)),
		AnthropicAPIKey:   pick("ANTHROPIC_API_KEY", f.Claude.AnthropicAPIKey, ""),
		ClaudeBinary:      expandHome(home, pick("HEXAGON_CLAUDE_BINARY", f.Claude.Binary, "")),
		ClaudeModel:       pick("HEXAGON_CLAUDE_MODEL", f.Claude.Model, ""),

		GitUserName:  pick("HEXAGON_GIT_USER_NAME", f.Git.UserName, ""),
		GitUserEmail: pick("HEXAGON_GIT_USER_EMAIL", f.Git.UserEmail, ""),

		VSCodeDir:     expandHome(home, pick("HEXAGON_VSCODE_DIR", f.VSCode.Dir, filepath.Join(dataDir, "code-server"))),
		VSCodeVersion: pick("HEXAGON_VSCODE_VERSION", f.VSCode.Version, codeserver.DefaultVersion),

		DockerHost: pick("DOCKER_HOST", f.Docker.Host, ""),
		DockerCLI:  expandHome(home, pick("HEXAGON_DOCKER_CLI", f.Docker.CLI, "")),

		MaxSessionsPerUser:  maxSessions,
		MaxConcurrentBuilds: maxBuilds,
		PublicRatePerMinute: publicRate,

		Debug: pickBool("HEXAGON_DEBUG", f.Debug),
	}

	if err := checkTransport(cfg); err != nil {
		return nil, err
	}

	for _, dir := range []string{cfg.DataDir, cfg.WorkspaceRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	keyFile := filepath.Join(cfg.DataDir, "secret.key")
	configured, source := os.Getenv("HEXAGON_SECRET_KEY"), "HEXAGON_SECRET_KEY"
	if configured == "" {
		configured, source = f.SecretKey, "the configuration file"
	}
	if configured == "" {
		source = keyFile
	}
	cfg.SecretKey, err = loadSecretKey(keyFile, configured)
	if err != nil {
		return nil, err
	}
	cfg.SecretKeySource = source
	return cfg, nil
}

// checkTransport refuses a configuration that would publish Hexagon in
// plaintext. Whoever reaches the port controls the Docker socket, and over http
// the session cookie that opens it travels in the clear; binding loopback is the
// only reason a plaintext instance is survivable at all.
//
// The check is a property of the pair rather than of either setting, which is
// why it lives here and not in a consumer of one of them. Note what it does not
// catch: a loopback address with an https public URL is the deployment behind a
// TLS proxy, and passes.
func checkTransport(cfg *Config) error {
	switch {
	case cfg.InsecureHTTP, isLoopbackAddr(cfg.Addr), strings.HasPrefix(cfg.PublicURL, "https://"):
		return nil
	}
	return fmt.Errorf("addr %s accepts traffic from the network but publicUrl %s is not https: "+
		"put a TLS reverse proxy in front and bind loopback, or set insecureHttp / HEXAGON_INSECURE_HTTP "+
		"to serve plaintext deliberately", cfg.Addr, cfg.PublicURL)
}

// isLoopbackAddr reports whether a listen address only accepts local traffic.
// An address with no host ("" or ":8080") listens on every interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// resolvePath decides which configuration file this process uses: the one named
// on the command line, then HEXAGON_CONFIG, then the default location. It
// answers whether or not the file exists, because the first-time wizard has to
// know where to create one.
//
// explicit says the user named the path, which is what makes a missing file an
// error rather than a server that simply has no configuration file.
func resolvePath(path string) (string, bool, error) {
	if path != "" {
		return path, true, nil
	}
	if path = os.Getenv("HEXAGON_CONFIG"); path != "" {
		return path, true, nil
	}
	// Not under DataDir: the file is what sets DataDir, so looking for it there
	// would be circular.
	dir, err := os.UserConfigDir()
	if err != nil {
		// No config directory to name, so there is no file and nowhere to put
		// one. Everything else still resolves from defaults and the
		// environment.
		return "", false, nil
	}
	return filepath.Join(dir, "hexagon", "config.json"), false, nil
}

// loadFile reads the configuration file at path and reports whether there was
// one.
//
// A file the user named explicitly must exist: ignoring a path someone asked for
// would start a server configured by accident. The default location is
// optional, so a machine that has never had a configuration file keeps behaving
// exactly as it did.
func loadFile(path string, explicit bool) (*file, bool, error) {
	if path == "" {
		return &file{}, false, nil
	}

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !explicit:
		return &file{}, false, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, false, fmt.Errorf("configuration file %s does not exist", path)
	case err != nil:
		return nil, false, fmt.Errorf("configuration file %s: %w", path, err)
	}

	// It can hold the OAuth client secret, an API key and the key that seals
	// stored GitHub tokens, so it is held to the standard of the files Hexagon
	// writes itself.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, false, fmt.Errorf("configuration file %s is readable by other users (mode %04o): chmod 600 it", path, perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	var f file
	dec := json.NewDecoder(bytes.NewReader(data))
	// A misspelled key would otherwise be dropped in silence, and a dropped
	// allowedUsers is an authentication bypass.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, true, nil
}

// loadSecretKey returns the configured key, falling back to path and generating
// a fresh key there when the file does not exist yet.
func loadSecretKey(path, configured string) ([]byte, error) {
	if configured != "" {
		key, err := decodeKey(configured)
		if err != nil {
			return nil, fmt.Errorf("secret key: %w", err)
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

// pick resolves one setting: the environment variable if it carries a value,
// then the configuration file, then the default.
func pick(key, fromFile, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if fromFile != "" {
		return fromFile
	}
	return def
}

// pickBool resolves a boolean setting. The environment carries these as a
// presence rather than a value — HEXAGON_DEBUG has never meant anything but
// "set" — so any value in the variable turns the setting on and none of them
// turns it off again. Saying false is the configuration file's job, which is
// also the layer an operator reads back later.
func pickBool(key string, fromFile bool) bool {
	return os.Getenv(key) != "" || fromFile
}

// pickInt resolves a numeric setting. A value that is not a positive number is
// an error rather than a silent fall back to the default: every one of these
// bounds something, and a zero or a typo would either refuse all work or, worse,
// read as "no limit".
func pickInt(key string, fromFile, def int) (int, error) {
	value := fromFile
	if raw := os.Getenv(key); raw != "" {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return 0, fmt.Errorf("%s: %q is not a number", key, raw)
		}
		value = n
	}
	switch {
	case value == 0:
		return def, nil
	case value < 0:
		return 0, fmt.Errorf("%s: %d is not a usable limit", key, value)
	}
	return value, nil
}

// allowedUsers reads the allowlist from whichever layer supplies one. The
// environment carries it as a comma-separated string, the file as a JSON array.
func allowedUsers(fromFile []string) []string {
	if raw := os.Getenv("HEXAGON_ALLOWED_USERS"); raw != "" {
		return splitList(raw)
	}
	var out []string
	for _, login := range fromFile {
		if login = strings.TrimSpace(login); login != "" {
			out = append(out, login)
		}
	}
	return out
}

// claudeCredentials resolves the file bind mounted into session containers.
// Unlike every other setting an empty value means something here — no mount at
// all — so both layers have to be able to say it: LookupEnv rather than Getenv,
// and a pointer in the configuration file.
func claudeCredentials(f *file, home string) string {
	if v, ok := os.LookupEnv("HEXAGON_CLAUDE_CREDENTIALS"); ok {
		return v
	}
	if f.Claude.Credentials != nil {
		return *f.Claude.Credentials
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// expandHome resolves a leading ~. A hand-written configuration file is exactly
// where one gets typed, and without this os.MkdirAll would cheerfully create a
// directory named "~".
func expandHome(home, path string) string {
	switch {
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	}
	return path
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
