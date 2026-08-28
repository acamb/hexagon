package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/store"
	"github.com/coder/websocket"
)

const testOrigin = "http://127.0.0.1:8080" // matches the test config's PublicURL

// insertSession adds a session row directly. Creating sessions properly is the
// next milestone's job; the terminal only needs one to exist.
func (e *testEnv) insertSession(id, status, containerID string) string {
	e.t.Helper()

	user, err := e.store.UserByGitHubID(context.Background(), 42)
	if err != nil {
		e.t.Fatalf("look up user: %v", err)
	}
	// sessions.image_id is a foreign key, so the image has to exist first.
	_, err = e.store.DB().Exec(`
		INSERT OR IGNORE INTO images (id, user_id, name, source_type, dockerfile, registry_ref,
			image_ref, status, build_log, error, created_at)
		VALUES ('img-1', ?, 'base', 'dockerfile', 'FROM busybox', '', 'hexagon/img-1:latest', 'ready', '', '',
			'2026-01-01 00:00:00.000')`, user.ID)
	if err != nil {
		e.t.Fatalf("insert image: %v", err)
	}

	_, err = e.store.DB().Exec(`
		INSERT INTO sessions (id, user_id, title, repo_full_name, repo_clone_url, branch, image_id,
			image_ref, workspace_dir, repo_dir, container_id, status, error, created_at, updated_at)
		VALUES (?, ?, 'demo', 'acme/widgets', 'https://github.com/acme/widgets.git', 'main', 'img-1',
			'hexagon/img-1:latest', '/w', '/w/repo', ?, ?, '', '2026-01-01 00:00:00.000', '2026-01-01 00:00:00.000')`,
		id, user.ID, containerID, status)
	if err != nil {
		e.t.Fatalf("insert session: %v", err)
	}
	return id
}

// dialTerminal opens the terminal WebSocket with the session cookie the client
// already holds.
func (e *testEnv) dialTerminal(sessionID, query, origin string) (*websocket.Conn, *http.Response, error) {
	e.t.Helper()

	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	target := strings.Replace(e.server.URL, "http://", "ws://", 1) + "/api/sessions/" + sessionID + "/terminal" + query
	return websocket.Dial(context.Background(), target, &websocket.DialOptions{
		HTTPClient: e.client,
		HTTPHeader: header,
	})
}

