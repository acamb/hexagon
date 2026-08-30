package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/store"
)

// maxSettingsRequestBody bounds the settings form. It carries the whole
// configuration file minus the two settings this API cannot express, which is
// still short: some paths, two URLs, three numbers and a list of GitHub logins.
const maxSettingsRequestBody = 16 << 10

// settingsResponse is the settings page's whole view of the server.
//
// Running and Saved are the point of it. Most of these settings are captured
// when the process starts — the store, the Docker client, the session manager,
// the code-server release, the rate limiter, the cookie Secure flag — so a page
// that showed one set of values would be claiming that saving is applying. It
// is not, for all but the three the gate holds, and RestartRequired is the
// difference said out loud.
type settingsResponse struct {
	ConfigPath      string `json:"configPath"`
	Writable        bool   `json:"writable"`
	RestartRequired bool   `json:"restartRequired"`
	// FromEnvironment names the settings a variable is supplying, by their
	// configuration file key. The variable outranks the file, so writing those
	// from here has no effect until it goes away.
	FromEnvironment []string `json:"fromEnvironment,omitempty"`
	// The two secrets in the configuration file are reported as present or
	// absent and never as values. A sealed token has never left internal/auth;
	// these are the same rule applied to the secrets that live in a file.
	ClientSecretSet    bool `json:"clientSecretSet"`
	AnthropicAPIKeySet bool `json:"anthropicApiKeySet"`

	Running settingsValues `json:"running"`
	Saved   settingsValues `json:"saved"`
}

// settingsValues mirrors the configuration file group for group, plus two
// values that are derived rather than configured.
type settingsValues struct {
	// Addr and SecretKeySource are here to be displayed and have no counterpart
	// in the request: they are the two settings the API deliberately cannot
	// change. SecretKeySource names where the key comes from, never the key.
	Addr            string `json:"addr"`
	SecretKeySource string `json:"secretKeySource"`

	PublicURL     string `json:"publicUrl"`
	InsecureHTTP  bool   `json:"insecureHttp"`
	DataDir       string `json:"dataDir"`
	WorkspaceRoot string `json:"workspaceRoot"`
	Debug         bool   `json:"debug"`

	GitHub    settingsGitHub    `json:"github"`
	Bitbucket settingsBitbucket `json:"bitbucket"`
	Claude    settingsClaude    `json:"claude"`
	Git       settingsGit       `json:"git"`
	VSCode    settingsVSCode    `json:"vscode"`
	Docker    settingsDocker    `json:"docker"`
	Limits    settingsLimits    `json:"limits"`

	// CallbackURL is derived from PublicURL and appears in both halves because
	// it is what has to be registered on GitHub: the running one is what the
	// handshake sends today, the saved one what it will send after a restart.
	CallbackURL string `json:"callbackUrl"`
}

type settingsGitHub struct {
	ClientID     string   `json:"clientId"`
	AllowedUsers []string `json:"allowedUsers"`
	APIURL       string   `json:"apiUrl"`
}

type settingsBitbucket struct {
	APIURL string `json:"apiUrl"`
}

type settingsClaude struct {
	Credentials string `json:"credentials"`
	Binary      string `json:"binary"`
	Model       string `json:"model"`
}

type settingsGit struct {
	UserName  string `json:"userName"`
	UserEmail string `json:"userEmail"`
}

type settingsVSCode struct {
	Dir     string `json:"dir"`
	Version string `json:"version"`
}

type settingsDocker struct {
	Host string `json:"host"`
}

type settingsLimits struct {
	MaxSessionsPerUser  int `json:"maxSessionsPerUser"`
	MaxConcurrentBuilds int `json:"maxConcurrentBuilds"`
	PublicRatePerMinute int `json:"publicRatePerMinute"`
}

