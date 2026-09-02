package httpapi

import (
	"context"
	"sync"

	"github.com/andrea/hexagon/internal/provider"
)

// fakeProvider stands in for a source of repositories. The tests wire real
// provider.Lister and auth machinery around these, so what is faked is the
// network and nothing else: the credentials still travel from the database,
// through the cipher, to the provider that asks for them.
type fakeProvider struct {
	kind provider.Kind

	mu      sync.Mutex
	repos   []provider.Repo
	err     error
	account string
	// seen records the credentials each call arrived with, which is how a test
	// checks that a listing used the caller's own account.
	seen []provider.Credentials
}

func newFakeProvider(kind provider.Kind, account string) *fakeProvider {
	return &fakeProvider{kind: kind, account: account}
}

func (f *fakeProvider) Kind() provider.Kind { return f.kind }

func (f *fakeProvider) Verify(_ context.Context, c provider.Credentials) (provider.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, c)
	if f.err != nil {
		return provider.Account{}, f.err
	}
	return provider.Account{Kind: f.kind, Account: f.account, Identity: c.Identity}, nil
}

func (f *fakeProvider) ListRepos(_ context.Context, c provider.Credentials) ([]provider.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, c)
	if f.err != nil {
		return nil, f.err
	}
	return append([]provider.Repo(nil), f.repos...), nil
}

// GitCredentials mirrors what the real providers do: a placeholder username
// that is not the account name, and the secret git should use as the password —
// the pasted one when there is one, which is what SecretForGit answers.
func (f *fakeProvider) GitCredentials(c provider.Credentials) provider.GitAuth {
	return provider.GitAuth{Username: "x-" + string(f.kind) + "-token", Secret: c.SecretForGit()}
}

// offer adds a repository to what this provider will list.
func (f *fakeProvider) offer(fullName, defaultBranch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, provider.Repo{
		Provider:      f.kind,
		FullName:      fullName,
		CloneURL:      "https://" + string(f.kind) + ".test/" + fullName + ".git",
		DefaultBranch: defaultBranch,
	})
}

// identify changes the account Verify reports, so a test can hand over a token
// that belongs to somebody else.
func (f *fakeProvider) identify(account string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.account = account
}

func (f *fakeProvider) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// credentials returns what the provider was called with.
func (f *fakeProvider) credentials() []provider.Credentials {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Credentials(nil), f.seen...)
}