func TestTerminalStreamsBothWays(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertSession("s1", store.SessionStatusRunning, "container-1")

	conn, _, err := env.dialTerminal("s1", "?cols=120&rows=40", testOrigin)
	if err != nil {
		t.Fatalf("dial terminal: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	container := env.docker.containerSide(t)

	// The exec must attach to tmux, with the geometry the browser reported.
	requests, _ := env.docker.execs()
	if len(requests) != 1 {
		t.Fatalf("made %d exec requests, want 1", len(requests))
	}
	req := requests[0]
	if req.ContainerID != "container-1" {
		t.Errorf("attached to container %q", req.ContainerID)
	}
	if got := strings.Join(req.Cmd, " "); got != "tmux new-session -A -D -s main -c /workspace" {
		t.Errorf("exec command = %q", got)
	}
	if req.Size.Cols != 120 || req.Size.Rows != 40 {
		t.Errorf("initial size = %dx%d, want 120x40", req.Size.Cols, req.Size.Rows)
	}

	// Container output reaches the browser.
	go func() {
		container.SetWriteDeadline(time.Now().Add(3 * time.Second))
		container.Write([]byte("\x1b[32mhello\x1b[0m"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	kind, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read from terminal: %v", err)
	}
	if kind != websocket.MessageBinary {
		t.Errorf("output arrived as %v, want a binary frame", kind)
	}
	if string(data) != "\x1b[32mhello\x1b[0m" {
		t.Errorf("output = %q", data)
	}

	// Keystrokes reach the container.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("ls -la\r")); err != nil {
		t.Fatalf("write to terminal: %v", err)
	}
	buf := make([]byte, 64)
	container.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := container.Read(buf)
	if err != nil {
		t.Fatalf("read keystrokes: %v", err)
	}
	if string(buf[:n]) != "ls -la\r" {
		t.Errorf("keystrokes = %q", buf[:n])
	}
}

func TestTerminalResizeControlMessage(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertSession("s1", store.SessionStatusRunning, "container-1")

	conn, _, err := env.dialTerminal("s1", "", testOrigin)
	if err != nil {
		t.Fatalf("dial terminal: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	env.docker.containerSide(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":132,"rows":50}`)); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	// A malformed message must not take the terminal down with it.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`not json`)); err != nil {
		t.Fatalf("send garbage: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, resizes := env.docker.execs(); len(resizes) == 1 {
			if resizes[0].Cols != 132 || resizes[0].Rows != 50 {
				t.Errorf("resize = %dx%d, want 132x50", resizes[0].Cols, resizes[0].Rows)
			}
			// Still alive after the garbage message.
			if err := conn.Write(ctx, websocket.MessageBinary, []byte("x")); err != nil {
				t.Errorf("the terminal died on a malformed control message: %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the resize never reached the daemon")
}

// Closing the browser tab must detach, not kill: tmux keeps Claude Code running
// so a reload finds the session where it was left.
func TestClosingTheTerminalDetaches(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertSession("s1", store.SessionStatusRunning, "container-1")

	conn, _, err := env.dialTerminal("s1", "", testOrigin)
	if err != nil {
		t.Fatalf("dial terminal: %v", err)
	}
	container := env.docker.containerSide(t)
	conn.Close(websocket.StatusNormalClosure, "")

	// The exec's streams are released, which is what detaching looks like from
	// the container's side.
	container.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 8)
	if _, err := container.Read(buf); err == nil {
		t.Error("the container streams stayed open after the browser left")
	}
}

func TestTerminalRefusesSessionsThatAreNotRunning(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertSession("stopped", store.SessionStatusStopped, "container-1")
	env.insertSession("creating", store.SessionStatusCreating, "")

	for _, id := range []string{"stopped", "creating"} {
		_, resp, err := env.dialTerminal(id, "", testOrigin)
		if err == nil {
			t.Fatalf("%s: the handshake succeeded, want it refused", id)
		}
		if resp == nil || resp.StatusCode != http.StatusConflict {
			t.Errorf("%s: status = %v, want 409", id, statusOf(resp))
		}
	}
	if requests, _ := env.docker.execs(); len(requests) != 0 {
		t.Errorf("a refused session still attached: %v", requests)
	}
}

func TestTerminalRequiresAMatchingOrigin(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertSession("s1", store.SessionStatusRunning, "container-1")

	cases := map[string]string{
		"a foreign origin": "http://evil.example",
		"no origin at all": "",
	}
	for name, origin := range cases {
		t.Run(name, func(t *testing.T) {
			_, resp, err := env.dialTerminal("s1", "", origin)
			if err == nil {
				t.Fatal("the handshake succeeded, want it refused")
			}
			if resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %v, want 403", statusOf(resp))
			}
		})
	}
}

func TestTerminalRequiresASessionOfYourOwn(t *testing.T) {
	env := newTestEnv(t, "alice")

	// Not signed in at all.
	_, resp, err := env.dialTerminal("s1", "", testOrigin)
	if err == nil {
		t.Fatal("an unauthenticated handshake succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %v, want 401", statusOf(resp))
	}

	// Signed in, but the session belongs to somebody else.
	env.signIn()
	ctx := context.Background()
	other, err := env.store.UpsertUser(ctx, &store.User{GitHubLogin: "bob", GitHubID: 7, GitHubTokenEnc: []byte("x")})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	_, err = env.store.DB().Exec(`
		INSERT INTO images (id, user_id, name, source_type, dockerfile, registry_ref, image_ref, status,
			build_log, error, created_at)
		VALUES ('img-2', ?, 'base', 'dockerfile', 'FROM busybox', '', 'ref', 'ready', '', '',
			'2026-01-01 00:00:00.000')`, other.ID)
	if err != nil {
		t.Fatalf("insert image: %v", err)
	}
	_, err = env.store.DB().Exec(`
		INSERT INTO sessions (id, user_id, title, repo_full_name, repo_clone_url, branch, image_id,
			image_ref, workspace_dir, repo_dir, container_id, status, error, created_at, updated_at)
		VALUES ('theirs', ?, 'demo', 'o/r', 'https://example.test/o/r.git', 'main', 'img-2', 'ref', '/w', '/w/repo',
			'container-2', 'running', '', '2026-01-01 00:00:00.000', '2026-01-01 00:00:00.000')`, other.ID)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	_, resp, err = env.dialTerminal("theirs", "", testOrigin)
	if err == nil {
		t.Fatal("attached to another user's session")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %v, want 404", statusOf(resp))
	}
}

func TestTerminalReportsAFailedAttach(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertSession("s1", store.SessionStatusRunning, "container-1")
	env.docker.attachErr = fmt.Errorf("no such container")

	conn, _, err := env.dialTerminal("s1", "", testOrigin)
	if err != nil {
		t.Fatalf("dial terminal: %v", err)
	}
	defer conn.Close(websocket.StatusInternalError, "")

	// The handshake succeeds and the failure arrives as a close frame, because
	// by then the response has already been upgraded.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("the socket stayed open after the attach failed")
	} else if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Errorf("close status = %v, want an internal error", websocket.CloseStatus(err))
	}
}

func statusOf(resp *http.Response) any {
	if resp == nil {
		return "no response"
	}
	return resp.StatusCode
}

func TestGetSession(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.insertSession("s1", store.SessionStatusRunning, "container-1")

	var session sessionResponse
	env.decode(env.do(http.MethodGet, "/api/sessions/s1", nil), &session)
	if session.ID != "s1" || session.RepoFullName != "acme/widgets" || session.Status != store.SessionStatusRunning {
		t.Errorf("session = %+v", session)
	}

	if got := env.do(http.MethodGet, "/api/sessions/nope", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("unknown session = %d, want 404", got)
	}
}
