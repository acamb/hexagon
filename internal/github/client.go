// Package github talks to the GitHub REST API on behalf of a Hexagon user.
//
// It serves two roles: the login, which is GitHub-specific and lives in
// CurrentUser, and one implementation of provider.Provider, which is how the
// rest of Hexagon reaches repositories without knowing where they come from.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/provider"
)

// ErrUnauthorized reports a token GitHub no longer accepts: revoked, expired,
// or with the app's access removed. The caller has to sign in again; retrying
// will not help. It wraps provider.ErrUnauthorized so callers that do not know
// which provider they are talking to can still recognise it.
var ErrUnauthorized = fmt.Errorf("%w: github: token rejected", provider.ErrUnauthorized)

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"
	userAgent      = "hexagon"
)

// Client is a minimal GitHub API client. The zero value is not usable; call New.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a client talking to api.github.com.
func New() *Client {
	return &Client{
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// NewWithBaseURL points the client at another host. Tests use it; so would
// GitHub Enterprise, should it ever come up.
func NewWithBaseURL(baseURL string) *Client {
	c := New()
	c.baseURL = strings.TrimRight(baseURL, "/")
	return c
}

// User is the subset of a GitHub account Hexagon stores.
type User struct {
	Login     string `json:"login"`
	ID        int64  `json:"id"`
	AvatarURL string `json:"avatar_url"`
}

// CurrentUser identifies the owner of token.
func (c *Client) CurrentUser(ctx context.Context, token string) (*User, error) {
	var user User
	if _, err := c.get(ctx, token, c.baseURL+"/user", &user); err != nil {
		return nil, err
	}
	if user.Login == "" {
		return nil, fmt.Errorf("github: /user returned no login")
	}
	return &user, nil
}

// Kind identifies this provider.
func (c *Client) Kind() provider.Kind { return provider.GitHub }

// GitCredentials are what git wants over HTTPS: the placeholder GitHub expects
// alongside a token, and the token itself. The username works for a personal
// access token as well as for an OAuth one, so preferring the pasted secret
// changes nothing else here.
func (c *Client) GitCredentials(cred provider.Credentials) provider.GitAuth {
	return provider.GitAuth{Username: "x-access-token", Secret: cred.SecretForGit()}
}

// Verify identifies the account behind a token. The login is the same call the
// OAuth callback makes, reached through the provider interface.
func (c *Client) Verify(ctx context.Context, cred provider.Credentials) (provider.Account, error) {
	user, err := c.CurrentUser(ctx, cred.Secret)
	if err != nil {
		return provider.Account{}, err
	}
	return provider.Account{
		Kind:      provider.GitHub,
		Account:   user.Login,
		AvatarURL: user.AvatarURL,
	}, nil
}

// repo is the subset of a repository the session picker needs, in the shape
// GitHub sends it.
type repo struct {
	FullName      string    `json:"full_name"`
	CloneURL      string    `json:"clone_url"`
	DefaultBranch string    `json:"default_branch"`
	Private       bool      `json:"private"`
	Description   string    `json:"description"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// maxRepoPages bounds the walk over the Link header. At 100 repositories per
// page this covers accounts far larger than the picker can usefully show, and
// it means a misbehaving server cannot keep us looping.
const maxRepoPages = 20

// ListRepos returns every repository the credentials can reach, newest activity
// first: owned, collaborated on, and through organisation membership.
func (c *Client) ListRepos(ctx context.Context, cred provider.Credentials) ([]provider.Repo, error) {
	next := c.baseURL + "/user/repos?per_page=100&sort=updated&affiliation=owner,collaborator,organization_member"

	var repos []provider.Repo
	for page := 0; next != "" && page < maxRepoPages; page++ {
		var batch []repo
		link, err := c.get(ctx, cred.Secret, next, &batch)
		if err != nil {
			return nil, err
		}
		for _, r := range batch {
			repos = append(repos, provider.Repo{
				Provider:      provider.GitHub,
				FullName:      r.FullName,
				CloneURL:      r.CloneURL,
				DefaultBranch: r.DefaultBranch,
				Private:       r.Private,
				Description:   r.Description,
				UpdatedAt:     r.UpdatedAt,
			})
		}
		next = nextPageURL(link)
	}
	return repos, nil
}

// nextPageURL pulls the rel="next" target out of a Link header, which is how
// GitHub paginates: <url>; rel="next", <url>; rel="last".
func nextPageURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		segments := strings.Split(strings.TrimSpace(part), ";")
		if len(segments) < 2 {
			continue
		}
		target := strings.TrimSpace(segments[0])
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		for _, attr := range segments[1:] {
			if strings.TrimSpace(attr) == `rel="next"` {
				return target[1 : len(target)-1]
			}
		}
	}
	return ""
}

// get performs a request and decodes the body, returning the Link header so
// callers can follow pagination.
func (c *Client) get(ctx context.Context, token, url string, out any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Bodies on errors are short JSON documents; include a slice of it so
		// the cause ("Bad credentials") reaches the logs.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("github: GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
		if resp.StatusCode == http.StatusUnauthorized {
			err = fmt.Errorf("%w: %s", ErrUnauthorized, err)
		}
		return "", err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", fmt.Errorf("github: decode %s: %w", url, err)
	}
	return resp.Header.Get("Link"), nil
}
