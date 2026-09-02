package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
	"github.com/coder/websocket"
)

// maxClaudeRequestBody bounds a claude account request: a name, a kind and a
// token.
const maxClaudeRequestBody = 8 << 10

type claudeStatusResponse struct {
	// File describes the credentials file the machine-wide login writes, and
	// the mount a session with no account falls back to.
	File claudeFileResponse `json:"file"`
	// Effective names what a new session naming no account will actually
	// authenticate with: "account", "apiKey", "file" or "none" — the user's
	// default account first, then the two configuration fallbacks, then
	// nothing.
	Effective string `json:"effective"`
	// CanLogin is false when no credentials path is configured: there would be
	// nowhere for the machine-wide browser login to write.
	CanLogin bool `json:"canLogin"`
	// CanVerify is false when there is no way to run the CLI at all, in which
	// case a credential is stored unchecked — the canAsk rule from the Images
	// page.
	CanVerify bool `json:"canVerify"`
	// InContainer says the CLI runs in a container because this server has no
	// claude binary. It is slower, and the pages that use it say so rather than
	// leaving a user to wonder.
	InContainer bool `json:"inContainer"`
	// DefaultImage is the image Hexagon builds for itself, which is what a
	// browser login runs in when the user has built none of their own. Absent
	// when there is no Docker to build one with.
	DefaultImage *defaultImageResponse `json:"defaultImage,omitempty"`
}

// defaultImageResponse is the state of that image: usable now, being built, or
// failed with a reason.
type defaultImageResponse struct {
	Ready    bool   `json:"ready"`
	Building bool   `json:"building"`
	Error    string `json:"error,omitempty"`
}

type claudeFileResponse struct {
	Path      string    `json:"path"`
	Present   bool      `json:"present"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// claudeStatus builds the status shape the Accounts page reads on load.
func (s *Server) claudeStatus(ctx context.Context, userID string) (claudeStatusResponse, error) {
	status := claudeStatusResponse{
		CanLogin:    s.cfg.ClaudeCredentials != "",
		CanVerify:   s.editor != nil,
		InContainer: s.editorInContainer,
	}
	// Asking for the state is what starts the build, so this is also where a
	// server that has never needed the image begins to make one: the Accounts
	// page is where a user goes to log in to Claude Code, and the login needs a
	// container to run in.
	if s.defaultImage != nil {
		image := s.defaultImage.State()
		status.DefaultImage = &defaultImageResponse{
			Ready: image.Ready, Building: image.Building, Error: image.Error,
		}
	}

	status.File.Path = s.cfg.ClaudeCredentials
	if info, err := os.Stat(s.cfg.ClaudeCredentials); err == nil {
		status.File.Present = true
		status.File.UpdatedAt = info.ModTime()
	}

	_, err := s.store.DefaultClaudeAccount(ctx, userID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		switch {
		case s.cfg.AnthropicAPIKey != "":
			status.Effective = "apiKey"
		case status.File.Present:
			status.Effective = "file"
		default:
			status.Effective = "none"
		}
	case err != nil:
		return claudeStatusResponse{}, err
	default:
		status.Effective = "account"
	}
	return status, nil
}

// handleClaudeStatus reports the state a new session naming no account would
// authenticate with, and what the Accounts page needs to offer a login.
func (s *Server) handleClaudeStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.claudeStatus(r.Context(), s.user(r).ID)
	if err != nil {
		s.log.Error("build claude status", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read the claude status")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// claudeAccountResponse is one account, without a secret field: that is the
// whole point of this type existing rather than serializing store.ClaudeAccount
// directly.
type claudeAccountResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	IsDefault bool      `json:"default"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Login is present only for a login account: whether anybody has signed in
	// on it yet, and when. A row can exist before that happens.
	Login *claudeAccountLoginResponse `json:"login,omitempty"`
}

