package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/store"
)

// insertVSCodeSession adds a session row for userID directly, with the
// integration on or off as asked, so the proxy's authorization can be driven
// without going through the full create flow.
func (e *testEnv) insertVSCodeSession(id, userID string, vscode bool, containerID string, running bool) {
	e.t.Helper()

	_, err := e.store.DB().Exec(`
		INSERT OR IGNORE INTO images (id, user_id, name, source_type, dockerfile, registry_ref,
			image_ref, status, build_log, error, created_at)
		VALUES ('img-vscode', ?, 'base', 'dockerfile', 'FROM busybox', '', 'hexagon/img-vscode:latest', 'ready', '', '',
			'2026-01-01 00:00:00.000')`, userID)
	if err != nil {
		e.t.Fatalf("insert image: %v", err)
	}

	status := store.SessionStatusStopped
	if running {
		status = store.SessionStatusRunning
	}
	_, err = e.store.DB().Exec(`
		INSERT INTO sessions (id, user_id, title, repo_full_name, repo_clone_url, branch, image_id,
			image_ref, workspace_dir, repo_dir, container_id, status, error, vscode, created_at, updated_at)
		VALUES (?, ?, 'demo', 'acme/widgets', 'https://github.com/acme/widgets.git', 'main', 'img-vscode',
			'hexagon/img-vscode:latest', '/w', '/w/repo', ?, ?, '', ?, '2026-01-01 00:00:00.000', '2026-01-01 00:00:00.000')`,
		id, userID, containerID, status, vscode)
	if err != nil {
		e.t.Fatalf("insert session: %v", err)
	}
	if containerID != "" {
		e.docker.addContainer(containerID, running)
	}
}

// backendPort parses the port httptest bound its server to, which is what the
// fake container has to report as its published host port.
func backendPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}
	return port
}

func TestVSCodeProxyForwardsRequests(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var gotPath, gotMethod, gotContentType string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		w.Write([]byte("hello from code-server"))
	}))
	defer backend.Close()

	env.insertVSCodeSession("s-vscode", env.userID(), true, "container-vscode", true)
	env.docker.setContainerPort("container-vscode", dockerx.VSCodePort, backendPort(t, backend))

	resp := env.do(http.MethodGet, "/api/sessions/s-vscode/vscode/foo?x=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	if got := string(body[:n]); got != "hello from code-server" {
		t.Errorf("body = %q", got)
	}
	if gotPath != "/foo?x=1" {
		t.Errorf("backend saw path %q, want /foo?x=1", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("backend saw method %q", gotMethod)
	}

	// The two halves of one decision: the proxy carries traffic that is not
	// JSON, but the ordinary API still refuses it. The Origin is what a browser
	// sends on any POST, and the proxy route demands it; the subject here is
	// the content type.
	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/api/sessions/s-vscode/vscode/edit", strings.NewReader("plain text"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	posted, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("post to proxy: %v", err)
	}
	defer posted.Body.Close()
	if posted.StatusCode != http.StatusOK {
		t.Errorf("a non-JSON POST to the proxy = %d, want 200", posted.StatusCode)
	}
	if gotContentType != "text/plain" {
		t.Errorf("backend saw content type %q, want text/plain", gotContentType)
	}

	sessionsReq, err := http.NewRequest(http.MethodPost, env.server.URL+"/api/sessions", strings.NewReader("plain text"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	sessionsReq.Header.Set("Content-Type", "text/plain")
	rejected, err := env.client.Do(sessionsReq)
	if err != nil {
		t.Fatalf("post to sessions: %v", err)
	}
	defer rejected.Body.Close()
	if rejected.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("a non-JSON POST to /api/sessions = %d, want 415: the exemption leaked", rejected.StatusCode)
	}
}

func TestVSCodeProxyRedirectsToTheTrailingSlash(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	env.insertVSCodeSession("s-vscode", env.userID(), true, "container-vscode", true)
	env.docker.setContainerPort("container-vscode", dockerx.VSCodePort, backendPort(t, backend))

	resp := env.do(http.MethodGet, "/api/sessions/s-vscode/vscode", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/api/sessions/s-vscode/vscode/" {
		t.Errorf("redirect target = %q", got)
	}
}

func TestVSCodeProxyRequiresASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	if got := env.do(http.MethodGet, "/api/sessions/s-vscode/vscode/", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("no cookie: status = %d, want 401", got)
	}
}

