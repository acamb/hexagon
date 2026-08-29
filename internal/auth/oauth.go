package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAuthorizeURL = "https://github.com/login/oauth/authorize"
	defaultTokenURL     = "https://github.com/login/oauth/access_token"

	// scopeRepo is what Claude Code needs inside the container: read the
	// repository, and push back what it changed.
	scopeRepo = "repo"
)

// OAuth drives the GitHub OAuth App login.
type OAuth struct {
	clientID     string
	clientSecret string
	redirectURI  string

	authorizeURL string
	tokenURL     string
	http         *http.Client
}

// OAuthConfig collects what NewOAuth needs from the process configuration.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	PublicURL    string

	// AuthorizeURL and TokenURL default to github.com. They exist so tests can
	// point at a stub, and so a GitHub Enterprise host could be used one day.
	AuthorizeURL string
	TokenURL     string
}

// NewOAuth validates the OAuth configuration and builds the provider. Who may
// sign in is not decided here: that is the Allowlist, which every request
// consults and not only this handshake.
func NewOAuth(cfg OAuthConfig) (*OAuth, error) {
	switch {
	case cfg.ClientID == "":
		return nil, errors.New("HEXAGON_GITHUB_CLIENT_ID is required")
	case cfg.ClientSecret == "":
		return nil, errors.New("HEXAGON_GITHUB_CLIENT_SECRET is required")
	}

	return &OAuth{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURI:  strings.TrimRight(cfg.PublicURL, "/") + "/api/auth/callback",
		authorizeURL: orDefault(cfg.AuthorizeURL, defaultAuthorizeURL),
		tokenURL:     orDefault(cfg.TokenURL, defaultTokenURL),
		http:         &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// RedirectURI is the callback GitHub must be configured to call.
func (o *OAuth) RedirectURI() string { return o.redirectURI }

// AuthorizeURL is where the browser is sent to start the login.
func (o *OAuth) AuthorizeURL(state string) string {
	q := url.Values{
		"client_id":    {o.clientID},
		"redirect_uri": {o.redirectURI},
		"scope":        {scopeRepo},
		"state":        {state},
	}
	return o.authorizeURL + "?" + q.Encode()
}

// Exchange trades the callback code for an access token.
func (o *OAuth) Exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {o.clientID},
		"client_secret": {o.clientSecret},
		"code":          {code},
		"redirect_uri":  {o.redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange oauth code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("exchange oauth code: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	// GitHub answers 200 with an error document when the code is stale, so the
	// body has to be inspected rather than the status alone.
	var payload struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode oauth token response: %w", err)
	}
	switch {
	case payload.Error != "":
		return "", fmt.Errorf("exchange oauth code: %s: %s", payload.Error, payload.ErrorDescription)
	case payload.AccessToken == "":
		return "", errors.New("exchange oauth code: no access token in response")
	}
	return payload.AccessToken, nil
}
