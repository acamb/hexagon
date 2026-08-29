package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
	"github.com/coder/websocket"
)

// maxClaudeRequestBody bounds a credential: a kind and a token.
const maxClaudeRequestBody = 8 << 10

type claudeStatusResponse struct {
	// Credential is the credential this user pasted, without its secret.
	Credential *claudeCredentialResponse `json:"credential"`
	// File describes the credentials file the session mount points at.
	File claudeFileResponse `json:"file"`
	// Effective names what a new session will actually authenticate with:
	// "credential", "apiKey", "file" or "none". Inside the container an
	// environment variable wins over the mounted file, so a pasted credential
	// shadows a browser login, and the page has to be able to say so.
	Effective string `json:"effective"`
	// CanLogin is false when no credentials path is configured: there would be
	// nowhere for a browser login to write.
	CanLogin bool `json:"canLogin"`
	// CanVerify is false without a host claude binary, in which case a
	// credential is stored unchecked — the canAsk rule from the Images page.
	CanVerify bool `json:"canVerify"`
}

type claudeCredentialResponse struct {
	Kind      string    `json:"kind"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type claudeFileResponse struct {
	Path      string    `json:"path"`
	Present   bool      `json:"present"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// claudeStatus builds the status shape shared by the GET and the PUT: the page
// has one response to render, whichever endpoint produced it.
func (s *Server) claudeStatus(ctx context.Context, userID string) (claudeStatusResponse, error) {
	status := claudeStatusResponse{
		CanLogin:  s.cfg.ClaudeCredentials != "",
		CanVerify: s.editor != nil,
	}

	stored, err := s.store.ClaudeCredential(ctx, userID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return claudeStatusResponse{}, err
	default:
		status.Credential = &claudeCredentialResponse{Kind: stored.Kind, UpdatedAt: stored.UpdatedAt}
	}

	status.File.Path = s.cfg.ClaudeCredentials
	if info, err := os.Stat(s.cfg.ClaudeCredentials); err == nil {
		status.File.Present = true
		status.File.UpdatedAt = info.ModTime()
	}

	switch {
	case status.Credential != nil:
		status.Effective = "credential"
	case s.cfg.AnthropicAPIKey != "":
		status.Effective = "apiKey"
	case status.File.Present:
		status.Effective = "file"
	default:
		status.Effective = "none"
	}
	return status, nil
}

// handleClaudeStatus reports the Claude login this user has configured, and
// what a new session will actually authenticate with.
func (s *Server) handleClaudeStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.claudeStatus(r.Context(), s.user(r).ID)
	if err != nil {
		s.log.Error("build claude status", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read the claude status")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

type setClaudeCredentialRequest struct {
	Kind   string `json:"kind"`
	Secret string `json:"secret"`
}

// handleSetClaudeCredential stores a pasted credential after checking it
// against the CLI. Verifying first is the point: a mistyped token that was
// stored anyway would show up later as a session with a Claude Code that
// cannot sign in, with no clue as to why.
func (s *Server) handleSetClaudeCredential(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	var req setClaudeCredentialRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxClaudeRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	kind := strings.TrimSpace(req.Kind)
	secret := strings.TrimSpace(req.Secret)
	switch {
	case kind != claudex.KindAPIKey && kind != claudex.KindOAuthToken:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("kind must be %q or %q", claudex.KindAPIKey, claudex.KindOAuthToken))
		return
	case secret == "":
		writeError(w, http.StatusBadRequest, "a secret is required")
		return
	}
	cred := claudex.Credential{Kind: kind, Secret: secret}

	if s.editor != nil {
		ctx, cancel := context.WithTimeout(r.Context(), dockerfileEditTimeout)
		defer cancel()
		if err := s.editor.Check(ctx, cred); err != nil {
			// The CLI's own words, not ours: what is wrong with a credential is
			// something only it knows.
			writeError(w, http.StatusBadRequest, "that credential was rejected — "+err.Error())
			return
		}
	}

	if err := s.auth.SetClaudeCredential(r.Context(), user.ID, cred); err != nil {
		s.log.Error("set claude credential", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot save the credential")
		return
	}

	status, err := s.claudeStatus(r.Context(), user.ID)
	if err != nil {
		s.log.Error("build claude status", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read the claude status")
		return
	}
	s.log.Info("claude credential stored", "login", user.GitHubLogin, "kind", kind)
	writeJSON(w, http.StatusOK, status)
}

// handleForgetClaudeCredential removes a pasted credential. It has no effect on
// a browser login: that credential is a file on the host, not a database row.
func (s *Server) handleForgetClaudeCredential(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)
	err := s.auth.ForgetClaudeCredential(r.Context(), user.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no claude credential is stored")
		return
	case err != nil:
		s.log.Error("forget claude credential", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot forget the credential")
		return
	}
	s.log.Info("claude credential forgotten", "login", user.GitHubLogin)
	w.WriteHeader(http.StatusNoContent)
}

// handleClaudeLoginTerminal bridges a browser WebSocket to a container running
// only for the length of the browser login: `claude` inside it writes the host
// credentials file directly, which is the mechanism the whole feature rests on.
func (s *Server) handleClaudeLoginTerminal(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	img, err := s.store.ImageByID(r.Context(), user.ID, r.URL.Query().Get("image"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such image")
		return
	case err != nil:
		s.log.Error("load image", "id", r.URL.Query().Get("image"), "err", err)
		writeError(w, http.StatusInternalServerError, "cannot load image")
		return
	case img.Status != store.ImageStatusReady:
		writeError(w, http.StatusConflict, fmt.Sprintf("image is %s, not ready", img.Status))
		return
	}

	// A browser always sends Origin on a WebSocket handshake, so this endpoint
	// asks for the header itself instead of taking the Sec-Fetch-Site signal
	// guardStateChanges also accepts: it hands out a shell.
	if !s.originMatches(r.Header.Get("Origin")) {
		writeError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}

	containerID, err := s.sessions.StartClaudeLogin(r.Context(), user.ID, img.ImageRef)
	switch {
	case errors.Is(err, session.ErrClaudeLoginUnavailable):
		writeError(w, http.StatusConflict, "no claude credentials path is configured")
		return
	case err != nil:
		s.log.Error("start claude login", "login", user.GitHubLogin, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot start the login container")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.originPatterns(),
	})
	if err != nil {
		s.log.Warn("claude login handshake failed", "login", user.GitHubLogin, "err", err)
		return
	}
	conn.SetReadLimit(terminalReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	exec, err := s.docker.AttachExec(ctx, dockerx.ExecRequest{
		ContainerID: containerID,
		Cmd: []string{"tmux", "new-session", "-A", "-D", "-s", session.ClaudeLoginTmux,
			"-c", dockerx.AgentHome, session.ClaudeLoginCommand},
		Env:  []string{"TERM=xterm-256color"},
		Size: sizeFromQuery(r.URL.Query()),
	})
	if err != nil {
		s.log.Error("attach claude login terminal", "login", user.GitHubLogin, "err", err)
		conn.Close(websocket.StatusInternalError, "cannot attach to the login container")
		return
	}

	s.log.Info("claude login terminal attached", "login", user.GitHubLogin, "exec", exec.ID)
	s.pumpTerminal(ctx, conn, exec)
	exec.Close()
	conn.Close(websocket.StatusNormalClosure, "")
	s.log.Info("claude login terminal detached", "login", user.GitHubLogin, "exec", exec.ID)
}

// handleStopClaudeLogin removes the login container. It is what the dialog
// calls when it closes; the socket closing does not remove anything, because a
// page reload is a closed socket too.
func (s *Server) handleStopClaudeLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.StopClaudeLogin(r.Context(), s.user(r).ID); err != nil {
		s.log.Error("stop claude login", "login", s.user(r).GitHubLogin, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot stop the login container")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
