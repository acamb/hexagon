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

	"os"
	"path/filepath"

	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/composex"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/store"
)

// fakeDocker records what the handlers ask of the daemon. For exec it hands
// back one end of a pipe, so a test can play the part of the container.
type fakeDocker struct {
	mu         sync.Mutex
	pingErr    error
	buildErr   error
	pullErr    error
	attachErr  error
	built      []string
	builtFrom  []string
	pulled     []string
	removed    []string
	inspected  []string
	inspectErr error
	// digest is what InspectImage returns for every ref, once set. Left empty,
	// InspectImage derives one from ref instead, which is enough for a test that
	// does not care about a specific value.
	digest string

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

	images          []dockerx.ImageSummary
	listImagesErr   error
	diskUsage       dockerx.DiskUsage
	diskUsageErr    error
	pruneContainers dockerx.Pruned
	pruneErr        error

	saveErr error
	loadErr error
	tagErr  error
	saved   []string
	// loaded is what LoadImage was asked to load, one entry per call, so a test
	// can tell a restore loaded the archive's own image.tar.gz rather than
	// something else.
	loaded []string
	tagged [][2]string
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
	f.builtFrom = append(f.builtFrom, dockerfile)
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

// InspectImage stands in for the daemon resolving a tag to content: it hands
// back a digest derived from ref, so a test can tell two builds under the same
// tag apart by rebuilding f.digest between them the way a real rebuild would
// change what the tag resolves to.
func (f *fakeDocker) InspectImage(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspected = append(f.inspected, ref)
	if f.inspectErr != nil {
		return "", f.inspectErr
	}
	if f.digest != "" {
		return f.digest, nil
	}
	return "digest:" + ref, nil
}

// SaveImage writes a small fake tar stream that names ref, which is enough for
// a test to tell one saved image from another without shelling out to a real
// daemon.
func (f *fakeDocker) SaveImage(_ context.Context, ref string, w io.Writer) error {
	f.mu.Lock()
	f.saved = append(f.saved, ref)
	err := f.saveErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	_, werr := io.WriteString(w, "fake-image-tar:"+ref)
	return werr
}

func (f *fakeDocker) LoadImage(_ context.Context, r io.Reader, logs io.Writer) error {
	content, _ := io.ReadAll(r)
	f.mu.Lock()
	f.loaded = append(f.loaded, string(content))
	err := f.loadErr
	f.mu.Unlock()
	if err != nil {
		fmt.Fprintf(logs, "load failed: %s\n", err)
		return err
	}
	fmt.Fprintf(logs, "Loaded image: %s\n", content)
	return nil
}

func (f *fakeDocker) TagImage(_ context.Context, source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagged = append(f.tagged, [2]string{source, target})
	return f.tagErr
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
	return e.sendJSON(http.MethodPost, path, body)
}

func (e *testEnv) sendJSON(method, path, body string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
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
	other, err := env.store.UpsertUser(ctx, &store.User{GitHubLogin: "bob", GitHubID: 7})
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

	var body struct {
		Dockerfile     string
		CanAsk         bool
		AskInContainer bool
	}
	env.decode(env.do(http.MethodGet, "/api/images/template", nil), &body)
	if !strings.HasPrefix(body.Dockerfile, "FROM scratch") {
		t.Errorf("template = %q, want the configured base Dockerfile", body.Dockerfile)
	}
	if !body.CanAsk || body.AskInContainer {
		t.Errorf("canAsk/askInContainer = %v/%v, want a server that runs the CLI directly",
			body.CanAsk, body.AskInContainer)
	}

	// A server that has to run the CLI in a container still offers the control,
	// and says which of the two it is: the container path is slower, and a page
	// that hid the difference would look merely sluggish.
	env.without(func(d *Deps) { d.EditorInContainer = true })
	env.decode(env.do(http.MethodGet, "/api/images/template", nil), &body)
	if !body.CanAsk || !body.AskInContainer {
		t.Errorf("canAsk/askInContainer = %v/%v, want the container path reported",
			body.CanAsk, body.AskInContainer)
	}
}

// builtDockerfiles returns the sources BuildImage was given, in order.
func (f *fakeDocker) builtDockerfiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.builtFrom...)
}

