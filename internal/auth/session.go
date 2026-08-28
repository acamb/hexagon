// Package auth handles who may use Hexagon: the GitHub OAuth login, the browser
// session cookies that follow it, and the encryption of the GitHub tokens the
// rest of the application uses on the user's behalf.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/store"
)

const (
	// SessionCookie holds the browser login session.
	SessionCookie = "hexagon_session"
	// StateCookie holds the OAuth state parameter between the redirect to
	// GitHub and the callback.
	StateCookie = "hexagon_oauth_state"

	sessionTTL    = 30 * 24 * time.Hour
	stateTTL      = 10 * time.Minute
	tokenBytes    = 32
	cookiePathAPI = "/api/auth"
)

// ErrNoSession means the request carried no usable session cookie.
var ErrNoSession = errors.New("no session")

// Service issues and validates browser sessions and owns the token cipher.
type Service struct {
	store  *store.Store
	cipher *Cipher
	secure bool
}

// NewService builds the session service. secureCookies should be true whenever
// the public URL is https, so the cookie is not sent over plaintext.
func NewService(st *store.Store, cipher *Cipher, publicURL string) *Service {
	return &Service{
		store:  st,
		cipher: cipher,
		secure: strings.HasPrefix(publicURL, "https://"),
	}
}

// Issue creates a session for user and sets the cookie on w.
func (s *Service) Issue(ctx context.Context, w http.ResponseWriter, user *store.User) error {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	expires := time.Now().Add(sessionTTL)
	if err := s.store.CreateUserSession(ctx, hashToken(token), user.ID, expires); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Authenticate resolves the session cookie on r to a user.
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (*store.User, error) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || cookie.Value == "" {
		return nil, ErrNoSession
	}
	user, err := s.store.UserBySessionToken(ctx, hashToken(cookie.Value))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNoSession
	}
	return user, err
}

// Logout deletes the session behind r and clears the cookie.
func (s *Service) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if cookie, err := r.Cookie(SessionCookie); err == nil && cookie.Value != "" {
		if err := s.store.DeleteUserSession(ctx, hashToken(cookie.Value)); err != nil {
			return err
		}
	}
	s.clearCookie(w, SessionCookie, "/")
	return nil
}

// SetState stores the OAuth state parameter in a short lived cookie.
func (s *Service) SetState(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{
		Name:     StateCookie,
		Value:    state,
		Path:     cookiePathAPI,
		MaxAge:   int(stateTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// State returns the stored OAuth state and clears the cookie: one redirect,
// one usable state value.
func (s *Service) State(w http.ResponseWriter, r *http.Request) string {
	s.clearCookie(w, StateCookie, cookiePathAPI)
	cookie, err := r.Cookie(StateCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (s *Service) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// SaveLogin seals the GitHub token and records the login.
func (s *Service) SaveLogin(ctx context.Context, ghUser *github.User, token string) (*store.User, error) {
	sealed, err := s.cipher.Seal([]byte(token))
	if err != nil {
		return nil, err
	}
	return s.store.UpsertUser(ctx, &store.User{
		GitHubLogin:    ghUser.Login,
		GitHubID:       ghUser.ID,
		AvatarURL:      ghUser.AvatarURL,
		GitHubTokenEnc: sealed,
	})
}

// GitHubToken unseals the token Hexagon uses to act on the user's behalf.
func (s *Service) GitHubToken(user *store.User) (string, error) {
	token, err := s.cipher.Open(user.GitHubTokenEnc)
	if err != nil {
		return "", err
	}
	return string(token), nil
}

// NewState returns a fresh OAuth state value.
func NewState() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

type contextKey struct{}

// WithUser returns a context carrying the authenticated user.
func WithUser(ctx context.Context, user *store.User) context.Context {
	return context.WithValue(ctx, contextKey{}, user)
}

// UserFrom returns the authenticated user attached by the auth middleware.
func UserFrom(ctx context.Context) (*store.User, bool) {
	user, ok := ctx.Value(contextKey{}).(*store.User)
	return user, ok
}
