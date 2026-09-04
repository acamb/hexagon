package httpapi

import (
	"net/http"
	"time"

	"github.com/andrea/hexagon/internal/provider"
)

type repoResponse struct {
	Provider      provider.Kind `json:"provider"`
	FullName      string        `json:"fullName"`
	CloneURL      string        `json:"cloneUrl"`
	DefaultBranch string        `json:"defaultBranch"`
	Private       bool          `json:"private"`
	Description   string        `json:"description"`
	UpdatedAt     time.Time     `json:"updatedAt"`
}

// listingResponse carries the repositories and, separately, the accounts that
// could not be reached. One expired Bitbucket token must not hide the GitHub
// repositories, and the user still has to be told which half is missing.
type listingResponse struct {
	Repos  []repoResponse    `json:"repos"`
	Failed map[string]string `json:"failed,omitempty"`
}

// handleListRepos returns the repositories the caller can start a session from,
// across every account they have connected. The listing is cached per user and
// provider; ?refresh=1 forces a fresh fetch.
func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	if r.URL.Query().Get("refresh") != "" {
		s.repos.Invalidate(user.ID)
	}

	listing, err := s.repos.List(r.Context(), user.ID)
	if err != nil {
		s.log.Error("list repositories", "login", user.GitHubLogin, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read your connected accounts")
		return
	}

	// Per provider, because the shape of a listing bug is a provider that
	// contributes nothing while reporting no failure — invisible in the
	// response, and until this line invisible in the log as well.
	counts := map[provider.Kind]int{}

	out := listingResponse{Repos: make([]repoResponse, 0, len(listing.Repos))}
	for _, repo := range listing.Repos {
		counts[repo.Provider]++
		out.Repos = append(out.Repos, repoResponse{
			Provider:      repo.Provider,
			FullName:      repo.FullName,
			CloneURL:      repo.CloneURL,
			DefaultBranch: repo.DefaultBranch,
			Private:       repo.Private,
			Description:   repo.Description,
			UpdatedAt:     repo.UpdatedAt,
		})
	}
	for kind, err := range listing.Failed {
		if out.Failed == nil {
			out.Failed = map[string]string{}
		}
		s.log.Warn("provider listing failed", "login", user.GitHubLogin, "provider", kind, "err", err)
		out.Failed[string(kind)] = err.Error()
	}
	s.log.Debug("repository listing", "login", user.GitHubLogin, "counts", counts, "failed", len(listing.Failed))
	writeJSON(w, http.StatusOK, out)
}