type claudeAccountLoginResponse struct {
	Present   bool      `json:"present"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

func newClaudeAccountResponse(a *store.ClaudeAccount, accountsDir string) claudeAccountResponse {
	out := claudeAccountResponse{
		ID: a.ID, Name: a.Name, Kind: a.Kind, IsDefault: a.IsDefault,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
	if a.Kind == store.ClaudeAccountKindLogin {
		out.Login = &claudeAccountLoginResponse{}
		if info, err := os.Stat(filepath.Join(accountsDir, a.ID, ".credentials.json")); err == nil {
			out.Login.Present = true
			out.Login.UpdatedAt = info.ModTime()
		}
	}
	return out
}

// handleListClaudeAccounts lists every Claude account this user has
// configured.
func (s *Server) handleListClaudeAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListClaudeAccounts(r.Context(), s.user(r).ID)
	if err != nil {
		s.log.Error("list claude accounts", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot list claude accounts")
		return
	}
	out := make([]claudeAccountResponse, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, newClaudeAccountResponse(a, s.cfg.ClaudeAccountsDir))
	}
	writeJSON(w, http.StatusOK, out)
}

// claudeAccountKinds are the values a create request may name, in the order
// they are quoted in an error message.
var claudeAccountKinds = []string{claudex.KindAPIKey, claudex.KindOAuthToken, store.ClaudeAccountKindLogin}

func isClaudeAccountKind(kind string) bool {
	for _, k := range claudeAccountKinds {
		if kind == k {
			return true
		}
	}
	return false
}

type createClaudeAccountRequest struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Secret string `json:"secret"`
}

// handleCreateClaudeAccount adds a new Claude account. A secret kind is
// checked against the CLI before it is stored, exactly as setting the old
// single credential did and for the same reason — a mistyped token stored
// anyway shows up later as a session that cannot sign in, with no clue why.
// The first account a user creates becomes their default.
func (s *Server) handleCreateClaudeAccount(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	var req createClaudeAccountRequest
	if err := decodeJSON(w, r, maxClaudeRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	kind := strings.TrimSpace(req.Kind)
	secret := strings.TrimSpace(req.Secret)
	switch {
	case name == "":
		writeError(w, http.StatusBadRequest, "a name is required")
		return
	case !isClaudeAccountKind(kind):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("kind must be one of %v", claudeAccountKinds))
		return
	case kind == store.ClaudeAccountKindLogin && secret != "":
		writeError(w, http.StatusBadRequest, "a login account takes no secret")
		return
	case kind != store.ClaudeAccountKindLogin && secret == "":
		writeError(w, http.StatusBadRequest, "a secret is required")
		return
	}

	if kind != store.ClaudeAccountKindLogin && s.editor != nil {
		ctx, cancel := context.WithTimeout(r.Context(), sourceEditTimeout)
		defer cancel()
		if err := s.editor.Check(ctx, claudex.Credential{Kind: kind, Secret: secret}); err != nil {
			// The CLI's own words, not ours: what is wrong with a credential is
			// something only it knows.
			writeError(w, http.StatusBadRequest, "that credential was rejected — "+err.Error())
			return
		}
	}

	existing, err := s.store.ListClaudeAccounts(r.Context(), user.ID)
	if err != nil {
		s.log.Error("list claude accounts", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot create the account")
		return
	}

	account, err := s.auth.CreateClaudeAccount(r.Context(), user.ID, name, kind, secret, len(existing) == 0)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "you already have an account with that name")
		return
	case err != nil:
		s.log.Error("create claude account", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot create the account")
		return
	}

	s.log.Info("claude account created", "login", user.GitHubLogin, "account", account.ID, "kind", kind)
	writeJSON(w, http.StatusCreated, newClaudeAccountResponse(account, s.cfg.ClaudeAccountsDir))
}

type updateClaudeAccountRequest struct {
	Name      *string `json:"name"`
	Secret    *string `json:"secret"`
	IsDefault *bool   `json:"default"`
}

// handleUpdateClaudeAccount renames an account, replaces its secret, or makes
// it the default — any combination of the three in one request.
func (s *Server) handleUpdateClaudeAccount(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)
	account, ok := s.claudeAccountOr404(w, r)
	if !ok {
		return
	}

	var req updateClaudeAccountRequest
	if err := decodeJSON(w, r, maxClaudeRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "a name is required")
			return
		}
		switch err := s.store.RenameClaudeAccount(r.Context(), user.ID, account.ID, name); {
		case errors.Is(err, store.ErrConflict):
			writeError(w, http.StatusConflict, "you already have an account with that name")
			return
		case err != nil:
			s.log.Error("rename claude account", "err", err)
			writeError(w, http.StatusInternalServerError, "cannot rename the account")
			return
		}
	}

	if req.Secret != nil {
		if account.Kind == store.ClaudeAccountKindLogin {
			writeError(w, http.StatusBadRequest, "a login account has no secret to replace")
			return
		}
		secret := strings.TrimSpace(*req.Secret)
		if secret == "" {
			writeError(w, http.StatusBadRequest, "a secret is required")
			return
		}
		if s.editor != nil {
			ctx, cancel := context.WithTimeout(r.Context(), sourceEditTimeout)
			defer cancel()
			if err := s.editor.Check(ctx, claudex.Credential{Kind: account.Kind, Secret: secret}); err != nil {
				writeError(w, http.StatusBadRequest, "that credential was rejected — "+err.Error())
				return
			}
		}
		if err := s.auth.SetClaudeAccountSecret(r.Context(), user.ID, account.ID, secret); err != nil {
			s.log.Error("replace claude account secret", "err", err)
			writeError(w, http.StatusInternalServerError, "cannot replace the secret")
			return
		}
	}

	if req.IsDefault != nil && *req.IsDefault {
		if err := s.store.SetDefaultClaudeAccount(r.Context(), user.ID, account.ID); err != nil {
			s.log.Error("set default claude account", "err", err)
			writeError(w, http.StatusInternalServerError, "cannot make the account the default")
			return
		}
	}

	account, err := s.store.ClaudeAccountByID(r.Context(), user.ID, account.ID)
	if err != nil {
		s.log.Error("load claude account", "id", account.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot load the account")
		return
	}
	writeJSON(w, http.StatusOK, newClaudeAccountResponse(account, s.cfg.ClaudeAccountsDir))
}

// handleDeleteClaudeAccount removes an account, refusing while a session still
// names it — the same rule CountSessionsUsingImage gives images, and for the
// same reason: a stopped session can be started, and rebuilt, from what its row
// names.
func (s *Server) handleDeleteClaudeAccount(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)
	account, ok := s.claudeAccountOr404(w, r)
	if !ok {
		return
	}

	inUse, err := s.store.CountSessionsUsingClaudeAccount(r.Context(), account.ID)
	if err != nil {
		s.log.Error("count sessions using claude account", "id", account.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot delete the account")
		return
	}
	if inUse > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("account is used by %d session(s)", inUse))
		return
	}

	if err := s.store.DeleteClaudeAccount(r.Context(), user.ID, account.ID); err != nil {
		s.log.Error("delete claude account", "id", account.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot delete the account")
		return
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.ClaudeAccountsDir, account.ID)); err != nil {
		s.log.Warn("remove claude account directory", "id", account.ID, "err", err)
	}
	s.log.Info("claude account deleted", "login", user.GitHubLogin, "account", account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// claudeAccountOr404 loads the account named in the path, scoped to the
// caller.
func (s *Server) claudeAccountOr404(w http.ResponseWriter, r *http.Request) (*store.ClaudeAccount, bool) {
	account, err := s.store.ClaudeAccountByID(r.Context(), s.user(r).ID, r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such claude account")
		return nil, false
	case err != nil:
		s.log.Error("load claude account", "id", r.PathValue("id"), "err", err)
		writeError(w, http.StatusInternalServerError, "cannot load the account")
		return nil, false
	}
	return account, true
}

// editorCredential is what the source editor authenticates Claude Code with:
// exactly what a session naming no account would, all the way down to the
// server's own configuration. It exists because the two used to disagree — the
// editor was handed the stored credential and nothing else, so a user who had
// signed in through the browser got a working session and an editor that
// answered "Not logged in", for the same account, on the same server.
func (s *Server) editorCredential(ctx context.Context, userID string) (claudex.Credential, error) {
	return s.sessions.ClaudeCredential(ctx, userID)
}

// loginImage resolves what a browser login runs in, and answers the request
// itself when there is nothing to run it in.
//
// A request that names no image gets Hexagon's own, which is the whole point:
// the login needs a container with Claude Code in it, and on a fresh install the
// user has not built one yet — that used to be a dialog with an empty menu and
// no way forward.
func (s *Server) loginImage(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("image"))
	if id == "" {
		if s.defaultImage == nil {
			writeError(w, http.StatusServiceUnavailable,
				"this server has no docker to build the default image with: build an image first")
			return "", false
		}
		switch image := s.defaultImage.State(); {
		case image.Ready:
			return image.Ref, true
		case image.Error != "":
			writeError(w, http.StatusServiceUnavailable, "the default image could not be built: "+image.Error)
		default:
			writeError(w, http.StatusConflict,
				"the default image is still being built — try again in a minute, or pick one of your own")
		}
		return "", false
	}

	img, err := s.store.ImageByID(r.Context(), s.user(r).ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such image")
	case err != nil:
		s.log.Error("load image", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot load image")
	case img.Status != store.ImageStatusReady:
		writeError(w, http.StatusConflict, fmt.Sprintf("image is %s, not ready", img.Status))
	default:
		return img.ImageRef, true
	}
	return "", false
}

// attachClaudeLoginTerminal bridges a browser WebSocket to containerID's tmux
// login session. Shared by the machine-wide login and an account's own: both
// run the same `claude auth login` and the same tmux dance once the container
// is up, and only how the container's directory was chosen differs.
func (s *Server) attachClaudeLoginTerminal(w http.ResponseWriter, r *http.Request, containerID string) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.originPatterns(),
	})
	if err != nil {
		s.log.Warn("claude login handshake failed", "err", err)
		return
	}
	conn.SetReadLimit(terminalReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	exec, err := s.docker.AttachExec(ctx, dockerx.ExecRequest{
		ContainerID: containerID,
		Cmd: []string{"tmux", "-u", "new-session", "-A", "-D", "-s", session.ClaudeLoginTmux,
			"-c", dockerx.AgentHome, session.ClaudeLoginCommand},
		Env:  []string{"TERM=xterm-256color", "LANG=C.UTF-8"},
		Size: sizeFromQuery(r.URL.Query()),
	})
	if err != nil {
		s.log.Error("attach claude login terminal", "err", err)
		conn.Close(websocket.StatusInternalError, "cannot attach to the login container")
		return
	}

	s.log.Info("claude login terminal attached", "exec", exec.ID)
	s.pumpTerminal(ctx, conn, exec)
	exec.Close()
	conn.Close(websocket.StatusNormalClosure, "")
	s.log.Info("claude login terminal detached", "exec", exec.ID)
}

// handleClaudeLoginTerminal bridges a browser WebSocket to a container running
// only for the length of the machine-wide browser login: `claude` inside it
// writes the host credentials file directly, which is the mechanism the whole
// feature rests on.
func (s *Server) handleClaudeLoginTerminal(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	imageRef, ok := s.loginImage(w, r)
	if !ok {
		return
	}
	// A browser always sends Origin on a WebSocket handshake, so this endpoint
	// asks for the header itself instead of taking the Sec-Fetch-Site signal
	// guardStateChanges also accepts: it hands out a shell.
	if !s.originMatches(r.Header.Get("Origin")) {
		writeError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}

	containerID, err := s.sessions.StartClaudeLogin(r.Context(), user.ID, imageRef)
	switch {
	case errors.Is(err, session.ErrClaudeLoginUnavailable):
		writeError(w, http.StatusConflict, "no claude credentials path is configured")
		return
	case err != nil:
		s.log.Error("start claude login", "login", user.GitHubLogin, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot start the login container")
		return
	}
	s.attachClaudeLoginTerminal(w, r, containerID)
}

// handleStopClaudeLogin removes the machine-wide login container. It is what
// the dialog calls when it closes; the socket closing does not remove
// anything, because a page reload is a closed socket too.
func (s *Server) handleStopClaudeLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.StopClaudeLogin(r.Context(), s.user(r).ID); err != nil {
		s.log.Error("stop claude login", "login", s.user(r).GitHubLogin, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot stop the login container")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleClaudeAccountLoginTerminal is handleClaudeLoginTerminal for one Claude
// account: what Claude Code writes goes into that account's own directory
// rather than the machine-wide one, so two accounts can be signed in to at once
// without either overwriting the other.
func (s *Server) handleClaudeAccountLoginTerminal(w http.ResponseWriter, r *http.Request) {
	account, ok := s.claudeAccountOr404(w, r)
	if !ok {
		return
	}
	if account.Kind != store.ClaudeAccountKindLogin {
		writeError(w, http.StatusConflict, "only a login account can sign in through the browser")
		return
	}

	imageRef, ok := s.loginImage(w, r)
	if !ok {
		return
	}
	if !s.originMatches(r.Header.Get("Origin")) {
		writeError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}

	containerID, err := s.sessions.StartAccountClaudeLogin(r.Context(), account.ID, imageRef)
	if err != nil {
		s.log.Error("start claude account login", "account", account.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot start the login container")
		return
	}
	s.attachClaudeLoginTerminal(w, r, containerID)
}

// handleStopClaudeAccountLogin removes one account's login container.
func (s *Server) handleStopClaudeAccountLogin(w http.ResponseWriter, r *http.Request) {
	account, ok := s.claudeAccountOr404(w, r)
	if !ok {
		return
	}
	if err := s.sessions.StopAccountClaudeLogin(r.Context(), account.ID); err != nil {
		s.log.Error("stop claude account login", "account", account.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot stop the login container")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