// fakeCompose stands in for the compose CLI. The real one is exercised in
// internal/composex against a fake binary; here what matters is that the
// handler refuses what it refuses.
type fakeCompose struct {
	mu sync.Mutex
	// docker is where Up registers the containers the project would have
	// created, so the rest of the server finds the agent container by name the
	// way it would with a real daemon.
	docker   *fakeDocker
	err      error
	services []string
	seen     []string
	calls    []string
}

func (f *fakeCompose) Validate(_ context.Context, content string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, content)
	if f.err != nil {
		return nil, f.err
	}
	if f.services == nil {
		return []string{"db"}, nil
	}
	return f.services, nil
}

// Up reads the file Hexagon generated and registers its agent container, which
// is also what says that file is well formed and names the container.
func (f *fakeCompose) Up(_ context.Context, p composex.Project) error {
	if err := f.record("up"); err != nil {
		return err
	}
	return f.registerAgent(p, true)
}

// Create is Up without starting anything, which is what the real `compose
// create` does: the agent container exists and is down.
func (f *fakeCompose) Create(_ context.Context, p composex.Project) error {
	if err := f.record("create"); err != nil {
		return err
	}
	return f.registerAgent(p, false)
}

func (f *fakeCompose) registerAgent(p composex.Project, running bool) error {
	name, err := f.agentName(p)
	if err != nil {
		return err
	}
	f.docker.addNamedContainer(name, running)
	return nil
}

// agentName reads the container name out of the file Hexagon generated, which
// is how the real compose learns it too.
func (f *fakeCompose) agentName(p composex.Project) (string, error) {
	overlay, err := os.ReadFile(filepath.Join(p.Dir, "hexagon.yaml"))
	if err != nil {
		return "", err
	}
	var file struct {
		Services map[string]struct {
			ContainerName string `json:"container_name"`
		} `json:"services"`
	}
	if err := json.Unmarshal(overlay, &file); err != nil {
		return "", err
	}
	return file.Services["hexagon"].ContainerName, nil
}

// Start and Stop move the agent container the way the real ones do. The state
// the daemon reports is what the server reconciles against, so a fake that only
// recorded the call would leave a stopped project looking like a running one.
func (f *fakeCompose) Start(_ context.Context, p composex.Project) error {
	return f.setAgentRunning(p, "start", true)
}

func (f *fakeCompose) Stop(_ context.Context, p composex.Project) error {
	return f.setAgentRunning(p, "stop", false)
}

func (f *fakeCompose) setAgentRunning(p composex.Project, call string, running bool) error {
	if err := f.record(call); err != nil {
		return err
	}
	name, err := f.agentName(p)
	if err != nil {
		return err
	}
	f.docker.setContainerRunning(name, running)
	return nil
}

func (f *fakeCompose) Down(_ context.Context, _ composex.Project, _ bool) error {
	return f.record("down")
}

func (f *fakeCompose) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakeCompose) validated() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen) > 0
}

// quote renders a string as a JSON literal, for a test that builds a request
// body around content it did not write itself.
func quote(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

// fakeEditor stands in for Claude Code. The real one is exercised in
// internal/claudex against a fake binary; here what matters is the handler.
type fakeEditor struct {
	mu          sync.Mutex
	err         error
	checkErr    error
	answer      string
	kind        string
	content     string
	instruction string
	checked     []claudex.Credential
	// edited is the credential each Edit was given, which is what says the
	// editor authenticates the way a session does.
	edited []claudex.Credential
}

func (f *fakeEditor) Edit(_ context.Context, cred claudex.Credential, kind, content, instruction string) (claudex.Edit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kind, f.content, f.instruction = kind, content, instruction
	f.edited = append(f.edited, cred)
	if f.err != nil {
		return claudex.Edit{}, f.err
	}
	if f.answer != "" {
		return claudex.Edit{Content: f.answer, Summary: "As asked"}, nil
	}
	return claudex.Edit{Content: content + "RUN apt-get install -y ripgrep\n", Summary: "Added ripgrep"}, nil
}

func (f *fakeEditor) Check(_ context.Context, cred claudex.Credential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, cred)
	return f.checkErr
}

