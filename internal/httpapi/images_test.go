package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/store"
)

// fakeDocker records what the handlers ask of the daemon. For exec it hands
// back one end of a pipe, so a test can play the part of the container.
type fakeDocker struct {
	mu        sync.Mutex
	pingErr   error
	buildErr  error
	pullErr   error
	attachErr error
	built     []string
	pulled    []string
	removed   []string

	createErr  error
	startErr   error
	runExecErr error

	containers        map[string]*fakeContainer
	nextContainer     int
	removedContainers []string
	ranExecs          [][]string
	runExecOutput     string
	runExecCode       int

	execRequests []dockerx.ExecRequest
	resizes      []dockerx.TerminalSize
	// container is the far end of the attached exec: writing to it is output
	// from the container, reading from it is what the user typed.
	container net.Conn
}

func newFakeDocker() *fakeDocker { return &fakeDocker{containers: map[string]*fakeContainer{}} }

func (f *fakeDocker) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingErr
}

func (f *fakeDocker) BuildImage(_ context.Context, dockerfile, tag string, logs io.Writer) error {
	f.mu.Lock()
	f.built = append(f.built, tag)
	err := f.buildErr
	f.mu.Unlock()

	fmt.Fprintf(logs, "Step 1/1 : %s\n", strings.SplitN(dockerfile, "\n", 2)[0])
	return err
}

func (f *fakeDocker) PullImage(_ context.Context, ref string, logs io.Writer) error {
	f.mu.Lock()
	f.pulled = append(f.pulled, ref)
	err := f.pullErr
	f.mu.Unlock()

	fmt.Fprintf(logs, "Pulling %s\n", ref)
	return err
}

func (f *fakeDocker) RemoveImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, ref)
	return nil
}

func (f *fakeDocker) AttachExec(_ context.Context, req dockerx.ExecRequest) (*dockerx.Exec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	f.execRequests = append(f.execRequests, req)

	local, remote := net.Pipe()
	f.container = remote
	return &dockerx.Exec{
		ID:     fmt.Sprintf("exec-%d", len(f.execRequests)),
		Conn:   local,
		Output: local,
		Closer: local,
	}, nil
}

func (f *fakeDocker) ResizeExec(_ context.Context, _ string, size dockerx.TerminalSize) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, size)
	return nil
}

// containerSide waits for the handler to attach and returns the pipe end that
// stands in for the container.
func (f *fakeDocker) containerSide(t *testing.T) net.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		conn := f.container
		f.mu.Unlock()
		if conn != nil {
			return conn
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the handler never attached to the container")
	return nil
}

func (f *fakeDocker) execs() ([]dockerx.ExecRequest, []dockerx.TerminalSize) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dockerx.ExecRequest(nil), f.execRequests...), append([]dockerx.TerminalSize(nil), f.resizes...)
}

func (f *fakeDocker) calls() (built, pulled, removed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.built...), append([]string(nil), f.pulled...), append([]string(nil), f.removed...)
}

// postJSON sends a request with a body, which testEnv.do does not do.
func (e *testEnv) postJSON(path, body string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("POST %s: %v", path, err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *testEnv) decode(resp *http.Response, out any) {
	e.t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		e.t.Fatalf("decode response: %v", err)
	}
}

