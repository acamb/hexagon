package provider

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultRepoTTL is how long a repository listing stays fresh. Long enough that
// opening the session dialog repeatedly costs one request per account, short
// enough that a repository created a minute ago shows up.
const DefaultRepoTTL = 60 * time.Second

// CredentialSource hands over the unsealed credentials of every account a user
// has connected. It is an interface so the cipher and the database stay in
// internal/auth, where the rest of the secret handling lives.
type CredentialSource interface {
	Connected(ctx context.Context, userID string) (map[Kind]Credentials, error)
}

// Listing is what one user's connected accounts came back with.
//
// Failures are recorded per provider rather than returned as one error: a
// Bitbucket token that expired must not hide the GitHub repositories, and the
// user needs to be told which half of the list is missing.
type Listing struct {
	Repos  []Repo
	Failed map[Kind]error
}

// Lister merges the repositories of every account a user has connected, and
// remembers each account's listing for a short while.
type Lister struct {
	registry Registry
	creds    CredentialSource
	ttl      time.Duration

	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
}

type cacheKey struct {
	userID string
	kind   Kind
}

type cacheEntry struct {
	repos     []Repo
	expiresAt time.Time
}

// NewLister wraps a registry. A ttl of zero means DefaultRepoTTL.
func NewLister(registry Registry, creds CredentialSource, ttl time.Duration) *Lister {
	if ttl <= 0 {
		ttl = DefaultRepoTTL
	}
	return &Lister{
		registry: registry,
		creds:    creds,
		ttl:      ttl,
		entries:  map[cacheKey]cacheEntry{},
	}
}

// List returns the user's repositories across every connected account, most
// recently updated first. Only a failure to read the accounts themselves is an
// error; a provider that could not be reached lands in Listing.Failed.
func (l *Lister) List(ctx context.Context, userID string) (Listing, error) {
	connected, err := l.creds.Connected(ctx, userID)
	if err != nil {
		return Listing{}, err
	}

	var (
		mu      sync.Mutex
		repos   []Repo
		failed  = map[Kind]error{}
		waiting sync.WaitGroup
	)
	for kind, credentials := range connected {
		p, err := l.registry.Get(kind)
		if err != nil {
			// A row for a provider this build does not know: worth reporting,
			// not worth failing the whole listing over.
			failed[kind] = err
			continue
		}

		waiting.Add(1)
		go func(kind Kind, p Provider, c Credentials) {
			defer waiting.Done()
			found, err := l.listOne(ctx, userID, kind, p, c)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed[kind] = err
				return
			}
			repos = append(repos, found...)
		}(kind, p, credentials)
	}
	waiting.Wait()

	// One list, newest activity first, whichever account each came from.
	sort.SliceStable(repos, func(i, j int) bool { return repos[i].UpdatedAt.After(repos[j].UpdatedAt) })
	return Listing{Repos: repos, Failed: failed}, nil
}

func (l *Lister) listOne(ctx context.Context, userID string, kind Kind, p Provider, c Credentials) ([]Repo, error) {
	if repos, ok := l.cached(cacheKey{userID, kind}); ok {
		return repos, nil
	}

	repos, err := p.ListRepos(ctx, c)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.entries[cacheKey{userID, kind}] = cacheEntry{repos: repos, expiresAt: time.Now().Add(l.ttl)}
	l.mu.Unlock()
	return repos, nil
}

// Invalidate drops everything remembered for a user, so the next List refetches
// from every account. The UI uses it for an explicit refresh.
func (l *Lister) Invalidate(userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key := range l.entries {
		if key.userID == userID {
			delete(l.entries, key)
		}
	}
}

func (l *Lister) cached(key cacheKey) ([]Repo, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.entries[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.repos, true
}

// Find looks a repository up in a user's listing. It is how a session's clone
// URL is decided: the browser names a repository, and only one the user can
// actually reach comes back.
//
// An empty kind matches on name alone, which is what a client that predates
// providers sends.
func (l *Lister) Find(ctx context.Context, userID string, kind Kind, fullName string) (Repo, error) {
	listing, err := l.List(ctx, userID)
	if err != nil {
		return Repo{}, err
	}
	for _, repo := range listing.Repos {
		if (kind == "" || repo.Provider == kind) && strings.EqualFold(repo.FullName, fullName) {
			return repo, nil
		}
	}
	// A rejected token is the likely reason a repository that exists is not in
	// the listing, so say so rather than "no such repository".
	for _, err := range listing.Failed {
		if errors.Is(err, ErrUnauthorized) {
			return Repo{}, err
		}
	}
	return Repo{}, ErrRepoNotFound
}

// ErrRepoNotFound reports a repository that is not in the caller's listing.
var ErrRepoNotFound = errors.New("provider: no such repository")