func TestVSCodeProxyIsScopedToTheOwner(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	other, err := env.store.UpsertUser(context.Background(), &store.User{GitHubLogin: "bob", GitHubID: 7})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	env.insertVSCodeSession("theirs", other.ID, true, "container-theirs", true)

	if got := env.do(http.MethodGet, "/api/sessions/theirs/vscode/", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("another user's session: status = %d, want 404", got)
	}
}

func TestVSCodeProxyRejectsASessionWithoutTheIntegration(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertVSCodeSession("s-plain", env.userID(), false, "container-plain", true)

	if got := env.do(http.MethodGet, "/api/sessions/s-plain/vscode/", nil).StatusCode; got != http.StatusConflict {
		t.Errorf("a session without the integration: status = %d, want 409", got)
	}
}

func TestVSCodeProxyRejectsASessionThatIsNotRunning(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertVSCodeSession("s-stopped", env.userID(), true, "container-stopped", false)

	if got := env.do(http.MethodGet, "/api/sessions/s-stopped/vscode/", nil).StatusCode; got != http.StatusConflict {
		t.Errorf("a stopped session: status = %d, want 409", got)
	}
}

// The container behind the proxy runs an agent over repository content, and the
// session cookie is the credential for the whole API. It has no business
// crossing that boundary.
func TestVSCodeProxyStripsTheCallersCredentials(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var gotCookie, gotAuthorization string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotAuthorization = r.Header.Get("Authorization")
		w.Write([]byte("hello from code-server"))
	}))
	defer backend.Close()

	env.insertVSCodeSession("s-vscode", env.userID(), true, "container-vscode", true)
	env.docker.setContainerPort("container-vscode", dockerx.VSCodePort, backendPort(t, backend))

	req, err := http.NewRequest(http.MethodGet, env.server.URL+"/api/sessions/s-vscode/vscode/foo", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer something")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("get through the proxy: %v", err)
	}
	defer resp.Body.Close()

	// The response still has to come back: stripping the headers must not break
	// the proxy itself.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "hello from code-server" {
		t.Errorf("body = %q", got)
	}
	if gotCookie != "" {
		t.Errorf("backend saw Cookie %q, want none: the session cookie reached the container", gotCookie)
	}
	if gotAuthorization != "" {
		t.Errorf("backend saw Authorization %q, want none", gotAuthorization)
	}
}

// The proxy is the one authenticated route a browser reaches by navigation, so
// its origin rule is by request shape. These are the shapes.
func TestVSCodeProxyEnforcesTheRequestsOrigin(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello from code-server"))
	}))
	defer backend.Close()

	env.insertVSCodeSession("s-vscode", env.userID(), true, "container-vscode", true)
	env.docker.setContainerPort("container-vscode", dockerx.VSCodePort, backendPort(t, backend))

	// The public URL newTestEnv configures, which is what the origin check
	// compares against — not the random port httptest bound.
	const ours = "http://127.0.0.1:8080"
	upgrade := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}
	with := func(base map[string]string, extra ...string) map[string]string {
		headers := map[string]string{}
		for k, v := range base {
			headers[k] = v
		}
		for i := 0; i < len(extra); i += 2 {
			headers[extra[i]] = extra[i+1]
		}
		return headers
	}

	for _, c := range []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"the button, a top-level navigation with no Origin", http.MethodGet,
			map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"an asset load from the editor's own page", http.MethodGet,
			map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"a client that sends neither header", http.MethodGet, nil, http.StatusOK},
		{"a navigation another site caused", http.MethodGet,
			map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"a page framing the editor", http.MethodGet,
			map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Dest": "iframe"}, http.StatusForbidden},
		{"an upgrade with no Origin", http.MethodGet, upgrade, http.StatusForbidden},
		{"an upgrade from another origin", http.MethodGet,
			with(upgrade, "Origin", "https://evil.example"), http.StatusForbidden},
		{"an upgrade from our own page", http.MethodGet,
			with(upgrade, "Origin", ours), http.StatusOK},
		{"a POST with no Origin", http.MethodPost, nil, http.StatusForbidden},
		{"a POST from our own page", http.MethodPost,
			map[string]string{"Origin": ours}, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := env.do(c.method, "/api/sessions/s-vscode/vscode/foo", c.headers).StatusCode; got != c.want {
				t.Errorf("status = %d, want %d", got, c.want)
			}
		})
	}
}
