package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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

func TestNewAllowlistRefusesAnEmptyList(t *testing.T) {
	for name, entries := range map[string][]string{
		"nothing at all":    nil,
		"only empty values": {"", "  "},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAllowlist(entries); err == nil {
				t.Error("NewAllowlist accepted an empty list: every GitHub account would be admitted")
			}
		})
	}
}

func TestAllowlistIsCaseInsensitive(t *testing.T) {
	list, err := NewAllowlist([]string{"Alice", " bob "})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	for _, login := range []string{"alice", "ALICE", "bob"} {
		if !list.Allowed(login) {
			t.Errorf("Allowed(%q) = false, want true", login)
		}
	}
	if list.Allowed("mallory") {
		t.Error("the allowlist matched a login it does not contain")
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

	token, err := o.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if token != "gho_token" {
		t.Errorf("token = %q", token)
	}
	if gotForm.Get("code") != "the-code" || gotForm.Get("client_secret") != "secret" {
		t.Errorf("unexpected form: %v", gotForm)
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

	_, err := o.Exchange(context.Background(), "stale")
	if err == nil {
		t.Fatal("Exchange accepted an error document")
	}
	if !strings.Contains(err.Error(), "bad_verification_code") {
		t.Errorf("error = %v, want it to mention the GitHub error code", err)
	}
}