// waitForImageStatus polls until the build goroutine has recorded its outcome.
func (e *testEnv) waitForImageStatus(id, want string) imageResponse {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var img imageResponse
	for time.Now().Before(deadline) {
		e.decode(e.do(http.MethodGet, "/api/images/"+id, nil), &img)
		if img.Status == want {
			return img
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("image %s stayed in status %q, want %q", id, img.Status, want)
	return img
}

func TestCreateImageFromDockerfile(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	resp := env.postJSON("/api/images", `{"name":"base","sourceType":"dockerfile","dockerfile":"FROM node:22-bookworm-slim\nRUN true"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	var created imageResponse
	env.decode(resp, &created)
	if created.Status != store.ImageStatusBuilding {
		t.Errorf("initial status = %q, want building", created.Status)
	}
	if !strings.HasPrefix(created.ImageRef, "hexagon/img-") {
		t.Errorf("image ref = %q, want a hexagon/img- tag", created.ImageRef)
	}

	ready := env.waitForImageStatus(created.ID, store.ImageStatusReady)
	if ready.Error != "" {
		t.Errorf("ready image carries an error: %q", ready.Error)
	}

	built, _, _ := env.docker.calls()
	if len(built) != 1 || built[0] != created.ImageRef {
		t.Errorf("built = %v, want [%s]", built, created.ImageRef)
	}

	var logBody struct{ Log, Status string }
	env.decode(env.do(http.MethodGet, "/api/images/"+created.ID+"/log", nil), &logBody)
	if !strings.Contains(logBody.Log, "FROM node:22-bookworm-slim") {
		t.Errorf("build log = %q, want the daemon output", logBody.Log)
	}
}

// A registry reference is normalized, so "node:22" and its fully qualified form
// name the same image.
func TestCreateImageFromRegistryNormalizesTheReference(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	resp := env.postJSON("/api/images", `{"name":"node","sourceType":"registry","registryRef":"node:22"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	var created imageResponse
	env.decode(resp, &created)
	if want := "docker.io/library/node:22"; created.ImageRef != want {
		t.Errorf("image ref = %q, want %q", created.ImageRef, want)
	}
	if created.RegistryRef != "node:22" {
		t.Errorf("registry ref = %q, want what the user typed", created.RegistryRef)
	}

	env.waitForImageStatus(created.ID, store.ImageStatusReady)
	_, pulled, _ := env.docker.calls()
	if len(pulled) != 1 || pulled[0] != created.ImageRef {
		t.Errorf("pulled = %v, want [%s]", pulled, created.ImageRef)
	}
}

func TestCreateImageValidation(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	cases := map[string]string{
		"empty name":         `{"name":"","sourceType":"dockerfile","dockerfile":"FROM x"}`,
		"name with slash":    `{"name":"a/b","sourceType":"dockerfile","dockerfile":"FROM x"}`,
		"unknown source":     `{"name":"ok","sourceType":"magic"}`,
		"missing dockerfile": `{"name":"ok","sourceType":"dockerfile","dockerfile":"   "}`,
		"missing ref":        `{"name":"ok","sourceType":"registry","registryRef":""}`,
		"invalid ref":        `{"name":"ok","sourceType":"registry","registryRef":"NOT A REF"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := env.postJSON("/api/images", body).StatusCode; got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", got)
			}
		})
	}

	var images []imageResponse
	env.decode(env.do(http.MethodGet, "/api/images", nil), &images)
	if len(images) != 0 {
		t.Errorf("rejected requests created %d images", len(images))
	}
}

func TestCreateImageRejectsDuplicateNames(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	body := `{"name":"base","sourceType":"dockerfile","dockerfile":"FROM x"}`
	first := env.postJSON("/api/images", body)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first create = %d, want 202", first.StatusCode)
	}
	var created imageResponse
	env.decode(first, &created)
	env.waitForImageStatus(created.ID, store.ImageStatusReady)

	if got := env.postJSON("/api/images", body).StatusCode; got != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", got)
	}
}

// Without a daemon the build would never happen, so the request fails instead
// of leaving a row that hangs in "building".
func TestCreateImageRequiresAReachableDaemon(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.docker.pingErr = fmt.Errorf("connection refused")

	resp := env.postJSON("/api/images", `{"name":"base","sourceType":"dockerfile","dockerfile":"FROM x"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}

	var images []imageResponse
	env.decode(env.do(http.MethodGet, "/api/images", nil), &images)
	if len(images) != 0 {
		t.Errorf("a failed precondition created %d images", len(images))
	}
}

func TestFailedBuildIsRecorded(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.docker.buildErr = fmt.Errorf("The command '/bin/sh -c false' returned a non-zero code: 1")

	var created imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"broken","sourceType":"dockerfile","dockerfile":"FROM x\nRUN false"}`), &created)

	failed := env.waitForImageStatus(created.ID, store.ImageStatusFailed)
	if !strings.Contains(failed.Error, "non-zero code") {
		t.Errorf("error = %q, want the daemon message", failed.Error)
	}

	var logBody struct{ Log string }
	env.decode(env.do(http.MethodGet, "/api/images/"+created.ID+"/log", nil), &logBody)
	if !strings.Contains(logBody.Log, "non-zero code") {
		t.Errorf("the failure is missing from the build log: %q", logBody.Log)
	}
}

func TestDeleteImage(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var built imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"built","sourceType":"dockerfile","dockerfile":"FROM x"}`), &built)
	env.waitForImageStatus(built.ID, store.ImageStatusReady)

	var pulled imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"pulled","sourceType":"registry","registryRef":"node:22"}`), &pulled)
	env.waitForImageStatus(pulled.ID, store.ImageStatusReady)

	if got := env.do(http.MethodDelete, "/api/images/"+built.ID, map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusNoContent {
		t.Fatalf("delete built image = %d, want 204", got)
	}
	if got := env.do(http.MethodDelete, "/api/images/"+pulled.ID, map[string]string{"Content-Type": "application/json"}).StatusCode; got != http.StatusNoContent {
		t.Fatalf("delete pulled image = %d, want 204", got)
	}

	// Only the tag Hexagon created is ours to remove; a pulled image may be in
	// use by something else on the machine.
	_, _, removed := env.docker.calls()
	if len(removed) != 1 || removed[0] != built.ImageRef {
		t.Errorf("removed = %v, want only %s", removed, built.ImageRef)
	}

	var images []imageResponse
	env.decode(env.do(http.MethodGet, "/api/images", nil), &images)
	if len(images) != 0 {
		t.Errorf("%d images left after deleting both", len(images))
	}
}

func TestDeleteImageInUseByASession(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var img imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"base","sourceType":"dockerfile","dockerfile":"FROM x"}`), &img)
	env.waitForImageStatus(img.ID, store.ImageStatusReady)

	user, err := env.store.UserByGitHubID(context.Background(), 42)
	if err != nil {
		t.Fatalf("look up user: %v", err)
	}
	_, err = env.store.DB().Exec(`
		INSERT INTO sessions (id, user_id, title, repo_full_name, repo_clone_url, branch, image_id,
			image_ref, workspace_dir, repo_dir, container_id, status, error, created_at, updated_at)
		VALUES ('s1', ?, 'demo', 'o/r', 'https://example.test/o/r.git', 'main', ?, ?, '/w', '/w/repo', '', 'running', '', '2026-01-01 00:00:00.000', '2026-01-01 00:00:00.000')`,
		user.ID, img.ID, img.ImageRef)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	resp := env.do(http.MethodDelete, "/api/images/"+img.ID, map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete an image in use = %d, want 409", resp.StatusCode)
	}
	if _, _, removed := env.docker.calls(); len(removed) != 0 {
		t.Errorf("a refused delete still removed %v", removed)
	}
}

