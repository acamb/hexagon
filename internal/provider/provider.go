// Package provider is where Hexagon stops assuming every repository comes from
// GitHub: the neutral vocabulary for repositories and accounts, the interface a
// source of repositories implements, and the aggregation and caching over the
// accounts one user has connected.
//
// The interface answers two separate questions about the same stored secret,
// because for some providers they have different answers: what the REST API
// wants as its credentials, and what git wants over HTTPS. Bitbucket's API
// takes the Atlassian account email while its git endpoint takes a fixed
// placeholder, so a single "token" passed around would have been wrong.
package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Kind identifies a provider. It is the value stored in the database and sent
// over the API, so it is part of the contract, not a display name.
type Kind string

const (
	GitHub    Kind = "github"
	Bitbucket Kind = "bitbucket"
)

// Valid reports whether k is a provider Hexagon knows.
func (k Kind) Valid() bool {
	switch k {
	case GitHub, Bitbucket:
		return true
	}
	return false
}

// ErrUnauthorized reports credentials the provider no longer accepts: revoked,
// expired, or never valid. Retrying will not help; the account has to be
// connected again.
var ErrUnauthorized = errors.New("provider: credentials rejected")

// Repo is a repository as the session picker needs it, whichever provider it
// came from.
type Repo struct {
	Provider      Kind
	FullName      string
	CloneURL      string
	DefaultBranch string
	Private       bool
	Description   string
	UpdatedAt     time.Time
}

// Credentials are one connected account's secrets, unsealed.
//
// Identity is what the provider's API wants as the user half of its
// credentials, which is not always the account name: Bitbucket wants the
// Atlassian account email. It is empty for providers that do not need one.
type Credentials struct {
	Account  string
	Identity string
	Secret   string
	// GitSecret is what git should authenticate with instead of Secret, empty
	// when there is nothing to prefer. It exists for GitHub, whose Secret is
	// the OAuth token from signing in: that token expires within hours, and a
	// container cannot be handed a new one, so a session outlives its own
	// ability to push. A personal access token pasted here does not expire.
	GitSecret string
}

// SecretForGit is the secret git wants: the one pasted for it, or the account's
// own. Every caller that authenticates git goes through it, so the preference is
// stated once rather than in each provider.
func (c Credentials) SecretForGit() string {
	if c.GitSecret != "" {
		return c.GitSecret
	}
	return c.Secret
}

// GitAuth is what git wants over HTTPS for one account. It is a type of its own
// because the pair is not the API's pair, and two loose strings would sooner or
// later be passed the wrong way round.
type GitAuth struct {
	Username string
	Secret   string
}

// Account is who a set of credentials belongs to, as the provider reports it.
type Account struct {
	Kind      Kind
	Account   string
	Identity  string
	AvatarURL string
}

// Provider is one source of repositories.
type Provider interface {
	Kind() Kind
	// Verify identifies the account behind the credentials. Connecting an
	// account goes through it, so a token is checked against the provider
	// before Hexagon ever stores it.
	Verify(ctx context.Context, c Credentials) (Account, error)
	// ListRepos returns everything those credentials can reach.
	ListRepos(ctx context.Context, c Credentials) ([]Repo, error)
	// GitCredentials are what git wants over HTTPS for this provider, which is
	// not necessarily the pair the API wants.
	GitCredentials(c Credentials) GitAuth
}

// Registry is the set of providers this server knows how to talk to.
type Registry map[Kind]Provider

// NewRegistry indexes providers by kind.
func NewRegistry(providers ...Provider) Registry {
	r := make(Registry, len(providers))
	for _, p := range providers {
		r[p.Kind()] = p
	}
	return r
}

// Get returns the provider for kind, or an error naming what was asked for:
// the kind arrives from a request, so this is a user error rather than a bug.
func (r Registry) Get(kind Kind) (Provider, error) {
	p, ok := r[kind]
	if !ok {
		return nil, fmt.Errorf("provider: no such provider %q", kind)
	}
	return p, nil
}

// Kinds returns the known providers in a stable order.
func (r Registry) Kinds() []Kind {
	kinds := make([]Kind, 0, len(r))
	for kind := range r {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}
