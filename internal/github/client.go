// Package github talks to the GitHub REST API on behalf of a Hexagon user.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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
	if err := c.get(ctx, token, "/user", &user); err != nil {
		return nil, err
	}
	if user.Login == "" {
		return nil, fmt.Errorf("github: /user returned no login")
	}
	return &user, nil
}

func (c *Client) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Bodies on errors are short JSON documents; include a slice of it so
		// the cause ("Bad credentials") reaches the logs.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github: GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github: decode %s: %w", path, err)
	}
	return nil
}
