package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/dockerx"
)

// The headers are set outside the router, so what matters is that they reach
// both kinds of response the server produces: the API and the SPA itself.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	env := newTestEnv(t, "alice")

	for _, path := range []string{"/api/health", "/"} {
		resp := env.do(http.MethodGet, path, nil)
		for _, c := range []struct{ header, want string }{
			{"X-Content-Type-Options", "nosniff"},
			{"Referrer-Policy", "same-origin"},
			{"Content-Security-Policy", contentSecurityPolicy},
		} {
			if got := resp.Header.Get(c.header); got != c.want {
				t.Errorf("%s on %s = %q, want %q", c.header, path, got, c.want)
			}
		}
		// newTestEnv's public URL is http, where HSTS is ignored by the browser
		// and remembered by nobody. Emitting it there would be a trap on a
		// development machine that outlives the instance.
		if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("HSTS on %s = %q, want none over http", path, got)
		}
	}
}

func TestSecurityHeadersEmitHSTSOverHTTPS(t *testing.T) {
	s := &Server{cfg: &config.Config{PublicURL: "https://hexagon.example"}}
	handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if got := rec.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Errorf("HSTS = %q", got)
	}
}

// code-server needs inline scripts and workers, so the policy written for our
// own pages must not reach it: it would break the editor rather than protect it.
func TestSecurityHeadersExemptTheVSCodeProxyFromTheCSP(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	env.insertVSCodeSession("s-vscode", env.userID(), true, "container-vscode", true)
	env.docker.setContainerPort("container-vscode", dockerx.VSCodePort, backendPort(t, backend))

	resp := env.do(http.MethodGet, "/api/sessions/s-vscode/vscode/foo", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		t.Errorf("CSP on the proxy route = %q, want none", got)
	}
	// The headers that cost the editor nothing are still there.
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options on the proxy route = %q, want nosniff", got)
	}
}