func TestImagesAreScopedToTheirOwner(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var mine imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"mine","sourceType":"dockerfile","dockerfile":"FROM x"}`), &mine)

	ctx := context.Background()
	other, err := env.store.UpsertUser(ctx, &store.User{GitHubLogin: "bob", GitHubID: 7, GitHubTokenEnc: []byte("x")})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	if _, err := env.store.CreateImage(ctx, &store.Image{
		UserID: other.ID, Name: "theirs", SourceType: store.ImageSourceRegistry,
		RegistryRef: "node:22", ImageRef: "docker.io/library/node:22", Status: store.ImageStatusReady,
	}); err != nil {
		t.Fatalf("create other image: %v", err)
	}

	var images []imageResponse
	env.decode(env.do(http.MethodGet, "/api/images", nil), &images)
	if len(images) != 1 || images[0].ID != mine.ID {
		t.Errorf("listed %d images, want only the caller's", len(images))
	}
}

func TestImageEndpointsRequireASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	for _, path := range []string{"/api/images", "/api/images/template", "/api/images/anything"} {
		if got := env.do(http.MethodGet, path, nil).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, got)
		}
	}
	if got := env.postJSON("/api/images", `{"name":"x","sourceType":"dockerfile","dockerfile":"FROM x"}`).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("POST /api/images without a session = %d, want 401", got)
	}
}

func TestImageTemplateIsTheBaseDockerfile(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var body struct{ Dockerfile string }
	env.decode(env.do(http.MethodGet, "/api/images/template", nil), &body)
	if !strings.HasPrefix(body.Dockerfile, "FROM scratch") {
		t.Errorf("template = %q, want the configured base Dockerfile", body.Dockerfile)
	}
}
