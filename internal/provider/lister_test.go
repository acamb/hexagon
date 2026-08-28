package provider

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// stubProvider counts the listings it is asked for, which is how the cache is
// observed: a cached listing is one that never reached the provider.
type stubProvider struct {
	kind     Kind
	repos    []Repo
	err      error
	listings atomic.Int32
}

func (s *stubProvider) Kind() Kind { return s.kind }

func (s *stubProvider) Verify(context.Context, Credentials) (Account, error) {
	return Account{Kind: s.kind}, s.err
}

func (s *stubProvider) ListRepos(context.Context, Credentials) ([]Repo, error) {
	s.listings.Add(1)
	return s.repos, s.err
}

func (s *stubProvider) GitCredentials(c Credentials) GitAuth {
	return GitAuth{Username: string(s.kind), Secret: c.Secret}
}

// stubCredentials is who has connected what.
type stubCredentials map[string]map[Kind]Credentials

func (s stubCredentials) Connected(_ context.Context, userID string) (map[Kind]Credentials, error) {
	return s[userID], nil
}

func repo(kind Kind, name string, updated time.Time) Repo {
	return Repo{Provider: kind, FullName: name, UpdatedAt: updated}
}

func newTestLister(t *testing.T, ttl time.Duration) (*Lister, *stubProvider, *stubProvider, stubCredentials) {
	t.Helper()
	gh := &stubProvider{kind: GitHub}
	bb := &stubProvider{kind: Bitbucket}
	creds := stubCredentials{
		"user-1": {GitHub: {Secret: "gh"}, Bitbucket: {Secret: "bb", Identity: "alice@example.test"}},
		"user-2": {GitHub: {Secret: "other"}},
	}
	return NewLister(NewRegistry(gh, bb), creds, ttl), gh, bb, creds
}

func TestListMergesConnectedAccountsNewestFirst(t *testing.T) {
	lister, gh, bb, _ := newTestLister(t, time.Minute)
	gh.repos = []Repo{repo(GitHub, "acme/old", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	bb.repos = []Repo{repo(Bitbucket, "acme/new", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))}

	listing, err := lister.List(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Repos) != 2 {
		t.Fatalf("got %d repositories, want one from each account", len(listing.Repos))
	}
	// One list, ordered by activity, whichever account each came from.
	if listing.Repos[0].FullName != "acme/new" || listing.Repos[1].FullName != "acme/old" {
		t.Errorf("order = %+v, want newest first across providers", listing.Repos)
	}
	if len(listing.Failed) != 0 {
		t.Errorf("failures reported when there were none: %v", listing.Failed)
	}
}

// One account being down must not hide the other's repositories.
func TestListReportsOneFailureWithoutLosingTheRest(t *testing.T) {
	lister, gh, bb, _ := newTestLister(t, time.Minute)
	gh.repos = []Repo{repo(GitHub, "acme/widgets", time.Now())}
	bb.err = fmt.Errorf("listing: %w", ErrUnauthorized)

	listing, err := lister.List(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listing.Repos) != 1 || listing.Repos[0].FullName != "acme/widgets" {
		t.Errorf("repositories = %+v, want the working account's", listing.Repos)
	}
	if !errors.Is(listing.Failed[Bitbucket], ErrUnauthorized) {
		t.Errorf("failure for bitbucket = %v, want the provider's error", listing.Failed[Bitbucket])
	}
}

func TestListCachesPerUserAndProvider(t *testing.T) {
	lister, gh, bb, _ := newTestLister(t, time.Minute)
	ctx := context.Background()

	if _, err := lister.List(ctx, "user-1"); err != nil {
		t.Fatalf("first List: %v", err)
	}
	if _, err := lister.List(ctx, "user-1"); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if gh.listings.Load() != 1 || bb.listings.Load() != 1 {
		t.Errorf("listings = github %d, bitbucket %d; want one each, the rest cached",
			gh.listings.Load(), bb.listings.Load())
	}

	// Another user has their own accounts, so their listing is their own.
	if _, err := lister.List(ctx, "user-2"); err != nil {
		t.Fatalf("List for another user: %v", err)
	}
	if gh.listings.Load() != 2 {
		t.Error("another user's listing was served from the first user's cache")
	}
	if bb.listings.Load() != 1 {
		t.Error("a provider that user-2 has not connected was called anyway")
	}

	lister.Invalidate("user-1")
	if _, err := lister.List(ctx, "user-1"); err != nil {
		t.Fatalf("List after Invalidate: %v", err)
	}
	if gh.listings.Load() != 3 || bb.listings.Load() != 2 {
		t.Errorf("Invalidate did not clear every provider for the user: github %d, bitbucket %d",
			gh.listings.Load(), bb.listings.Load())
	}
}

func TestListExpiresTheCache(t *testing.T) {
	lister, gh, _, _ := newTestLister(t, time.Nanosecond)
	ctx := context.Background()

	if _, err := lister.List(ctx, "user-1"); err != nil {
		t.Fatalf("first List: %v", err)
	}
	if _, err := lister.List(ctx, "user-1"); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if gh.listings.Load() != 2 {
		t.Errorf("made %d listings, want 2: an expired entry must be refetched", gh.listings.Load())
	}
}

// Find is what decides a session's clone URL, so it has to match the right
// repository of the right account, and refuse anything else.
func TestFindMatchesWithinAProvider(t *testing.T) {
	lister, gh, bb, _ := newTestLister(t, time.Minute)
	// The same name on both accounts, which is exactly why the provider is part
	// of the request.
	gh.repos = []Repo{{Provider: GitHub, FullName: "acme/widgets", CloneURL: "https://github.test/w.git"}}
	bb.repos = []Repo{{Provider: Bitbucket, FullName: "acme/widgets", CloneURL: "https://bitbucket.test/w.git"}}
	ctx := context.Background()

	found, err := lister.Find(ctx, "user-1", Bitbucket, "acme/widgets")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.CloneURL != "https://bitbucket.test/w.git" {
		t.Errorf("found %+v, want the Bitbucket one", found)
	}

	// Names are compared the way both providers treat them.
	if _, err := lister.Find(ctx, "user-1", GitHub, "ACME/Widgets"); err != nil {
		t.Errorf("Find with different case: %v", err)
	}
	// No provider named: any account will do, which is what an older client
	// sends.
	if _, err := lister.Find(ctx, "user-1", "", "acme/widgets"); err != nil {
		t.Errorf("Find without a provider: %v", err)
	}
	if _, err := lister.Find(ctx, "user-1", GitHub, "acme/nothing"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("error = %v, want ErrRepoNotFound", err)
	}
}

// A repository missing because the account was rejected is a different problem
// from one that does not exist, and the user needs to be told which.
func TestFindReportsARejectedAccount(t *testing.T) {
	lister, gh, _, _ := newTestLister(t, time.Minute)
	gh.err = fmt.Errorf("listing: %w", ErrUnauthorized)

	_, err := lister.Find(context.Background(), "user-1", GitHub, "acme/widgets")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized", err)
	}
}