// settingsRequest is the settings form, mirroring the configuration file group
// for group as settingsValues does.
//
// Every field is a pointer: absent means "leave this one alone". That is what
// lets the two secrets be kept without ever being sent to the browser and back,
// and it is why there is no addr and no secretKey — see config.Patch for why
// those two are not settings this API can express.
type settingsRequest struct {
	PublicURL     *string `json:"publicUrl"`
	InsecureHTTP  *bool   `json:"insecureHttp"`
	DataDir       *string `json:"dataDir"`
	WorkspaceRoot *string `json:"workspaceRoot"`
	Debug         *bool   `json:"debug"`

	GitHub struct {
		ClientID     *string   `json:"clientId"`
		ClientSecret *string   `json:"clientSecret"`
		AllowedUsers *[]string `json:"allowedUsers"`
		APIURL       *string   `json:"apiUrl"`
	} `json:"github"`

	Bitbucket struct {
		APIURL *string `json:"apiUrl"`
	} `json:"bitbucket"`

	Claude struct {
		// Credentials is decoded by hand because it is the one setting where
		// absent, null and "" are three different requests: leave it alone, put
		// it back to the default path, and mount nothing at all.
		Credentials     json.RawMessage `json:"credentials"`
		AnthropicAPIKey *string         `json:"anthropicApiKey"`
		Binary          *string         `json:"binary"`
		Model           *string         `json:"model"`
	} `json:"claude"`

	Git struct {
		UserName  *string `json:"userName"`
		UserEmail *string `json:"userEmail"`
	} `json:"git"`

	VSCode struct {
		Dir     *string `json:"dir"`
		Version *string `json:"version"`
	} `json:"vscode"`

	Docker struct {
		Host *string `json:"host"`
	} `json:"docker"`

	Limits struct {
		MaxSessionsPerUser  *int `json:"maxSessionsPerUser"`
		MaxConcurrentBuilds *int `json:"maxConcurrentBuilds"`
		PublicRatePerMinute *int `json:"publicRatePerMinute"`
	} `json:"limits"`
}

// settingsError is a save the caller can fix rather than a failure of this
// server: a value the configuration loader refuses, a change that would leave
// nobody able to sign in, an allowlist without the caller in it. The handler
// answers 400 with its message; anything else is a 500.
type settingsError struct{ err error }

func (e settingsError) Error() string { return e.err.Error() }
func (e settingsError) Unwrap() error { return e.err }

