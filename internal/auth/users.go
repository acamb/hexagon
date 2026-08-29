package auth

import (
	"context"

	"github.com/andrea/hexagon/internal/github"
)

// UserFetcher resolves a GitHub token to the account that owns it. It is the
// step of the login that turns a token into an identity, and it is an interface
// so the handlers can be driven without talking to GitHub.
type UserFetcher interface {
	CurrentUser(ctx context.Context, token string) (*github.User, error)
}
