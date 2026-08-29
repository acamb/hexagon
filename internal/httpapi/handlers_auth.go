package httpapi

import (
	"crypto/subtle"
	"net/http"

	"github.com/andrea/hexagon/internal/auth"
)

// loginPath is where the SPA shows the sign-in button. Failed logins land back
// there with an error code rather than on a bare API error page.
const loginPath = "/login"

// handleAuthLogin starts the GitHub OAuth dance.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	state, err := auth.NewState()
	if err != nil {
		s.log.Error("generate oauth state", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot start login")
		return
	}
	s.auth.SetState(w, state)
	http.Redirect(w, r, s.oauth.AuthorizeURL(state), http.StatusFound)
}

// handleAuthCallback completes the login: verify state, exchange the code,
// identify the account, check the allowlist, issue a session.
func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	want := s.auth.State(w, r)
	got := r.URL.Query().Get("state")
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		s.log.Warn("oauth callback with mismatched state", "remote", r.RemoteAddr)
		s.failLogin(w, r, "invalid_state")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		s.failLogin(w, r, "missing_code")
		return
	}

	ctx := r.Context()
	token, err := s.oauth.Exchange(ctx, code)
	if err != nil {
		s.log.Error("oauth code exchange", "err", err)
		s.failLogin(w, r, "exchange_failed")
		return
	}

	ghUser, err := s.github.CurrentUser(ctx, token)
	if err != nil {
		s.log.Error("identify github user", "err", err)
		s.failLogin(w, r, "github_failed")
		return
	}

	allowed, err := s.allowlist.Allowed(ctx, ghUser.Login, ghUser.ID)
	if err != nil {
		s.log.Error("check the allowlist", "login", ghUser.Login, "err", err)
		s.failLogin(w, r, "server_error")
		return
	}
	if !allowed {
		s.log.Warn("login refused, user not in allowlist", "login", ghUser.Login, "github_id", ghUser.ID)
		s.failLogin(w, r, "not_allowed")
		return
	}

	user, err := s.auth.SaveLogin(ctx, ghUser, token)
	if err != nil {
		s.log.Error("save login", "login", ghUser.Login, "err", err)
		s.failLogin(w, r, "server_error")
		return
	}
	if err := s.auth.Issue(ctx, w, user); err != nil {
		s.log.Error("issue session", "login", ghUser.Login, "err", err)
		s.failLogin(w, r, "server_error")
		return
	}

	s.log.Info("login", "login", user.GitHubLogin)
	http.Redirect(w, r, "/", http.StatusFound)
}

// failLogin sends the browser back to the sign-in page with a code the SPA
// turns into a message.
func (s *Server) failLogin(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, loginPath+"?error="+reason, http.StatusFound)
}

// handleAuthMe describes the caller. The SPA uses it as its session check.
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	user := s.user(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        user.ID,
		"login":     user.GitHubLogin,
		"avatarUrl": user.AvatarURL,
	})
}

// handleAuthLogout ends the browser session.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context(), w, r); err != nil {
		s.log.Error("logout", "err", err)
		writeError(w, http.StatusInternalServerError, "logout failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
