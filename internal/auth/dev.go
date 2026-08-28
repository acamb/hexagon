package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/store"
)

// UserFetcher resolves a GitHub token to the account that owns it.
type UserFetcher interface {
	CurrentUser(ctx context.Context, token string) (*github.User, error)
}

// DevProvider is the HEXAGON_DEV_USER bypass: it skips OAuth and treats every
// request as coming from one preconfigured account. It exists so the UI can be
// worked on without registering an OAuth App, and it is deliberately hard to
// leave switched on by accident.
type DevProvider struct {
	login string
	token string
	gh    UserFetcher
	svc   *Service

	mu     sync.Mutex
	cached *store.User
}

// NewDevProvider validates the bypass configuration. It refuses to run on
// anything but a loopback listener: this mode authenticates nobody, so exposing
// it on a network would hand the Docker socket to whoever finds the port.
func NewDevProvider(login, token, addr string, gh UserFetcher, svc *Service) (*DevProvider, error) {
	if token == "" {
		return nil, errors.New("HEXAGON_DEV_USER requires HEXAGON_GITHUB_TOKEN")
	}
	if !isLoopbackAddr(addr) {
		return nil, fmt.Errorf("HEXAGON_DEV_USER refuses to run on %q: bind to a loopback address", addr)
	}
	return &DevProvider{login: login, token: token, gh: gh, svc: svc}, nil
}

// User returns the development user, creating the database row on first use.
// The identity comes from GitHub so the stored token is verified and the user
// id matches what a real login would produce.
func (d *DevProvider) User(ctx context.Context) (*store.User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cached != nil {
		return d.cached, nil
	}

	ghUser, err := d.gh.CurrentUser(ctx, d.token)
	if err != nil {
		return nil, fmt.Errorf("dev user: %w", err)
	}
	if !strings.EqualFold(ghUser.Login, d.login) {
		return nil, fmt.Errorf("dev user: HEXAGON_GITHUB_TOKEN belongs to %q, not %q", ghUser.Login, d.login)
	}

	user, err := d.svc.SaveLogin(ctx, ghUser, d.token)
	if err != nil {
		return nil, err
	}
	d.cached = user
	return user, nil
}

// isLoopbackAddr reports whether a listen address only accepts local traffic.
// An address with no host ("" or ":8080") listens on every interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
