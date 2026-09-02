// Package auth handles who may use Hexagon: the GitHub OAuth login, the browser
// session cookies that follow it, and the encryption of the provider
// credentials the rest of the application uses on the user's behalf.
//
// Signing in is GitHub only, and stays that way: the allowlist is a list of
// GitHub logins. Other providers are accounts a signed-in user connects, which
// is a different question from who they are.
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

	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/provider"
	"github.com/andrea/hexagon/internal/store"
)

const (
	// sessionCookie holds the browser login session. It is served under the
	// name CookieName returns, which over https is this one prefixed.
	sessionCookie = "hexagon_session"
	// stateCookie holds the OAuth state parameter between the redirect to
	// GitHub and the callback. It keeps its plain name in both modes: __Host-
	// forbids a Path, and this cookie's is /api/auth.
	stateCookie = "hexagon_oauth_state"
	// hostPrefix is enforced by the browser, which refuses a cookie carrying it
	// unless it is Secure, Path=/ and has no Domain. That is what makes the
	// session cookie impossible to overwrite from a sibling subdomain over
	// plaintext, which is a thing no server-side check can prevent.
	hostPrefix = "__Host-"

	// sessionTTL is short because a session cookie is the whole credential:
	// whoever holds one drives the Docker socket. It stays a constant rather
	// than a setting — a session lifetime is a property of the security model,
	// not of the machine — and Renew keeps it from being a weekly annoyance.
	sessionTTL    = 7 * 24 * time.Hour
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

// NewService builds the session service. The cookie is Secure whenever the
// public URL is https, which config.Load's transport check makes a statement
// about the deployment rather than a guess: an instance reachable from the
// network cannot be configured with an http public URL.
func NewService(st *store.Store, cipher *Cipher, publicURL string) *Service {
	return &Service{
		store:  st,
		cipher: cipher,
		secure: strings.HasPrefix(publicURL, "https://"),
	}
}

// CookieName is the name the session cookie is served under. The __Host-
// prefix requires Secure, so the name depends on the deployment and cannot be a
// constant; Issue, Authenticate and Logout all ask here so the three cannot
// disagree about what to set, read and clear.
func (s *Service) CookieName() string {
	if s.secure {
		return hostPrefix + sessionCookie
	}
	return sessionCookie
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

	s.setSessionCookie(w, token, expires)
	return nil
}

// Renew extends a session that is more than halfway through its life and
// re-sets the cookie, so a browser in daily use is never signed out by the
// clock. A session younger than that is left alone: at most one write per
// browser per half life, whatever the request rate.
//
// It is separate from Authenticate because failing to extend a session is not a
// reason to refuse a request that carries a valid one — the caller decides what
// to do with the error, and has a logger to say it with.
func (s *Service) Renew(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	cookie, err := r.Cookie(s.CookieName())
	if err != nil || cookie.Value == "" {
		return nil
	}
	expires := time.Now().Add(sessionTTL)
	renewed, err := s.store.TouchUserSession(ctx, hashToken(cookie.Value), time.Now().Add(sessionTTL/2), expires)
	if err != nil || !renewed {
		return err
	}
	s.setSessionCookie(w, cookie.Value, expires)
	return nil
}

// setSessionCookie is the one place the session cookie's attributes are
// written, so Issue and Renew cannot disagree about them.
func (s *Service) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.CookieName(),
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Authenticate resolves the session cookie on r to a user.
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (*store.User, error) {
	cookie, err := r.Cookie(s.CookieName())
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
	if cookie, err := r.Cookie(s.CookieName()); err == nil && cookie.Value != "" {
		if err := s.store.DeleteUserSession(ctx, hashToken(cookie.Value)); err != nil {
			return err
		}
	}
	s.clearCookie(w, s.CookieName(), "/")
	return nil
}

// SetState stores the OAuth state parameter in a short lived cookie.
func (s *Service) SetState(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
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
	s.clearCookie(w, stateCookie, cookiePathAPI)
	cookie, err := r.Cookie(stateCookie)
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

// SaveLogin records the login and stores the token that came with it.
//
// Two writes, because they are two different things: the user row is the
// identity, and the GitHub provider account is one of the places this user's
// repositories come from. The login just happens to hand us both at once.
func (s *Service) SaveLogin(ctx context.Context, ghUser *github.User, token string) (*store.User, error) {
	user, err := s.store.UpsertUser(ctx, &store.User{
		GitHubLogin: ghUser.Login,
		GitHubID:    ghUser.ID,
		AvatarURL:   ghUser.AvatarURL,
	})
	if err != nil {
		return nil, err
	}
	_, err = s.Connect(ctx, user, provider.Account{
		Kind:      provider.GitHub,
		Account:   ghUser.Login,
		AvatarURL: ghUser.AvatarURL,
	}, token)
	if err != nil {
		return nil, err
	}
	return user, nil
}

// Connect stores an account's secret, sealed. The account has already been
// verified against the provider by the time it gets here.
func (s *Service) Connect(ctx context.Context, user *store.User, account provider.Account, secret string) (*store.ProviderAccount, error) {
	sealed, err := s.cipher.Seal([]byte(secret))
	if err != nil {
		return nil, err
	}
	return s.store.UpsertProviderAccount(ctx, &store.ProviderAccount{
		UserID:    user.ID,
		Provider:  string(account.Kind),
		Account:   account.Account,
		Identity:  account.Identity,
		AvatarURL: account.AvatarURL,
		SecretEnc: sealed,
	})
}

// Credentials unseals one connected account, for acting on the user's behalf.
func (s *Service) Credentials(ctx context.Context, userID string, kind provider.Kind) (provider.Credentials, error) {
	account, err := s.store.ProviderAccount(ctx, userID, string(kind))
	if err != nil {
		return provider.Credentials{}, err
	}
	return s.credentials(account)
}

// Connected unseals every account the user has connected. It is what
// provider.Lister needs, and the reason the cipher never has to leave this
// package.
func (s *Service) Connected(ctx context.Context, userID string) (map[provider.Kind]provider.Credentials, error) {
	accounts, err := s.store.ListProviderAccounts(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[provider.Kind]provider.Credentials, len(accounts))
	for _, account := range accounts {
		credentials, err := s.credentials(account)
		if err != nil {
			return nil, err
		}
		out[provider.Kind(account.Provider)] = credentials
	}
	return out, nil
}

// CreateClaudeAccount adds a new Claude account for this user, sealing its
// secret when it has one. A login account is created with no secret at all: it
// is a row with a name and a directory, and the directory is what a browser
// login on it fills in.
func (s *Service) CreateClaudeAccount(ctx context.Context, userID, name, kind, secret string, isDefault bool) (*store.ClaudeAccount, error) {
	var sealed []byte
	if secret != "" {
		var err error
		if sealed, err = s.cipher.Seal([]byte(secret)); err != nil {
			return nil, err
		}
	}
	return s.store.CreateClaudeAccount(ctx, &store.ClaudeAccount{
		UserID: userID, Name: name, Kind: kind, SecretEnc: sealed, IsDefault: isDefault,
	})
}

// SetClaudeAccountSecret replaces a pasted account's secret, sealed. It has
// been checked against the CLI by the time it gets here.
func (s *Service) SetClaudeAccountSecret(ctx context.Context, userID, id, secret string) error {
	sealed, err := s.cipher.Seal([]byte(secret))
	if err != nil {
		return err
	}
	return s.store.SetClaudeAccountSecret(ctx, userID, id, sealed)
}

// ClaudeSecret opens the sealed secret of a pasted account. It is called only
// for an api_key or oauth_token account: a login account has none, and the
// session manager never asks for one — its credential is the directory the
// manager itself owns.
func (s *Service) ClaudeSecret(ctx context.Context, account *store.ClaudeAccount) (claudex.Credential, error) {
	secret, err := s.cipher.Open(account.SecretEnc)
	if err != nil {
		return claudex.Credential{}, err
	}
	return claudex.Credential{Kind: account.Kind, Secret: string(secret)}, nil
}

// GitCredentialSource pairs a user's sealed credentials with the provider that
// knows what git wants for them. The session orchestrator holds one of these
// rather than a cipher and a registry.
type GitCredentialSource struct {
	service  *Service
	registry provider.Registry
}

// NewGitCredentialSource wires the two together.
func NewGitCredentialSource(service *Service, registry provider.Registry) *GitCredentialSource {
	return &GitCredentialSource{service: service, registry: registry}
}

// GitCredentials returns what git needs to reach one of the user's accounts.
func (g *GitCredentialSource) GitCredentials(ctx context.Context, userID string, kind provider.Kind) (provider.GitAuth, error) {
	p, err := g.registry.Get(kind)
	if err != nil {
		return provider.GitAuth{}, err
	}
	credentials, err := g.service.Credentials(ctx, userID, kind)
	if err != nil {
		return provider.GitAuth{}, err
	}
	return p.GitCredentials(credentials), nil
}

// ClaudeSecret delegates to the service, so the session orchestrator holds one
// collaborator for every credential it needs rather than one per kind.
func (g *GitCredentialSource) ClaudeSecret(ctx context.Context, account *store.ClaudeAccount) (claudex.Credential, error) {
	return g.service.ClaudeSecret(ctx, account)
}

func (s *Service) credentials(account *store.ProviderAccount) (provider.Credentials, error) {
	secret, err := s.cipher.Open(account.SecretEnc)
	if err != nil {
		return provider.Credentials{}, err
	}
	return provider.Credentials{
		Account:  account.Account,
		Identity: account.Identity,
		Secret:   string(secret),
	}, nil
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
