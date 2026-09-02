package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/provider"
	"github.com/andrea/hexagon/internal/store"
)

// maxAccountRequestBody bounds a connect request: an email and a token.
const maxAccountRequestBody = 8 << 10

type accountResponse struct {
	Provider provider.Kind `json:"provider"`
	Account  string        `json:"account"`
	// Identity is what the provider's API is addressed with — the Atlassian
	// account email for Bitbucket — shown so the user can see which one they
	// connected.
	Identity  string    `json:"identity,omitempty"`
	AvatarURL string    `json:"avatarUrl,omitempty"`
	Connected bool      `json:"connected"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	// Removable is false for GitHub: it is the account this user signs in with,
	// so forgetting it would mean forgetting how they got here.
	Removable bool `json:"removable"`
	// GitTokenSet reports whether a long-lived token has been pasted for git.
	// The value itself never comes back out, the same way the settings page
	// reports the client secret.
	GitTokenSet bool `json:"gitTokenSet"`
}

// handleListAccounts describes every provider this build knows, connected or
// not, so the page can offer the ones that are missing.
func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	connected, err := s.store.ListProviderAccounts(r.Context(), user.ID)
	if err != nil {
		s.log.Error("list provider accounts", "login", user.GitHubLogin, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot list your accounts")
		return
	}
	byKind := make(map[provider.Kind]*store.ProviderAccount, len(connected))
	for _, account := range connected {
		byKind[provider.Kind(account.Provider)] = account
	}

	out := make([]accountResponse, 0, len(s.providers))
	for _, kind := range s.providers.Kinds() {
		response := accountResponse{Provider: kind, Removable: kind != provider.GitHub}
		if account, ok := byKind[kind]; ok {
			response.Account = account.Account
			response.Identity = account.Identity
			response.AvatarURL = account.AvatarURL
			response.Connected = true
			response.UpdatedAt = account.UpdatedAt
			response.GitTokenSet = len(account.GitSecretEnc) > 0
		}
		out = append(out, response)
	}
	writeJSON(w, http.StatusOK, out)
}

type connectAccountRequest struct {
	Identity string `json:"identity"`
	Secret   string `json:"secret"`
}

// handleConnectAccount stores a set of credentials after checking them against
// the provider. Verifying first is the point: a mistyped token that was stored
// anyway would show up later as an account that lists nothing, with no clue as
// to why.
func (s *Server) handleConnectAccount(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	kind := provider.Kind(r.PathValue("provider"))
	p, err := s.providers.Get(kind)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such provider")
		return
	}
	if kind == provider.GitHub {
		// It arrives with the login and is refreshed by signing in again.
		writeError(w, http.StatusConflict, "the GitHub account comes from signing in")
		return
	}

	var req connectAccountRequest
	if err := decodeJSON(w, r, maxAccountRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	credentials := provider.Credentials{
		Identity: strings.TrimSpace(req.Identity),
		Secret:   strings.TrimSpace(req.Secret),
	}
	if credentials.Secret == "" {
		writeError(w, http.StatusBadRequest, "an API token is required")
		return
	}
	if kind == provider.Bitbucket && credentials.Identity == "" {
		// Bitbucket's API authenticates as email plus token, so half of it is
		// no good at all.
		writeError(w, http.StatusBadRequest, "the Atlassian account email is required")
		return
	}

	account, err := p.Verify(r.Context(), credentials)
	switch {
	case errors.Is(err, provider.ErrUnauthorized):
		// The provider's own words, not ours: what is wrong with a token is
		// something only it knows, and a message we invented would send the
		// user off to check the wrong thing.
		s.log.Warn("provider refused the credentials", "provider", kind, "err", err)
		writeError(w, http.StatusBadRequest, "those credentials were rejected — "+err.Error())
		return
	case err != nil:
		s.log.Error("verify provider account", "provider", kind, "err", err)
		writeError(w, http.StatusBadGateway, "cannot reach "+string(kind)+": "+err.Error())
		return
	}

	if _, err := s.auth.Connect(r.Context(), user, account, credentials.Secret); err != nil {
		s.log.Error("connect provider account", "provider", kind, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot save the account")
		return
	}
	// Whatever was listed before was listed with the old credentials.
	s.repos.Invalidate(user.ID)

	s.log.Info("account connected", "login", user.GitHubLogin, "provider", kind, "account", account.Account)
	writeJSON(w, http.StatusOK, accountResponse{
		Provider:  kind,
		Account:   account.Account,
		Identity:  account.Identity,
		AvatarURL: account.AvatarURL,
		Connected: true,
		UpdatedAt: time.Now(),
		Removable: true,
	})
}

// handleDisconnectAccount forgets a connected account. Sessions already created
// from it keep their clone; what they lose is the credential, which is what
// disconnecting means.
func (s *Server) handleDisconnectAccount(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	kind := provider.Kind(r.PathValue("provider"))
	if _, err := s.providers.Get(kind); err != nil {
		writeError(w, http.StatusNotFound, "no such provider")
		return
	}
	if kind == provider.GitHub {
		writeError(w, http.StatusConflict, "the GitHub account is how you sign in")
		return
	}

	err := s.store.DeleteProviderAccount(r.Context(), user.ID, string(kind))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "that account is not connected")
		return
	case err != nil:
		s.log.Error("disconnect provider account", "provider", kind, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot disconnect the account")
		return
	}
	s.repos.Invalidate(user.ID)

	s.log.Info("account disconnected", "login", user.GitHubLogin, "provider", kind)
	w.WriteHeader(http.StatusNoContent)
}

type gitTokenRequest struct {
	Secret string `json:"secret"`
}

// handleSetAccountGitToken stores the long-lived token an account's git
// operations should use, in place of the credential it was connected with.
//
// It is its own route rather than a field on handleConnectAccount because that
// handler refuses GitHub outright, and rightly: this is not a way to connect a
// GitHub account, it is a second secret attached to one that already exists.
func (s *Server) handleSetAccountGitToken(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	kind := provider.Kind(r.PathValue("provider"))
	p, err := s.providers.Get(kind)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such provider")
		return
	}
	stored, err := s.store.ProviderAccount(r.Context(), user.ID, string(kind))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "that account is not connected")
		return
	case err != nil:
		s.log.Error("read provider account", "provider", kind, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read the account")
		return
	}

	var req gitTokenRequest
	if err := decodeJSON(w, r, maxAccountRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	secret := strings.TrimSpace(req.Secret)
	if secret == "" {
		writeError(w, http.StatusBadRequest, "a token is required")
		return
	}

	// Checked before it is stored, for the reason connecting is — and with more
	// at stake: a token that does not work fails inside a container, in a push
	// nobody is watching, hours after it was pasted. The identity is the stored
	// one because that is the half of the credentials the provider's API wants
	// and this request does not carry.
	account, err := p.Verify(r.Context(), provider.Credentials{Identity: stored.Identity, Secret: secret})
	switch {
	case errors.Is(err, provider.ErrUnauthorized):
		s.log.Warn("provider refused the git token", "provider", kind, "err", err)
		writeError(w, http.StatusBadRequest, "that token was rejected — "+err.Error())
		return
	case err != nil:
		s.log.Error("verify git token", "provider", kind, "err", err)
		writeError(w, http.StatusBadGateway, "cannot reach "+string(kind)+": "+err.Error())
		return
	}
	// A token identifies an account of its own, and it does not have to be this
	// one. Storing somebody else's would produce a session pushing commits under
	// their name, which is unpleasant to unpick and impossible to guess at from
	// the symptom.
	if account.Account != stored.Account {
		writeError(w, http.StatusBadRequest,
			"that token belongs to "+account.Account+", not to "+stored.Account)
		return
	}

	if err := s.auth.SetGitSecret(r.Context(), user.ID, kind, secret); err != nil {
		s.log.Error("store git token", "provider", kind, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot save the token")
		return
	}

	// No repos.Invalidate: the listing runs on the account's own credential, so
	// nothing it holds went stale.
	s.log.Info("git token stored", "login", user.GitHubLogin, "provider", kind, "account", stored.Account)
	writeJSON(w, http.StatusOK, accountResponse{
		Provider:    kind,
		Account:     stored.Account,
		Identity:    stored.Identity,
		AvatarURL:   stored.AvatarURL,
		Connected:   true,
		UpdatedAt:   time.Now(),
		Removable:   kind != provider.GitHub,
		GitTokenSet: true,
	})
}

// handleClearAccountGitToken forgets it, leaving git back on the credential the
// account was connected with — which for GitHub is the one that expires.
func (s *Server) handleClearAccountGitToken(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)

	kind := provider.Kind(r.PathValue("provider"))
	if _, err := s.providers.Get(kind); err != nil {
		writeError(w, http.StatusNotFound, "no such provider")
		return
	}

	err := s.auth.ClearGitSecret(r.Context(), user.ID, kind)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "that account is not connected")
		return
	case err != nil:
		s.log.Error("clear git token", "provider", kind, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot remove the token")
		return
	}

	s.log.Info("git token removed", "login", user.GitHubLogin, "provider", kind)
	w.WriteHeader(http.StatusNoContent)
}
