package httpapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/config"
)

// maxSetupRequestBody bounds the first-time wizard's form. It carries a
// password, two short OAuth values and a list of GitHub logins.
const maxSetupRequestBody = 8 << 10

// setupStatusResponse describes the first-time wizard to the SPA.
//
// Everything but required is omitted when it is empty, and the SPA reads none
// of it unless required is true: while the wizard is closed this route says so
// and nothing else, because the path of a configuration file is a fact about
// the machine and this route answers anyone who can reach the port. A missing
// writable therefore means false, which is what a client should assume anyway.
type setupStatusResponse struct {
	Required    bool   `json:"required"`
	ConfigPath  string `json:"configPath,omitempty"`
	Writable    bool   `json:"writable,omitempty"`
	CallbackURL string `json:"callbackUrl,omitempty"`
	// ClientID and AllowedUsers are what the server is running on, not what was
	// last submitted: an environment variable outranks the file the wizard
	// writes, and an operator has to be able to see that rather than guess it.
	ClientID     string   `json:"clientId,omitempty"`
	AllowedUsers []string `json:"allowedUsers,omitempty"`
	// FromEnvironment names the settings a variable is supplying, by their
	// configuration file key. Writing those from here has no effect until the
	// variable goes away.
	FromEnvironment []string `json:"fromEnvironment,omitempty"`
}

// setupRequest is the wizard's form. The password authenticates the call: there
// is no session yet, and nothing else about this server can identify a caller.
type setupRequest struct {
	Password     string   `json:"password"`
	ClientID     string   `json:"clientId"`
	ClientSecret string   `json:"clientSecret"`
	AllowedUsers []string `json:"allowedUsers"`
}

// handleSetupStatus reports whether the first-time wizard has anything left to
// do, and what it would be working with.
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	required, err := s.setupRequired(r.Context())
	if err != nil {
		s.log.Error("check whether setup is required", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read the configuration")
		return
	}
	if !required {
		writeJSON(w, http.StatusOK, setupStatusResponse{Required: false})
		return
	}
	writeJSON(w, http.StatusOK, s.setupStatus())
}

// handleSetup writes the OAuth settings the wizard collected and reconfigures
// the running server with them.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	required, err := s.setupRequired(r.Context())
	if err != nil {
		s.log.Error("check whether setup is required", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read the configuration")
		return
	}
	if !required {
		writeError(w, http.StatusConflict, "this server is already set up: change these settings from the settings page")
		return
	}

	var req setupRequest
	if err := decodeJSON(w, r, maxSetupRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !s.setup.Verify(req.Password) {
		s.log.Warn("first-time setup refused: wrong password", "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, "that is not the password in the server log")
		return
	}

	clientID := strings.TrimSpace(req.ClientID)
	clientSecret := strings.TrimSpace(req.ClientSecret)
	allowedUsers := trimAll(req.AllowedUsers)
	switch {
	case clientID == "":
		writeError(w, http.StatusBadRequest, "a GitHub OAuth client id is required")
		return
	case clientSecret == "":
		writeError(w, http.StatusBadRequest, "a GitHub OAuth client secret is required")
		return
	case len(allowedUsers) == 0:
		writeError(w, http.StatusBadRequest, "list at least one GitHub account allowed to sign in")
		return
	}

	// The wizard is a settings save for a server nobody can sign in to yet, so
	// it goes through the same apply: written only once it is known to be
	// usable, because a configuration file that cannot build a login is worse
	// than no configuration file — the wizard would be writing over its own way
	// out. There is no caller to lock out of an allowlist here, which is what
	// the nil user says.
	if err := s.applySettings(r.Context(), nil, config.Patch{
		GitHubClientID:     &clientID,
		GitHubClientSecret: &clientSecret,
		AllowedUsers:       &allowedUsers,
	}); err != nil {
		var rejected settingsError
		if errors.As(err, &rejected) {
			writeError(w, http.StatusBadRequest, rejected.Error())
			return
		}
		s.log.Error("write the configuration file", "path", s.cfg.ConfigPath, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.log.Info("configured by the first-time wizard", "file", s.cfg.ConfigPath)
	writeJSON(w, http.StatusOK, s.setupStatus())
}

// setupRequired reports whether the first-time wizard is still open.
//
// The condition is that nobody has ever signed in, and not that the settings
// look incomplete. The two differ exactly where it matters: an operator who
// saved a client secret with a typo has settings that look complete and a
// server nobody can get into, and this is what leaves them a way back in. It is
// asked per request rather than answered once at startup, so the wizard closes
// the moment a login succeeds.
func (s *Server) setupRequired(ctx context.Context) (bool, error) {
	if s.setup == nil {
		return false, nil
	}
	signedIn, err := s.store.AnyUser(ctx)
	if err != nil {
		return false, err
	}
	return !signedIn, nil
}

// setupStatus describes the settings the server is actually running on.
func (s *Server) setupStatus() setupStatusResponse {
	status := setupStatusResponse{
		Required:    true,
		ConfigPath:  s.cfg.ConfigPath,
		Writable:    config.Writable(s.cfg.ConfigPath),
		CallbackURL: auth.CallbackURL(s.cfg.PublicURL),
		// Before the wizard has run these are whatever the file and the
		// environment supplied, which may be some of what is needed.
		ClientID:     s.cfg.GitHubClientID,
		AllowedUsers: s.cfg.AllowedUsers,
	}
	if oauth := s.gate.OAuth(); oauth != nil {
		status.ClientID = oauth.ClientID()
	}
	if allowlist := s.gate.Allowlist(); allowlist != nil {
		status.AllowedUsers = allowlist.Entries()
	}
	for _, shadowed := range []struct{ variable, key string }{
		{"HEXAGON_GITHUB_CLIENT_ID", "clientId"},
		{"HEXAGON_GITHUB_CLIENT_SECRET", "clientSecret"},
		{"HEXAGON_ALLOWED_USERS", "allowedUsers"},
	} {
		if os.Getenv(shadowed.variable) != "" {
			status.FromEnvironment = append(status.FromEnvironment, shadowed.key)
		}
	}
	return status
}

func trimAll(values []string) []string {
	var out []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
