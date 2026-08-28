package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/andrea/hexagon/internal/github"
)

type repoResponse struct {
	FullName      string    `json:"fullName"`
	CloneURL      string    `json:"cloneUrl"`
	DefaultBranch string    `json:"defaultBranch"`
	Private       bool      `json:"private"`
	Description   string    `json:"description"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// handleListRepos returns the repositories the caller can start a session from.
// The listing is cached per user; ?refresh=1 forces a fresh fetch.
func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	token, err := s.auth.GitHubToken(user)
	if err != nil {
		s.log.Error("unseal github token", "login", user.GitHubLogin, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read your GitHub credentials")
		return
	}

	if r.URL.Query().Get("refresh") != "" {
		s.repos.Invalidate(user.ID)
	}

	repos, err := s.repos.List(r.Context(), user.ID, token)
	switch {
	case errors.Is(err, github.ErrUnauthorized):
		// Nothing to retry: the token is gone, the browser has to sign in
		// again. 401 is what the SPA already turns into a redirect.
		s.log.Warn("github rejected the stored token", "login", user.GitHubLogin)
		writeError(w, http.StatusUnauthorized, "GitHub access expired, sign in again")
		return
	case err != nil:
		s.log.Error("list repositories", "login", user.GitHubLogin, "err", err)
		writeError(w, http.StatusBadGateway, "cannot reach GitHub")
		return
	}

	out := make([]repoResponse, 0, len(repos))
	for _, repo := range repos {
		out = append(out, repoResponse{
			FullName:      repo.FullName,
			CloneURL:      repo.CloneURL,
			DefaultBranch: repo.DefaultBranch,
			Private:       repo.Private,
			Description:   repo.Description,
			UpdatedAt:     repo.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
