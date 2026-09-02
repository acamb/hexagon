package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func validOAuthConfig() OAuthConfig {
	return OAuthConfig{
		ClientID:     "client",
		ClientSecret: "secret",
		PublicURL:    "http://127.0.0.1:8080",
	}
}

func TestNewOAuthFailsClosed(t *testing.T) {
	tests := map[string]func(*OAuthConfig){
		"no client id":     func(c *OAuthConfig) { c.ClientID = "" },
		"no client secret": func(c *OAuthConfig) { c.ClientSecret = "" },
	}
	for name, mangle := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validOAuthConfig()
			mangle(&cfg)
			if _, err := NewOAuth(cfg); err == nil {
				t.Fatal("NewOAuth accepted the configuration, want an error")
			}
		})
	}
}

func TestOAuthAuthorizeURL(t *testing.T) {
	o, _ := NewOAuth(validOAuthConfig())
	raw := o.AuthorizeURL("state-value")

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := u.Query()
	if got := q.Get("state"); got != "state-value" {
		t.Errorf("state = %q", got)
	}
	if got := q.Get("client_id"); got != "client" {
		t.Errorf("client_id = %q", got)
	}
	if got := q.Get("scope"); got != scopeRepo {
		t.Errorf("scope = %q, want %q", got, scopeRepo)
	}
	if want := "http://127.0.0.1:8080/api/auth/callback"; q.Get("redirect_uri") != want {
		t.Errorf("redirect_uri = %q, want %q", q.Get("redirect_uri"), want)
	}
}

func TestOAuthExchange(t *testing.T) {
	var gotForm url.Values
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"gho_token","token_type":"bearer"}`))
	}))
	defer stub.Close()

	cfg := validOAuthConfig()
	cfg.TokenURL = stub.URL
	o, _ := NewOAuth(cfg)

	token, expiresAt, err := o.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if token != "gho_token" {
		t.Errorf("token = %q", token)
	}
	if !expiresAt.IsZero() {
		t.Errorf("expiresAt = %v, want zero: this response carried no expires_in", expiresAt)
	}
	if gotForm.Get("code") != "the-code" || gotForm.Get("client_secret") != "secret" {
		t.Errorf("unexpected form: %v", gotForm)
	}
}

// A token that expires (a GitHub App user token, or an OAuth App with token
// expiration on) carries expires_in, and Exchange turns it into the moment the
// token dies so the login session can be bound to it.
func TestOAuthExchangeReturnsTokenExpiry(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"gho_token","token_type":"bearer","expires_in":28800}`))
	}))
	defer stub.Close()

	cfg := validOAuthConfig()
	cfg.TokenURL = stub.URL
	o, _ := NewOAuth(cfg)

	before := time.Now()
	_, expiresAt, err := o.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	// 28800s = 8h; allow a wide window so the test is not clock-flaky.
	want := before.Add(8 * time.Hour)
	if expiresAt.Before(want.Add(-time.Minute)) || expiresAt.After(want.Add(time.Minute)) {
		t.Errorf("expiresAt = %v, want ~%v (now + expires_in)", expiresAt, want)
	}
}

// GitHub reports a stale or reused code with HTTP 200 and an error document,
// so the body has to be checked rather than the status alone.
func TestOAuthExchangeRejectsErrorDocument(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect"}`))
	}))
	defer stub.Close()

	cfg := validOAuthConfig()
	cfg.TokenURL = stub.URL
	o, _ := NewOAuth(cfg)

	_, _, err := o.Exchange(context.Background(), "stale")
	if err == nil {
		t.Fatal("Exchange accepted an error document")
	}
	if !strings.Contains(err.Error(), "bad_verification_code") {
		t.Errorf("error = %v, want it to mention the GitHub error code", err)
	}
}
