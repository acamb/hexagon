// Package bitbucket talks to the Bitbucket Cloud REST API on behalf of a
// Hexagon user.
//
// Authentication is an Atlassian API token. App passwords, the older
// mechanism, were removed by Atlassian in July 2026 and now fail outright, so
// they are not supported here. The token is used in two different ways, which
// is the reason this package exists rather than a few more branches in the
// GitHub client:
//
//   - the REST API takes the Atlassian account email as the basic-auth user;
//   - git over HTTPS takes the fixed placeholder x-bitbucket-api-token-auth.
package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/provider"
)

// DefaultBaseURL is the Bitbucket Cloud API root, version included: every
// endpoint below is relative to it.
const DefaultBaseURL = "https://api.bitbucket.org/2.0"

// gitUsername is what Bitbucket wants in place of a username when the password
// is an API token. It is a constant, not the account's name.
const gitUsername = "x-bitbucket-api-token-auth"

// maxRepoPages bounds the walk over the paginated listing, as the GitHub client
// does: a misbehaving server cannot keep us looping.
const maxRepoPages = 20

// Client is a minimal Bitbucket Cloud client. The zero value is not usable;
// call New.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a client talking to api.bitbucket.org.
func New() *Client {
	return &Client{
		baseURL: DefaultBaseURL,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// NewWithBaseURL points the client at another host, for tests and for a
// development stub.
func NewWithBaseURL(baseURL string) *Client {
	c := New()
	if baseURL != "" {
		c.baseURL = strings.TrimRight(baseURL, "/")
	}
	return c
}

// Kind identifies this provider.
func (c *Client) Kind() provider.Kind { return provider.Bitbucket }

// GitCredentials are what git wants over HTTPS: the placeholder username and
// the API token. Deliberately not the account email, which is what the REST API
// wants — the two are not interchangeable.
func (c *Client) GitCredentials(cred provider.Credentials) provider.GitAuth {
	return provider.GitAuth{Username: gitUsername, Secret: cred.Secret}
}

// account is the shape of GET /2.0/user.
type account struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Links       struct {
		Avatar struct {
			Href string `json:"href"`
		} `json:"avatar"`
	} `json:"links"`
}

// workspacePage is one page of GET /2.0/user/workspaces.
type workspacePage struct {
	Values []struct {
		Slug string `json:"slug"`
	} `json:"values"`
	Next string `json:"next"`
}

// workspaces names the workspaces the account belongs to.
//
// It is the first half of every listing, because repositories can only be asked
// for one workspace at a time: Atlassian retired the cross-workspace listing —
// GET /2.0/repositories, now 410 Gone (CHANGE-2770) — and did not replace it.
func (c *Client) workspaces(ctx context.Context, cred provider.Credentials) ([]string, error) {
	next := c.baseURL + "/user/workspaces?pagelen=100"

	var slugs []string
	for page := 0; next != "" && page < maxRepoPages; page++ {
		var batch workspacePage
		if _, err := c.get(ctx, cred, next, &batch); err != nil {
			return nil, err
		}
		for _, workspace := range batch.Values {
			if workspace.Slug != "" {
				slugs = append(slugs, workspace.Slug)
			}
		}
		next = batch.Next
	}
	return slugs, nil
}

// Verify checks the credentials against the call a listing starts from.
//
// GET /2.0/user is not used for this. It carries a scope of its own, so a token
// scoped for what Hexagon actually does would be refused there while being
// perfectly able to list and clone.
func (c *Client) Verify(ctx context.Context, cred provider.Credentials) (provider.Account, error) {
	if _, err := c.workspaces(ctx, cred); err != nil {
		return provider.Account{}, err
	}
	name, avatar := c.accountName(ctx, cred)
	return provider.Account{
		Kind:      provider.Bitbucket,
		Account:   name,
		Identity:  cred.Identity,
		AvatarURL: avatar,
	}, nil
}

// accountName is the name to show for a connected account. It is a nicety, not
// a check: a token scoped only for repositories cannot read the account
// endpoint, and the email it was connected with is a perfectly good label.
func (c *Client) accountName(ctx context.Context, cred provider.Credentials) (name, avatar string) {
	var user account
	if _, err := c.get(ctx, cred, c.baseURL+"/user", &user); err != nil || user.Username == "" {
		return cred.Identity, ""
	}
	return user.Username, user.Links.Avatar.Href
}

// repoPage is one page of GET /2.0/repositories. Unlike GitHub, the next page
// is a URL in the body rather than a Link header.
type repoPage struct {
	Values []repo `json:"values"`
	Next   string `json:"next"`
}

type repo struct {
	FullName    string `json:"full_name"`
	Description string `json:"description"`
	IsPrivate   bool   `json:"is_private"`
	UpdatedOn   string `json:"updated_on"`
	MainBranch  struct {
		Name string `json:"name"`
	} `json:"mainbranch"`
	Links struct {
		Clone []struct {
			Name string `json:"name"`
			Href string `json:"href"`
		} `json:"clone"`
	} `json:"links"`
}

// ListRepos returns every repository the credentials can reach, workspace by
// workspace.
//
// There is no way to ask for all of them at once: Atlassian retired the
// cross-workspace listing — GET /2.0/repositories, now 410 Gone (CHANGE-2770) —
// and said no equivalent is coming back. So a listing is one call to find the
// workspaces and one per workspace after that.
func (c *Client) ListRepos(ctx context.Context, cred provider.Credentials) ([]provider.Repo, error) {
	workspaces, err := c.workspaces(ctx, cred)
	if err != nil {
		return nil, err
	}

	var repos []provider.Repo
	for _, workspace := range workspaces {
		found, err := c.workspaceRepos(ctx, cred, workspace, maxRepoPages)
		if err != nil {
			return nil, err
		}
		repos = append(repos, found...)
	}
	return repos, nil
}

// workspaceRepos lists one workspace, following pages up to maxPages.
func (c *Client) workspaceRepos(ctx context.Context, cred provider.Credentials, workspace string, maxPages int) ([]provider.Repo, error) {
	next := c.baseURL + "/repositories/" + url.PathEscape(workspace) + "?" + url.Values{
		// The account's own repositories in that workspace, rather than every
		// repository it can see.
		"role":    {"member"},
		"pagelen": {"100"},
		"sort":    {"-updated_on"},
	}.Encode()

	var repos []provider.Repo
	for page := 0; next != "" && page < maxPages; page++ {
		var batch repoPage
		if _, err := c.get(ctx, cred, next, &batch); err != nil {
			return nil, fmt.Errorf("workspace %s: %w", workspace, err)
		}
		for _, r := range batch.Values {
			repos = append(repos, provider.Repo{
				Provider:      provider.Bitbucket,
				FullName:      r.FullName,
				CloneURL:      httpsClone(r),
				DefaultBranch: r.MainBranch.Name,
				Private:       r.IsPrivate,
				Description:   r.Description,
				UpdatedAt:     parseTime(r.UpdatedOn),
			})
		}
		next = batch.Next
	}
	return repos, nil
}

// httpsClone picks the https clone URL out of the links Bitbucket offers. The
// other one is ssh, which Hexagon has no key for.
func httpsClone(r repo) string {
	for _, link := range r.Links.Clone {
		if link.Name == "https" {
			return link.Href
		}
	}
	return ""
}

// parseTime reads Bitbucket's timestamps, which carry more decimal places than
// RFC 3339 requires but parse as it. A value we cannot read sorts oldest rather
// than failing the whole listing.
func parseTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

// errorMessage pulls the human half out of a Bitbucket error document, which is
// shaped {"type":"error","error":{"message":"…"}}. It is what the user is shown
// when a token is refused, so it has to be Bitbucket's own words: a message we
// invented would be a guess at the cause presented as a fact.
func errorMessage(body []byte) string {
	var document struct {
		Error struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &document); err == nil {
		if message := strings.TrimSpace(document.Error.Message + " " + document.Error.Detail); message != "" {
			return message
		}
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return trimmed
	}
	return "no explanation given"
}

// get performs a request and decodes the body, returning the raw response for
// callers that need more than the payload.
func (c *Client) get(ctx context.Context, cred provider.Credentials, url string, out any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "hexagon")
	// The API's basic-auth user is the Atlassian account email, held as the
	// account's Identity.
	req.SetBasicAuth(cred.Identity, cred.Secret)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitbucket: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("bitbucket: %s: %s", resp.Status, errorMessage(body))
		// 403 is what Bitbucket answers for a token whose scopes are too narrow,
		// which the user fixes the same way as a rejected one.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			err = fmt.Errorf("%w: %s", provider.ErrUnauthorized, err)
		}
		return nil, err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("bitbucket: decode %s: %w", url, err)
	}
	return resp, nil
}