func (f *fakeEditor) asked() (kind, content, instruction string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kind, f.content, f.instruction
}

func TestEditSourceReturnsWhatClaudeAnswered(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	resp := env.postJSON("/api/images/source",
		`{"kind":"dockerfile","content":"FROM busybox\n","instruction":"add ripgrep"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var edit claudex.Edit
	env.decode(resp, &edit)
	if !strings.Contains(edit.Content, "ripgrep") || !strings.HasPrefix(edit.Content, "FROM busybox") {
		t.Errorf("content = %q", edit.Content)
	}
	if edit.Summary != "Added ripgrep" {
		t.Errorf("summary = %q", edit.Summary)
	}

	kind, content, instruction := env.editor.asked()
	if kind != "dockerfile" || content != "FROM busybox\n" || instruction != "add ripgrep" {
		t.Errorf("asked to edit the %s %q with %q", kind, content, instruction)
	}
}

func TestEditSourceEditsAComposeFileToo(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	resp := env.postJSON("/api/images/source",
		`{"kind":"compose","content":"services: {}\n","instruction":"add postgres"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if kind, _, _ := env.editor.asked(); kind != "compose" {
		t.Errorf("kind = %q, want the compose file to have been asked for", kind)
	}
}

func TestEditSourceNeedsSomethingToDo(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	for _, body := range []string{
		`{"kind":"dockerfile","content":"FROM busybox\n","instruction":"   "}`,
		`{"kind":"dockerfile","content":"  ","instruction":"add ripgrep"}`,
		// A kind this server has no prompt for is a request it cannot answer.
		`{"kind":"readme","content":"hello","instruction":"add ripgrep"}`,
		`{"content":"FROM busybox\n","instruction":"add ripgrep"}`,
	} {
		if resp := env.postJSON("/api/images/source", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d for %s, want 400", resp.StatusCode, body)
		}
	}
	if _, _, instruction := env.editor.asked(); instruction != "" {
		t.Errorf("a refused request still reached Claude Code: %q", instruction)
	}
}

// The failure is in something this server called out to, and the message is
// what the user needs to see: not logged in, out of credit, and so on.
func TestEditSourceReportsAFailureAsABadGateway(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.editor.err = fmt.Errorf("claude code failed: credit balance too low")

	resp := env.postJSON("/api/images/source",
		`{"kind":"dockerfile","content":"FROM busybox\n","instruction":"add ripgrep"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var body map[string]string
	env.decode(resp, &body)
	if !strings.Contains(body["error"], "credit balance") {
		t.Errorf("error = %q, want the reason Claude Code gave", body["error"])
	}
}

func TestEditSourceNeedsAuthentication(t *testing.T) {
	env := newTestEnv(t, "alice")

	resp := env.postJSON("/api/images/source",
		`{"kind":"dockerfile","content":"FROM busybox\n","instruction":"add ripgrep"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Without a Claude Code binary the route answers 503 rather than pretending.
func TestEditSourceWithoutAnEditor(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.withoutEditor()
	env.signIn()

	resp := env.postJSON("/api/images/source",
		`{"kind":"dockerfile","content":"FROM busybox\n","instruction":"add ripgrep"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestTemplateSaysWhatThisServerCanDo(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var body struct {
		Dockerfile string `json:"dockerfile"`
		Compose    string `json:"compose"`
		CanAsk     bool   `json:"canAsk"`
		CanCompose bool   `json:"canCompose"`
	}
	env.decode(env.do(http.MethodGet, "/api/images/template", nil), &body)
	if body.Dockerfile == "" {
		t.Error("the Dockerfile template came back empty")
	}
	if body.Compose == "" {
		t.Error("the compose template came back empty")
	}
	if !body.CanAsk {
		t.Error("canAsk = false with an editor wired in")
	}
	if !body.CanCompose {
		t.Error("canCompose = false with docker compose wired in")
	}
}

// An advanced image is a Dockerfile and a compose file together: it builds like
// any other image, and the compose file is kept for session time.
func TestCreateComposeImageKeepsBothFiles(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	resp := env.postJSON("/api/images", `{"name":"advanced","sourceType":"compose",
		"dockerfile":"FROM busybox\n","compose":"services:\n  db:\n    image: postgres:16\n"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var img imageResponse
	env.decode(resp, &img)
	if img.SourceType != store.ImageSourceCompose {
		t.Errorf("sourceType = %q, want compose", img.SourceType)
	}
	if !strings.Contains(img.Compose, "postgres") {
		t.Errorf("compose = %q, want the file that was sent", img.Compose)
	}
	if !strings.Contains(img.Dockerfile, "busybox") {
		t.Errorf("dockerfile = %q, want the file that was sent", img.Dockerfile)
	}
	if !env.compose.validated() {
		t.Error("the compose file was stored without being validated")
	}

	// It builds its Dockerfile, exactly as a plain image does.
	env.waitForImageStatus(img.ID, store.ImageStatusReady)
	if built := env.docker.builtDockerfiles(); len(built) != 1 || !strings.Contains(built[0], "busybox") {
		t.Errorf("built = %v, want the image's Dockerfile", built)
	}
}

// Both halves are required: a compose image with no Dockerfile has no container
// to run Claude Code in, and one with no compose file is a plain image.
func TestCreateComposeImageNeedsBothFiles(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	for _, body := range []string{
		`{"name":"a","sourceType":"compose","compose":"services: {}"}`,
		`{"name":"b","sourceType":"compose","dockerfile":"FROM busybox\n"}`,
	} {
		if resp := env.postJSON("/api/images", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d for %s, want 400", resp.StatusCode, body)
		}
	}
}

// The refusals are the security boundary, and they run while the operator is
// still looking at the editor rather than at the first session that fails.
func TestCreateComposeImageRefusesAFileTheValidatorRejects(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.compose.err = fmt.Errorf(`service "db": privileged is not allowed here`)

	resp := env.postJSON("/api/images", `{"name":"advanced","sourceType":"compose",
		"dockerfile":"FROM busybox\n","compose":"services:\n  db:\n    privileged: true\n"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	env.decode(resp, &body)
	if !strings.Contains(body["error"], "privileged") {
		t.Errorf("error = %q, want it to name the refused key", body["error"])
	}
	// Nothing was stored, so there is no image to start a session from.
	var images []imageResponse
	env.decode(env.do(http.MethodGet, "/api/images", nil), &images)
	if len(images) != 0 {
		t.Errorf("images = %v, want the refused one not to have been saved", images)
	}
}

// A compose file Claude Code wrote is not trusted any more than one typed by
// hand: the prompt lists the rules, this is what enforces them.
func TestAComposeFileFromClaudeGoesThroughTheSameValidation(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.editor.answer = "services:\n  db:\n    privileged: true\n"

	var edit claudex.Edit
	env.decode(env.postJSON("/api/images/source",
		`{"kind":"compose","content":"services: {}","instruction":"give db everything"}`), &edit)

	env.compose.err = fmt.Errorf(`service "db": privileged is not allowed here`)
	resp := env.postJSON("/api/images", `{"name":"advanced","sourceType":"compose",
		"dockerfile":"FROM busybox\n","compose":`+quote(edit.Content)+`}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: an answer from Claude Code is still just a file", resp.StatusCode)
	}
}

// Without `docker compose` the server does not offer advanced images at all.
func TestCreateComposeImageWithoutCompose(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.withoutCompose()
	env.signIn()

	resp := env.postJSON("/api/images", `{"name":"advanced","sourceType":"compose",
		"dockerfile":"FROM busybox\n","compose":"services: {}"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}

	var body struct {
		CanCompose bool `json:"canCompose"`
	}
	env.decode(env.do(http.MethodGet, "/api/images/template", nil), &body)
	if body.CanCompose {
		t.Error("canCompose = true on a server with no docker compose")
	}
}

// A rebuild replaces the stored source and starts the build over, under the
// same tag it already had.
func TestRebuildImageStartsANewBuildFromEditedContent(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	created := env.readyImage("base")

	resp := env.postJSON("/api/images/"+created.ID+"/rebuild", `{"dockerfile":"FROM busybox\nRUN true"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("rebuild status = %d, want 202", resp.StatusCode)
	}
	var rebuilt imageResponse
	env.decode(resp, &rebuilt)
	if rebuilt.Status != store.ImageStatusBuilding {
		t.Errorf("status right after rebuild = %q, want building", rebuilt.Status)
	}
	if rebuilt.ImageRef != created.ImageRef {
		t.Errorf("image ref = %q, want the same tag as before: %q", rebuilt.ImageRef, created.ImageRef)
	}

	ready := env.waitForImageStatus(created.ID, store.ImageStatusReady)
	if !strings.Contains(ready.Dockerfile, "RUN true") {
		t.Errorf("dockerfile = %q, want the edited content", ready.Dockerfile)
	}

	built := env.docker.builtDockerfiles()
	if len(built) != 2 || !strings.Contains(built[1], "RUN true") {
		t.Errorf("built = %v, want a second build from the edited Dockerfile", built)
	}
}

// A registry image has no Dockerfile or compose file, so there is nothing to
// rebuild it from.
func TestRebuildImageRejectsARegistryImage(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var created imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"node","sourceType":"registry","registryRef":"node:22"}`), &created)
	env.waitForImageStatus(created.ID, store.ImageStatusReady)

	resp := env.postJSON("/api/images/"+created.ID+"/rebuild", `{"dockerfile":"FROM node:22"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// Two rebuilds cannot run at once: the second request has nothing to append
// its result to but a build already in flight.
func TestRebuildImageRejectsWhileAlreadyBuilding(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	created := env.readyImage("base")

	// Puts the row back into "building" without going through the handler,
	// standing in for a rebuild already in flight.
	if err := env.store.UpdateImageSource(context.Background(), env.userID(), created.ID, "FROM busybox", ""); err != nil {
		t.Fatalf("UpdateImageSource: %v", err)
	}

	resp := env.postJSON("/api/images/"+created.ID+"/rebuild", `{"dockerfile":"FROM busybox\nRUN true"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// A rebuilt compose image goes through the same refusals a new one does, and
// a refused edit is not persisted over the working content it replaces.
func TestRebuildImageValidatesComposeBeforePersisting(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var created imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"advanced","sourceType":"compose",
		"dockerfile":"FROM busybox\n","compose":"services:\n  db:\n    image: postgres:16\n"}`), &created)
	env.waitForImageStatus(created.ID, store.ImageStatusReady)

	env.compose.err = fmt.Errorf(`service "db": privileged is not allowed here`)
	resp := env.postJSON("/api/images/"+created.ID+"/rebuild",
		`{"dockerfile":"FROM busybox\n","compose":"services:\n  db:\n    privileged: true\n"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	env.decode(resp, &body)
	if !strings.Contains(body["error"], "privileged") {
		t.Errorf("error = %q, want it to name the refused key", body["error"])
	}

	var current imageResponse
	env.decode(env.do(http.MethodGet, "/api/images/"+created.ID, nil), &current)
	if strings.Contains(current.Compose, "privileged") {
		t.Errorf("compose = %q, want the refused edit not to have been stored", current.Compose)
	}
	if current.Status != store.ImageStatusReady {
		t.Errorf("status = %q, want the image to stay ready after a refused rebuild", current.Status)
	}
}
