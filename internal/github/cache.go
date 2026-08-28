package github

import (
	"context"
	"sync"
	"time"
)

// DefaultRepoTTL is how long a repository listing stays fresh. Long enough that
// opening the session dialog repeatedly costs one request, short enough that a
// repository created a minute ago shows up.
const DefaultRepoTTL = 60 * time.Second

// RepoCache remembers each user's repository listing for a short while. GitHub
// rate limits by token, and the listing is several requests for a large
// account, so repeating it on every page load would be wasteful.
type RepoCache struct {
	client *Client
	ttl    time.Duration

	mu      sync.Mutex
	entries map[string]repoCacheEntry
}

type repoCacheEntry struct {
	repos     []Repo
	expiresAt time.Time
}

// NewRepoCache wraps a client. A ttl of zero means DefaultRepoTTL.
func NewRepoCache(client *Client, ttl time.Duration) *RepoCache {
	if ttl <= 0 {
		ttl = DefaultRepoTTL
	}
	return &RepoCache{client: client, ttl: ttl, entries: map[string]repoCacheEntry{}}
}

// List returns the user's repositories, from cache when it is still fresh.
func (rc *RepoCache) List(ctx context.Context, userID, token string) ([]Repo, error) {
	if repos, ok := rc.cached(userID); ok {
		return repos, nil
	}

	repos, err := rc.client.ListRepos(ctx, token)
	if err != nil {
		return nil, err
	}

	rc.mu.Lock()
	rc.entries[userID] = repoCacheEntry{repos: repos, expiresAt: time.Now().Add(rc.ttl)}
	rc.mu.Unlock()
	return repos, nil
}

// Invalidate drops a user's entry, so the next List refetches. The UI uses it
// for an explicit refresh.
func (rc *RepoCache) Invalidate(userID string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.entries, userID)
}

func (rc *RepoCache) cached(userID string) ([]Repo, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	entry, ok := rc.entries[userID]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.repos, true
}
