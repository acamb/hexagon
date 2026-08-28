package httpapi

import (
	"encoding/json"
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
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Provider     string    `json:"provider"`
	RepoFullName string    `json:"repoFullName"`
	Branch       string    `json:"branch"`
	ImageID      string    `json:"imageId"`
	ImageRef     string    `json:"imageRef"`
	RepoDir      string    `json:"repoDir"`
	Status       string    `json:"status"`
	Error        string    `json:"error,omitempty"`
	AutoClaude   bool      `json:"autoClaude"`
	CreatedAt    time.Time `json:"createdAt"`
}

func newSessionResponse(s *store.Session) sessionResponse {
	return sessionResponse{
		ID:           s.ID,
		Title:        s.Title,
		Provider:     s.Provider,
		RepoFullName: s.RepoFullName,
		Branch:       s.Branch,
		ImageID:      s.ImageID,
		ImageRef:     s.ImageRef,
		RepoDir:      s.RepoDir,
		Status:       s.Status,
		Error:        s.Error,
		AutoClaude:   s.AutoClaude,
		CreatedAt:    s.CreatedAt,
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
		out = append(out, newSessionResponse(session))
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
	writeJSON(w, http.StatusOK, newSessionResponse(s.sessions.Refresh(r.Context(), found)))
}

type createSessionRequest struct {
	// Provider is which connected account the repository comes from. An empty
	// one matches on name alone, across every account.
	Provider     string `json:"provider"`
	RepoFullName string `json:"repoFullName"`
	Branch       string `json:"branch"`
	ImageID      string `json:"imageId"`
	Title        string `json:"title"`
	// AutoClaude is a pointer so that a client which has never heard of it gets
	// the default — Claude Code started for them — rather than a bare shell.
	AutoClaude *bool `json:"autoClaude"`
}

// handleCreateSession starts provisioning a session and returns straight away;
// the work happens in the background and shows up as the session's status.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.RepoFullName) == "" || strings.TrimSpace(req.ImageID) == "" {
		writeError(w, http.StatusBadRequest, "a repository and an image are required")
		return
	}

	if err := s.docker.Ping(r.Context()); err != nil {
		s.log.Error("docker unreachable", "err", err)
		writeError(w, http.StatusServiceUnavailable, "docker is unreachable")
		return
	}

	// The clone URL is never taken from the request: it is looked up in the
	// caller's own accounts, so a session can only ever clone something they
	// actually have.
	repo, ok := s.resolveRepo(w, r, provider.Kind(req.Provider), req.RepoFullName)
	if !ok {
		return
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = repo.DefaultBranch
	}

	created, err := s.sessions.Create(r.Context(), s.user(r), session.CreateRequest{
		Title:        req.Title,
		Provider:     repo.Provider,
		RepoFullName: repo.FullName,
		RepoCloneURL: repo.CloneURL,
		Branch:       branch,
		ImageID:      req.ImageID,
		AutoClaude:   req.AutoClaude == nil || *req.AutoClaude,
	})
	switch {
	case errors.Is(err, session.ErrImageNotFound):
		writeError(w, http.StatusBadRequest, "no such image")
		return
	case errors.Is(err, session.ErrImageNotReady):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.log.Error("create session", "repo", repo.FullName, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot create session")
		return
	}

	s.log.Info("session created", "session", created.ID, "repo", created.RepoFullName, "branch", branch)
	writeJSON(w, http.StatusAccepted, newSessionResponse(created))
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
func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	found, ok := s.sessionOr404(w, r)
	if !ok {
		return
	}

	var req updateSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionRequestBody)).Decode(&req); err != nil {
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
	writeJSON(w, http.StatusOK, newSessionResponse(s.sessions.Refresh(r.Context(), found)))
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
	writeJSON(w, http.StatusOK, newSessionResponse(s.sessions.Refresh(r.Context(), found)))
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
	writeJSON(w, http.StatusOK, newSessionResponse(s.sessions.Refresh(r.Context(), found)))
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
