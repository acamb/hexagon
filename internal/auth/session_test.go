package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/andrea/hexagon/internal/store"
)

// newTestService builds a session service over a real database, since issuing
// and resolving a session both go through it.
func newTestService(t *testing.T, publicURL string) (*Service, *store.User) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cipher, err := NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	user, err := st.UpsertUser(context.Background(), &store.User{GitHubLogin: "alice", GitHubID: 42})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return NewService(st, cipher, publicURL), user
}

// The __Host- prefix is a promise to the browser about three attributes, and the
// browser drops the cookie if any of them is missing. Over plaintext the prefix
// cannot be used at all, so the name depends on the deployment.
func TestSessionCookieCarriesTheHostPrefixOverHTTPS(t *testing.T) {
	for _, c := range []struct {
		name       string
		publicURL  string
		wantName   string
		wantSecure bool
	}{
		{"plaintext", "http://127.0.0.1:8080", "hexagon_session", false},
		{"https", "https://hexagon.example", "__Host-hexagon_session", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, user := newTestService(t, c.publicURL)

			rec := httptest.NewRecorder()
			if err := svc.Issue(context.Background(), rec, user); err != nil {
				t.Fatalf("Issue: %v", err)
			}
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("Issue set %d cookies, want 1", len(cookies))
			}
			cookie := cookies[0]

			if cookie.Name != c.wantName {
				t.Errorf("cookie name = %q, want %q", cookie.Name, c.wantName)
			}
			if cookie.Secure != c.wantSecure {
				t.Errorf("Secure = %v, want %v", cookie.Secure, c.wantSecure)
			}
			// The other two halves of the prefix's contract, which hold in both
			// modes and must keep holding.
			if cookie.Path != "/" {
				t.Errorf("Path = %q, want /", cookie.Path)
			}
			if cookie.Domain != "" {
				t.Errorf("Domain = %q, want none", cookie.Domain)
			}
			if !cookie.HttpOnly {
				t.Error("HttpOnly = false, want true")
			}

			// Issue, Authenticate and Logout have to agree about the name, which
			// is the reason it is a method rather than three constants.
			req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			req.AddCookie(cookie)
			got, err := svc.Authenticate(context.Background(), req)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if got.ID != user.ID {
				t.Errorf("authenticated user = %q, want %q", got.ID, user.ID)
			}

			cleared := httptest.NewRecorder()
			if err := svc.Logout(context.Background(), cleared, req); err != nil {
				t.Fatalf("Logout: %v", err)
			}
			if _, err := svc.Authenticate(context.Background(), req); err != ErrNoSession {
				t.Errorf("after Logout: err = %v, want ErrNoSession", err)
			}
			if got := cleared.Result().Cookies()[0]; got.Name != c.wantName || got.MaxAge != -1 {
				t.Errorf("cleared cookie = %q with MaxAge %d, want %q with -1", got.Name, got.MaxAge, c.wantName)
			}
		})
	}
}