// handleGetSettings reports the settings this server is running on beside the
// ones its configuration file now holds.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.settingsSnapshot()
	if err != nil {
		s.log.Error("read the configuration file", "path", s.cfg.ConfigPath, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// handleUpdateSettings writes the configuration file and applies the part of it
// that can be applied without a restart.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if err := decodeJSON(w, r, maxSettingsRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Against the file as the next start would read it, not against what this
	// process is running on: a form sending back a value the file already has
	// must not rewrite the file, and a path an operator spelled with a ~ must
	// not come back expanded.
	current, err := config.Resolve(s.cfg.ConfigPath)
	if err != nil {
		s.log.Error("read the configuration file", "path", s.cfg.ConfigPath, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	patch, err := req.patch(current)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.applySettings(r.Context(), s.user(r), patch); err != nil {
		var rejected settingsError
		if errors.As(err, &rejected) {
			writeError(w, http.StatusBadRequest, rejected.Error())
			return
		}
		s.log.Error("save the configuration file", "path", s.cfg.ConfigPath, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	settings, err := s.settingsSnapshot()
	if err != nil {
		s.log.Error("read back the configuration file", "path", s.cfg.ConfigPath, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("settings saved", "file", s.cfg.ConfigPath, "restart_required", settings.RestartRequired)
	writeJSON(w, http.StatusOK, settings)
}

// applySettings writes a patch and hands what can be applied to the gate.
//
// Nothing is written until the whole change is known to work. config.Check
// resolves the configuration the file would have and refuses one this server
// could not start from; the login is then built from that result, so settings
// that would leave nobody able to sign in are refused with the file untouched;
// and the caller is checked against the allowlist they just wrote, because
// requireAuth consults it on every request and there is no wizard to come back
// through. Only then is the file replaced and the gate moved.
//
// user is nil for the first-time wizard, where there is no caller to lock out.
//
// The OAuth client is built with the public URL this process is running on and
// not with the one in the file. Everything else that URL feeds — the origin
// check, the cookie Secure flag, the HSTS header — was captured at startup, and
// a redirect_uri that moved on its own would send the browser to an origin this
// same process rejects.
func (s *Server) applySettings(ctx context.Context, user *store.User, patch config.Patch) error {
	if patch.PublicURL != nil {
		if err := checkPublicURL(*patch.PublicURL); err != nil {
			return settingsError{err}
		}
	}

	candidate, err := config.Check(s.cfg.ConfigPath, patch)
	if err != nil {
		return settingsError{err}
	}
	allowlist, err := auth.NewAllowlist(candidate.AllowedUsers, s.store, s.log)
	if err != nil {
		return settingsError{err}
	}
	oauth, err := auth.NewOAuth(auth.OAuthConfig{
		ClientID:     candidate.GitHubClientID,
		ClientSecret: candidate.GitHubClientSecret,
		PublicURL:    s.cfg.PublicURL,
	})
	if err != nil {
		return settingsError{err}
	}
	if user != nil {
		switch allowed, err := allowlist.Allowed(ctx, user.GitHubLogin, user.GitHubID); {
		case err != nil:
			return err
		case !allowed:
			return settingsError{fmt.Errorf(
				"this list does not include you (%s): saving it would sign you out with no way back in",
				user.GitHubLogin)}
		}
	}

	if _, err := config.Update(s.cfg.ConfigPath, patch); err != nil {
		return err
	}
	s.gate.Set(oauth, allowlist)
	return nil
}

// settingsSnapshot describes the settings this server is running on beside the
// ones its configuration file holds.
//
// The saved half goes through config.Resolve rather than reading the file's raw
// values, because what matters is not what the file says but what the next
// start will resolve from it, defaults and environment included. It costs the
// side effects Load has — the data directories are created and the secret key
// is read — which are no-ops for the ones this process is already using.
func (s *Server) settingsSnapshot() (settingsResponse, error) {
	saved, err := config.Resolve(s.cfg.ConfigPath)
	if err != nil {
		return settingsResponse{}, err
	}

	running := settingsValuesOf(s.cfg)
	// The gate is the live truth for the three settings a save applies without
	// a restart; s.cfg is the snapshot this process started from.
	if oauth := s.gate.OAuth(); oauth != nil {
		running.GitHub.ClientID = oauth.ClientID()
	}
	if allowlist := s.gate.Allowlist(); allowlist != nil {
		running.GitHub.AllowedUsers = allowlist.Entries()
	}

	return settingsResponse{
		ConfigPath:         s.cfg.ConfigPath,
		Writable:           config.Writable(s.cfg.ConfigPath),
		RestartRequired:    restartRequired(running, settingsValuesOf(saved)),
		FromEnvironment:    environmentSettings(),
		ClientSecretSet:    saved.GitHubClientSecret != "",
		AnthropicAPIKeySet: saved.AnthropicAPIKey != "",
		Running:            running,
		Saved:              settingsValuesOf(saved),
	}, nil
}

// settingsValuesOf reads a resolved configuration into the shape the page uses.
func settingsValuesOf(cfg *config.Config) settingsValues {
	return settingsValues{
		Addr:            cfg.Addr,
		SecretKeySource: cfg.SecretKeySource,

		PublicURL:     cfg.PublicURL,
		InsecureHTTP:  cfg.InsecureHTTP,
		DataDir:       cfg.DataDir,
		WorkspaceRoot: cfg.WorkspaceRoot,
		Debug:         cfg.Debug,

		GitHub: settingsGitHub{
			ClientID:     cfg.GitHubClientID,
			AllowedUsers: cfg.AllowedUsers,
			APIURL:       cfg.GitHubAPIURL,
		},
		Bitbucket: settingsBitbucket{APIURL: cfg.BitbucketAPIURL},
		Claude: settingsClaude{
			Credentials: cfg.ClaudeCredentials,
			Binary:      cfg.ClaudeBinary,
			Model:       cfg.ClaudeModel,
		},
		Git:    settingsGit{UserName: cfg.GitUserName, UserEmail: cfg.GitUserEmail},
		VSCode: settingsVSCode{Dir: cfg.VSCodeDir, Version: cfg.VSCodeVersion},
		Docker: settingsDocker{Host: cfg.DockerHost},
		Limits: settingsLimits{
			MaxSessionsPerUser:  cfg.MaxSessionsPerUser,
			MaxConcurrentBuilds: cfg.MaxConcurrentBuilds,
			PublicRatePerMinute: cfg.PublicRatePerMinute,
		},

		CallbackURL: auth.CallbackURL(cfg.PublicURL),
	}
}

// restartRequired reports whether the configuration file has moved away from
// what this process is running on, ignoring the settings a save applies through
// the gate. It answers yes for a file edited by hand too, which is the point:
// the page is about the difference, not about the last save.
func restartRequired(running, saved settingsValues) bool {
	running.GitHub.ClientID, saved.GitHub.ClientID = "", ""
	running.GitHub.AllowedUsers, saved.GitHub.AllowedUsers = nil, nil
	return !reflect.DeepEqual(running, saved)
}

// environmentSettings names the settings a variable is supplying, by their
// configuration file key.
//
// Two of them carry no HEXAGON_ prefix, and the two booleans are worth knowing
// about for a reason of their own: for those the variable's mere presence wins,
// so a file saying false cannot turn one off again.
func environmentSettings() []string {
	var out []string
	for _, shadowed := range []struct{ variable, key string }{
		{"HEXAGON_PUBLIC_URL", "publicUrl"},
		{"HEXAGON_INSECURE_HTTP", "insecureHttp"},
		{"HEXAGON_DATA_DIR", "dataDir"},
		{"HEXAGON_WORKSPACE_ROOT", "workspaceRoot"},
		{"HEXAGON_DEBUG", "debug"},
		{"HEXAGON_GITHUB_CLIENT_ID", "github.clientId"},
		{"HEXAGON_GITHUB_CLIENT_SECRET", "github.clientSecret"},
		{"HEXAGON_ALLOWED_USERS", "github.allowedUsers"},
		{"HEXAGON_GITHUB_API_URL", "github.apiUrl"},
		{"HEXAGON_BITBUCKET_API_URL", "bitbucket.apiUrl"},
		{"HEXAGON_CLAUDE_CREDENTIALS", "claude.credentials"},
		{"ANTHROPIC_API_KEY", "claude.anthropicApiKey"},
		{"HEXAGON_CLAUDE_BINARY", "claude.binary"},
		{"HEXAGON_CLAUDE_MODEL", "claude.model"},
		{"HEXAGON_GIT_USER_NAME", "git.userName"},
		{"HEXAGON_GIT_USER_EMAIL", "git.userEmail"},
		{"HEXAGON_VSCODE_DIR", "vscode.dir"},
		{"HEXAGON_VSCODE_VERSION", "vscode.version"},
		{"DOCKER_HOST", "docker.host"},
		{"HEXAGON_MAX_SESSIONS_PER_USER", "limits.maxSessionsPerUser"},
		{"HEXAGON_MAX_CONCURRENT_BUILDS", "limits.maxConcurrentBuilds"},
		{"HEXAGON_PUBLIC_RATE_PER_MINUTE", "limits.publicRatePerMinute"},
	} {
		if os.Getenv(shadowed.variable) != "" {
			out = append(out, shadowed.key)
		}
	}
	return out
}

// patch turns the form into the settings that actually changed, measured
// against current: the configuration the file resolves to today.
//
// Dropping the fields that came back unchanged is what keeps a save from
// rewriting the file it was shown. A form posts every field it displays, so
// without this a path spelled with a ~ would come back expanded, and every
// value left at its default would become a key.
func (req settingsRequest) patch(current *config.Config) (config.Patch, error) {
	patch := config.Patch{
		PublicURL:     text(req.PublicURL, current.PublicURL),
		InsecureHTTP:  flag(req.InsecureHTTP, current.InsecureHTTP),
		DataDir:       text(req.DataDir, current.DataDir),
		WorkspaceRoot: text(req.WorkspaceRoot, current.WorkspaceRoot),
		Debug:         flag(req.Debug, current.Debug),

		GitHubClientID: text(req.GitHub.ClientID, current.GitHubClientID),
		// A secret the form did not send is one it was not shown: an empty
		// field means "keep the stored one", not "clear it".
		GitHubClientSecret: secret(req.GitHub.ClientSecret, current.GitHubClientSecret),
		AllowedUsers:       list(req.GitHub.AllowedUsers, current.AllowedUsers),
		GitHubAPIURL:       text(req.GitHub.APIURL, current.GitHubAPIURL),

		BitbucketAPIURL: text(req.Bitbucket.APIURL, current.BitbucketAPIURL),

		AnthropicAPIKey: secret(req.Claude.AnthropicAPIKey, current.AnthropicAPIKey),
		ClaudeBinary:    text(req.Claude.Binary, current.ClaudeBinary),
		ClaudeModel:     text(req.Claude.Model, current.ClaudeModel),

		GitUserName:  text(req.Git.UserName, current.GitUserName),
		GitUserEmail: text(req.Git.UserEmail, current.GitUserEmail),

		VSCodeDir:     text(req.VSCode.Dir, current.VSCodeDir),
		VSCodeVersion: text(req.VSCode.Version, current.VSCodeVersion),

		DockerHost: text(req.Docker.Host, current.DockerHost),

		MaxSessionsPerUser:  number(req.Limits.MaxSessionsPerUser, current.MaxSessionsPerUser),
		MaxConcurrentBuilds: number(req.Limits.MaxConcurrentBuilds, current.MaxConcurrentBuilds),
		PublicRatePerMinute: number(req.Limits.PublicRatePerMinute, current.PublicRatePerMinute),
	}

	// Absent, null and a string are three different requests here, and only
	// json.RawMessage can tell the first two apart.
	switch raw := strings.TrimSpace(string(req.Claude.Credentials)); {
	case raw == "":
	case raw == "null":
		patch.ClaudeCredentialsDefault = true
	default:
		var value string
		if err := json.Unmarshal(req.Claude.Credentials, &value); err != nil {
			return config.Patch{}, errors.New("claude.credentials must be a path, an empty string for no mount, or null for the default")
		}
		patch.ClaudeCredentials = text(&value, current.ClaudeCredentials)
	}
	return patch, nil
}

// text prepares a string field: absent stays absent, and a value the server
// already resolves to is dropped.
func text(want *string, current string) *string {
	if want == nil {
		return nil
	}
	value := strings.TrimSpace(*want)
	if value == current {
		return nil
	}
	return &value
}

// secret prepares one of the two secrets in the file. An empty field is the
// form saying it has nothing to offer — it is never shown their values — so it
// means "leave the stored one alone" and not "clear it".
func secret(want *string, current string) *string {
	if want == nil || strings.TrimSpace(*want) == "" {
		return nil
	}
	return text(want, current)
}

func flag(want *bool, current bool) *bool {
	if want == nil || *want == current {
		return nil
	}
	return want
}

func number(want *int, current int) *int {
	if want == nil || *want == current {
		return nil
	}
	return want
}

func list(want *[]string, current []string) *[]string {
	if want == nil {
		return nil
	}
	value := trimAll(*want)
	if slices.Equal(value, current) {
		return nil
	}
	return &value
}

// checkPublicURL refuses a public URL that could not serve as an origin.
//
// config.Load takes any string, and an unusable one surfaces much later as an
// OAuth redirect GitHub never matches, or as an origin check nothing satisfies.
// Both are the kind of failure that leaves an operator with no page to fix it
// from, so it is caught while they are still looking at the form.
func checkPublicURL(raw string) error {
	parsed, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("public URL %q: %w", raw, err)
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return fmt.Errorf("public URL %q: want an http:// or https:// address", raw)
	case parsed.Host == "":
		return fmt.Errorf("public URL %q: want a host, as in https://hexagon.example.com", raw)
	case strings.Trim(parsed.Path, "/") != "":
		return fmt.Errorf("public URL %q: want an origin, with no path after the host", raw)
	}
	return nil
}
