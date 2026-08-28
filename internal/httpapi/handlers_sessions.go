package httpapi

import (
	"net/http"
	"time"

	"github.com/andrea/hexagon/internal/store"
)

type sessionResponse struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	RepoFullName string    `json:"repoFullName"`
	Branch       string    `json:"branch"`
	ImageRef     string    `json:"imageRef"`
	Status       string    `json:"status"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

func newSessionResponse(session *store.Session) sessionResponse {
	return sessionResponse{
		ID:           session.ID,
		Title:        session.Title,
		RepoFullName: session.RepoFullName,
		Branch:       session.Branch,
		ImageRef:     session.ImageRef,
		Status:       session.Status,
		Error:        session.Error,
		CreatedAt:    session.CreatedAt,
	}
}

// handleGetSession describes one session. The terminal view reads it to decide
// whether to open a socket at all.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	session, ok := s.sessionOr404(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newSessionResponse(session))
}
