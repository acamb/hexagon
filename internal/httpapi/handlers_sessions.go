package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/provider"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
)

// maxSessionRequestBody bounds a create request. It carries a few short
// strings, nothing more.
const maxSessionRequestBody = 8 << 10

type sessionResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Provider is the account the session is attached to, empty when it is
	// attached to none. RepoFullName and Branch are empty together, for a
	// session that started on an empty workspace rather than on a clone.
	Provider     string `json:"provider"`
	RepoFullName string `json:"repoFullName"`
	Branch       string `json:"branch"`
	ImageID      string `json:"imageId"`
	ImageRef     string `json:"imageRef"`
	// RepoDir is the directory mounted at /workspace, clone or not.
	RepoDir    string `json:"repoDir"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	AutoClaude bool   `json:"autoClaude"`
	// PropagateToken is reported but never updated: the container carries the
	// environment it was created with.
	PropagateToken bool `json:"propagateToken"`
	// VSCode is reported but never updated: the mount and the port binding are
	// the container, and there is no way to add them to one that exists.
	VSCode bool `json:"vscode"`
	// Ports pairs each container port the session publishes with the host port
	// Docker gave it. Reported and never updated, for the same reason as VSCode
	// above.
	Ports []sessionPort `json:"ports"`
	// Compose is whether this session is a project rather than a single
	// container, which is a property of the image it came from.
	Compose   bool      `json:"compose"`
	CreatedAt time.Time `json:"createdAt"`
}

// sessionPort is one published port. Host is absent while the session is not
// running, because that is the truth: there is no binding to report until the
// container is up, and Docker picks a new one every time it starts.
type sessionPort struct {
	Container int `json:"container"`
	Host      int `json:"host,omitempty"`
}

func newSessionResponse(s *store.Session, hostPorts map[int]int) sessionResponse {
	ports := make([]sessionPort, 0, len(s.Ports))
	for _, container := range s.Ports {
		ports = append(ports, sessionPort{Container: container, Host: hostPorts[container]})
	}
	return sessionResponse{
		ID:             s.ID,
		Title:          s.Title,
		Provider:       s.Provider,
		RepoFullName:   s.RepoFullName,
		Branch:         s.Branch,
		ImageID:        s.ImageID,
		ImageRef:       s.ImageRef,
		RepoDir:        s.RepoDir,
		Status:         s.Status,
		Error:          s.Error,
		AutoClaude:     s.AutoClaude,
		PropagateToken: s.PropagateToken,
		VSCode:         s.VSCode,
		Ports:          ports,
		Compose:        s.Compose,
		CreatedAt:      s.CreatedAt,
	}
}

// handleListSessions returns the caller's sessions, reconciled against Docker.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions(r.Context(), s.user(r).ID)
	if err != nil {
		s.log.Error("list sessions", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot list sessions")
		return
	}
	s.sessions.RefreshAll(r.Context(), sessions)

	out := make([]sessionResponse, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, newSessionResponse(session, s.sessions.PublishedPorts(r.Context(), session)))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetSession describes one session. The terminal view reads it to decide
// whether to open a socket at all.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	found, ok := s.sessionOr404(w, r)
	if !ok {
		return
	}
	s.writeSession(w, r, http.StatusOK, s.sessions.Refresh(r.Context(), found))
}

// writeSession answers with one session, looking its published ports up as it
// goes. They are never stored: Docker picks a new host port every time a
// container starts.
func (s *Server) writeSession(w http.ResponseWriter, r *http.Request, status int, found *store.Session) {
	writeJSON(w, status, newSessionResponse(found, s.sessions.PublishedPorts(r.Context(), found)))
}

type createSessionRequest struct {
	// Provider is which connected account the repository comes from. An empty
	// one matches on name alone, across every account.
	//
	// Without a repository it means something else, because there is no
	// repository to have come from anywhere: it names the account whose
	// credentials the session gets, and empty means none.
	Provider string `json:"provider"`
	// RepoFullName is optional. A request without it creates a session on an
	// empty workspace rather than on a clone.
	RepoFullName string `json:"repoFullName"`
	Branch       string `json:"branch"`
	ImageID      string `json:"imageId"`
	Title        string `json:"title"`
	// AutoClaude is a pointer so that a client which has never heard of it gets
	// the default — Claude Code started for them — rather than a bare shell.
	AutoClaude *bool `json:"autoClaude"`
	// PropagateToken hands the account's credentials to the container. A
	// pointer for the same reason, and defaulting to on: Hexagon is for running
	// an agent that commits and pushes, and the switch is for the session where
	// that is not wanted. It is only settable here — see handleUpdateSession.
	PropagateToken *bool `json:"propagateToken"`
	// VSCode asks for the container to publish code-server and have the
	// release bind mounted. Unlike the two flags above it defaults to off: it
	// costs a mount and a published port, and a session that never opens the
	// editor should carry neither. Only settable here — the mount and the port
	// binding are the container, and there is no way to add them afterwards.
	VSCode *bool `json:"vscode"`
	// Ports are container ports to publish on the host's loopback interface,
	// with the host side left to Docker. Only settable here, like VSCode above:
	// a container keeps the port bindings it was created with.
	Ports []int `json:"ports"`
}

// handleCreateSession starts provisioning a session and returns straight away;
// the work happens in the background and shows up as the session's status.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := decodeJSON(w, r, maxSessionRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.ImageID) == "" {
		writeError(w, http.StatusBadRequest, "an image is required")
		return
	}

	// Before anything is provisioned: a session is a container, a clone on disk
	// and a workspace directory, and refusing after any of that exists would
	// leave the mess behind.
	switch live, err := s.store.CountSessions(r.Context(), s.user(r).ID); {
	case err != nil:
		s.log.Error("count sessions", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot create session")
		return
	case live >= s.cfg.MaxSessionsPerUser:
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf(
			"at most %d sessions at a time: delete one first", s.cfg.MaxSessionsPerUser))
		return
	}

	if err := s.docker.Ping(r.Context()); err != nil {
		s.log.Error("docker unreachable", "err", err)
		writeError(w, http.StatusServiceUnavailable, "docker is unreachable")
		return
	}

	create := session.CreateRequest{
		Title:          req.Title,
		ImageID:        req.ImageID,
		AutoClaude:     req.AutoClaude == nil || *req.AutoClaude,
		PropagateToken: req.PropagateToken == nil || *req.PropagateToken,
		VSCode:         req.VSCode != nil && *req.VSCode,
		Ports:          req.Ports,
	}
	if fullName := strings.TrimSpace(req.RepoFullName); fullName != "" {
		// The clone URL is never taken from the request: it is looked up in the
		// caller's own accounts, so a session can only ever clone something
		// they actually have.
		repo, ok := s.resolveRepo(w, r, provider.Kind(req.Provider), fullName)
		if !ok {
			return
		}
		branch := strings.TrimSpace(req.Branch)
		if branch == "" {
			branch = repo.DefaultBranch
		}
		create.Provider, create.RepoFullName = repo.Provider, repo.FullName
		create.RepoCloneURL, create.Branch = repo.CloneURL, branch
	} else {
		kind := provider.Kind(strings.TrimSpace(req.Provider))
		switch {
		case kind == "" || !create.PropagateToken:
			// Nothing to propagate. The account is left off the session rather
			// than recorded as an attachment it does not have.
			create.Provider, create.PropagateToken = "", false
		case !s.requireConnectedAccount(w, r, kind):
			return
		default:
			create.Provider = kind
		}
	}

	created, err := s.sessions.Create(r.Context(), s.user(r), create)
	switch {
	case errors.Is(err, session.ErrImageNotFound):
		writeError(w, http.StatusBadRequest, "no such image")
		return
	case errors.Is(err, session.ErrImageNotReady):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, session.ErrVSCodeUnavailable):
		writeError(w, http.StatusServiceUnavailable, "the VS Code integration is not available on this server")
		return
	case errors.Is(err, session.ErrComposeUnavailable):
		writeError(w, http.StatusServiceUnavailable, "this server has no docker compose to run a project with")
		return
	case errors.Is(err, session.ErrInvalidPorts):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		s.log.Error("create session", "repo", create.RepoFullName, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot create session")
		return
	}

	s.log.Info("session created", "session", created.ID,
		"repo", created.RepoFullName, "branch", created.Branch, "provider", created.Provider)
	s.writeSession(w, r, http.StatusAccepted, created)
}

// requireConnectedAccount reports whether the caller has that account, and
// answers the request itself when they do not. A session created without a
// repository names its provider directly, so this is the only thing standing
// between a request and a session that would fail to provision with a message
// about unsealing a credential that was never there.
func (s *Server) requireConnectedAccount(w http.ResponseWriter, r *http.Request, kind provider.Kind) bool {
	if _, err := s.providers.Get(kind); err != nil {
		writeError(w, http.StatusBadRequest, "no such provider")
		return false
	}
	_, err := s.store.ProviderAccount(r.Context(), s.user(r).ID, string(kind))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("your %s account is not connected", kind))
		return false
	case err != nil:
		s.log.Error("read provider account", "provider", kind, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read your accounts")
		return false
	}
	return true
}

// resolveRepo finds a repository in the caller's own listing, which is what
// makes the clone URL trustworthy: it comes from the provider, not the browser.
func (s *Server) resolveRepo(w http.ResponseWriter, r *http.Request, kind provider.Kind, fullName string) (provider.Repo, bool) {
	user := s.user(r)

	repo, err := s.repos.Find(r.Context(), user.ID, kind, fullName)
	switch {
	case errors.Is(err, provider.ErrRepoNotFound):
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a repository in your connected accounts", fullName))
		return provider.Repo{}, false
	case errors.Is(err, provider.ErrUnauthorized):
		// 401 is what the SPA turns into a redirect to the login, which is the
		// right move when the rejected account is the GitHub one it signed in
		// with.
		writeError(w, http.StatusUnauthorized, "your access to that account expired, connect it again")
		return provider.Repo{}, false
	case err != nil:
		s.log.Error("resolve repository", "login", user.GitHubLogin, "repo", fullName, "err", err)
		writeError(w, http.StatusBadGateway, "cannot reach the provider")
		return provider.Repo{}, false
	}
	return repo, true
}

type updateSessionRequest struct {
	AutoClaude *bool `json:"autoClaude"`
}

// handleUpdateSession changes a session's settings. Only autoClaude so far, and
// it takes effect the next time the container starts: the tmux session that is
// already running was created with, or without, Claude Code as its command.
//
// propagateToken is deliberately not here. It is part of the container's
// environment, which Docker cannot change once the container exists, so the
// only honest way to flip it would be to build another container — a lifecycle
// operation, not a settings change.
func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	found, ok := s.sessionOr404(w, r)
	if !ok {
		return
	}

	var req updateSessionRequest
	if err := decodeJSON(w, r, maxSessionRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.AutoClaude == nil {
		writeError(w, http.StatusBadRequest, "nothing to update")
		return
	}

	if err := s.store.SetSessionAutoClaude(r.Context(), found.UserID, found.ID, *req.AutoClaude); err != nil {
		s.log.Error("update session", "session", found.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot update the session")
		return
	}
	found.AutoClaude = *req.AutoClaude
	s.writeSession(w, r, http.StatusOK, s.sessions.Refresh(r.Context(), found))
}

// handleStartSession brings a stopped session back up.
func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	found, ok := s.sessionOr404(w, r)
	if !ok {
		return
	}
	if err := s.sessions.Start(r.Context(), found); err != nil {
		s.reportLifecycleError(w, "start", found, err)
		return
	}
	s.writeSession(w, r, http.StatusOK, s.sessions.Refresh(r.Context(), found))
}

// handleStopSession shuts a session's container down, keeping the workspace.
func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	found, ok := s.sessionOr404(w, r)
	if !ok {
		return
	}
	if err := s.sessions.Stop(r.Context(), found); err != nil {
		s.reportLifecycleError(w, "stop", found, err)
		return
	}
	s.writeSession(w, r, http.StatusOK, s.sessions.Refresh(r.Context(), found))
}

// handleDeleteSession removes a session. ?purge=true also deletes the workspace
// on disk, and with it any work that was never pushed.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	found, ok := s.sessionOr404(w, r)
	if !ok {
		return
	}
	purge := r.URL.Query().Get("purge") == "true"
	if err := s.sessions.Delete(r.Context(), found, purge); err != nil {
		s.reportLifecycleError(w, "delete", found, err)
		return
	}
	s.log.Info("session deleted", "session", found.ID, "purged", purge)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reportLifecycleError(w http.ResponseWriter, action string, found *store.Session, err error) {
	if errors.Is(err, session.ErrNoContainer) {
		writeError(w, http.StatusConflict, "this session has no container yet")
		return
	}
	s.log.Error("session lifecycle", "action", action, "session", found.ID, "err", err)
	writeError(w, http.StatusInternalServerError, "cannot "+action+" the session")
}
