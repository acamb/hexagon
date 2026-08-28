package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
)

// maxSessionRequestBody bounds a create request. It carries a few short
// strings, nothing more.
const maxSessionRequestBody = 8 << 10

type sessionResponse struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	RepoFullName string    `json:"repoFullName"`
	Branch       string    `json:"branch"`
	ImageID      string    `json:"imageId"`
	ImageRef     string    `json:"imageRef"`
	RepoDir      string    `json:"repoDir"`
	Status       string    `json:"status"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

func newSessionResponse(s *store.Session) sessionResponse {
	return sessionResponse{
		ID:           s.ID,
		Title:        s.Title,
		RepoFullName: s.RepoFullName,
		Branch:       s.Branch,
		ImageID:      s.ImageID,
		ImageRef:     s.ImageRef,
		RepoDir:      s.RepoDir,
		Status:       s.Status,
		Error:        s.Error,
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
	RepoFullName string `json:"repoFullName"`
	Branch       string `json:"branch"`
	ImageID      string `json:"imageId"`
	Title        string `json:"title"`
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
	// caller's own GitHub account, so a session can only ever clone something
	// they actually have.
	repo, ok := s.resolveRepo(w, r, req.RepoFullName)
	if !ok {
		return
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = repo.DefaultBranch
	}

	created, err := s.sessions.Create(r.Context(), s.user(r), session.CreateRequest{
		Title:        req.Title,
		RepoFullName: repo.FullName,
		RepoCloneURL: repo.CloneURL,
		Branch:       branch,
		ImageID:      req.ImageID,
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

// resolveRepo finds a repository in the caller's GitHub listing.
func (s *Server) resolveRepo(w http.ResponseWriter, r *http.Request, fullName string) (github.Repo, bool) {
	user := s.user(r)

	token, err := s.auth.GitHubToken(user)
	if err != nil {
		s.log.Error("unseal github token", "login", user.GitHubLogin, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read your GitHub credentials")
		return github.Repo{}, false
	}

	repos, err := s.repos.List(r.Context(), user.ID, token)
	switch {
	case errors.Is(err, github.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "GitHub access expired, sign in again")
		return github.Repo{}, false
	case err != nil:
		s.log.Error("list repositories", "login", user.GitHubLogin, "err", err)
		writeError(w, http.StatusBadGateway, "cannot reach GitHub")
		return github.Repo{}, false
	}

	for _, repo := range repos {
		if strings.EqualFold(repo.FullName, fullName) {
			return repo, true
		}
	}
	writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a repository in your GitHub account", fullName))
	return github.Repo{}, false
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
