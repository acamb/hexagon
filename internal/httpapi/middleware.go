package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/store"
)

// requireAuth rejects requests without a valid session and puts the user into
// the request context for the wrapped handler.
//
// The WebSocket terminal endpoint goes through this too: browsers send cookies
// on the upgrade handshake, so there is no reason for it to authenticate
// differently from the rest of the API.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.authenticate(r)
		switch {
		case errors.Is(err, auth.ErrNoSession):
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		case err != nil:
			s.log.Error("authenticate request", "path", r.URL.Path, "err", err)
			writeError(w, http.StatusInternalServerError, "authentication failed")
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
	})
}

// authenticate resolves the caller, honouring the development bypass.
func (s *Server) authenticate(r *http.Request) (*store.User, error) {
	if s.dev != nil {
		return s.dev.User(r.Context())
	}
	return s.auth.Authenticate(r.Context(), r)
}

// user returns the authenticated caller. Only call it from handlers behind
// requireAuth, where the middleware guarantees it is present.
func (s *Server) user(r *http.Request) *store.User {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		panic("httpapi: handler without requireAuth asked for the user")
	}
	return user
}

// guardStateChanges is defence in depth against cross-site requests. The
// session cookie is SameSite=Lax, which already blocks cross-site form posts;
// on top of that a mutating API call must look like it came from our own
// frontend: a JSON content type (which HTML forms cannot produce) and, when the
// browser sends one, a matching Origin.
func (s *Server) guardStateChanges(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, "expected Content-Type: application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// sameOrigin reports whether the request originates from the configured public
// URL. A missing Origin header is accepted: non-browser clients such as curl do
// not send one, and browsers always do on the requests we care about.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	got, err := url.Parse(origin)
	if err != nil {
		return false
	}
	want, err := url.Parse(s.cfg.PublicURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(got.Scheme, want.Scheme) && strings.EqualFold(got.Host, want.Host)
}
