package claudex

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
)

// fakeDocker records what the container runner asked of the daemon and answers
// with what a test told it to. Nothing here talks to Docker: what is being
// tested is the shape of the calls, which is where this path can go wrong.
type fakeDocker struct {
	mu sync.Mutex

	buildErr   error
	builds     []string
	buildTags  []string
	createErr  error
	specs      []dockerx.ContainerSpec
	execs      [][]string
	removed    []string
	execOutput string
	execCode   int
	execErr    error
	// stderr is what `cat` on the diagnostics file answers with.
	stderr string
}

func (f *fakeDocker) BuildImage(_ context.Context, dockerfile, tag string, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds = append(f.builds, dockerfile)
	f.buildTags = append(f.buildTags, tag)
	return f.buildErr
}

func (f *fakeDocker) CreateContainer(_ context.Context, spec dockerx.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.specs = append(f.specs, spec)
	return "container-1", nil
}

func (f *fakeDocker) StartContainer(context.Context, string) error { return nil }

func (f *fakeDocker) RunExec(_ context.Context, _ string, cmd []string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, cmd)
	if len(cmd) > 0 && cmd[0] == "cat" {
		return f.stderr, 0, nil
	}
	return f.execOutput, f.execCode, f.execErr
}

func (f *fakeDocker) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeDocker) calls() (specs []dockerx.ContainerSpec, execs [][]string, removed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dockerx.ContainerSpec(nil), f.specs...),
		append([][]string(nil), f.execs...),
		append([]string(nil), f.removed...)
}

// readyImage returns a default image whose build has finished, which is the
// state every call but the first one finds.
func readyImage(t *testing.T, docker Docker) *DefaultImage {
	t.Helper()
	image := NewDefaultImage(docker, "FROM busybox\n", slog.New(slog.DiscardHandler))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if image.State().Ready {
			return image
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the default image never became ready")
	return nil
}

// A build is minutes long, so asking for the image starts one and says it is not
// there yet. A request that waited for it would be a browser waiting on a socket
// for as long as an image takes.
func TestDefaultImageIsBuiltInTheBackgroundAndAskedForOnce(t *testing.T) {
	docker := &fakeDocker{}
	image := NewDefaultImage(docker, "FROM busybox\n", slog.New(slog.DiscardHandler))

	if state := image.State(); state.Ready || !state.Building {
		t.Errorf("state = %+v, want a build started and nothing usable yet", state)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !image.State().Ready {
		time.Sleep(5 * time.Millisecond)
	}
	state := image.State()
	if !state.Ready {
		t.Fatalf("state = %+v, want the image ready once the build returned", state)
	}
	if !strings.HasPrefix(state.Ref, "hexagon-default:") {
		t.Errorf("ref = %q, want a tag of Hexagon's own", state.Ref)
	}

	// Asking again builds nothing: the image is there.
	image.State()
	docker.mu.Lock()
	defer docker.mu.Unlock()
	if len(docker.builds) != 1 {
		t.Errorf("built %d times, want once", len(docker.builds))
	}
	if docker.builds[0] != "FROM busybox\n" {
		t.Errorf("built %q, want the reference Dockerfile it was given", docker.builds[0])
	}
}

// The tag carries a hash of the Dockerfile, so editing the reference file
// produces a different image instead of a stale one under the same name.
func TestDefaultImageTagFollowsTheDockerfile(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	one := NewDefaultImage(&fakeDocker{}, "FROM busybox\n", log).State().Ref
	same := NewDefaultImage(&fakeDocker{}, "FROM busybox\n", log).State().Ref
	other := NewDefaultImage(&fakeDocker{}, "FROM busybox\nRUN true\n", log).State().Ref

	if one != same {
		t.Errorf("the same Dockerfile produced %q and %q", one, same)
	}
	if one == other {
		t.Errorf("a different Dockerfile produced the same tag %q", one)
	}
}

func TestDefaultImageReportsWhyABuildFailed(t *testing.T) {
	docker := &fakeDocker{buildErr: errors.New("no space left on device")}
	image := NewDefaultImage(docker, "FROM busybox\n", slog.New(slog.DiscardHandler))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && image.State().Building {
		time.Sleep(5 * time.Millisecond)
	}
	state := image.State()
	if state.Ready {
		t.Fatal("the image is reported ready after a failed build")
	}
	if !strings.Contains(state.Error, "no space left") {
		t.Errorf("error = %q, want the daemon's own words", state.Error)
	}
}

// The prompt and the schema reach the CLI through the environment and never
// through the command line: one is a JSON document and the other is whatever a
// user typed, and a shell command built out of either is a quoting bug waiting
// for the input that triggers it.
func TestContainerPassesThePromptThroughTheEnvironment(t *testing.T) {
	docker := &fakeDocker{execOutput: `{"is_error":false,"result":"{\"dockerfile\":\"FROM alpine\",\"summary\":\"swapped the base\"}"}`}
	runner := NewContainer(docker, readyImage(t, docker), "1000:1000", "opus", slog.New(slog.DiscardHandler))

	edit, err := runner.Edit(context.Background(), Credential{Kind: KindAPIKey, Secret: "sk-test"},
		SourceDockerfile, "FROM debian", `use alpine, and mind the "quotes"`)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if edit.Content != "FROM alpine" || edit.Summary != "swapped the base" {
		t.Errorf("edit = %+v, want the answer the CLI gave", edit)
	}

	specs, execs, removed := docker.calls()
	if len(specs) != 1 || len(execs) == 0 {
		t.Fatalf("specs = %d, execs = %d, want one container and at least one exec", len(specs), len(execs))
	}
	env := strings.Join(specs[0].Env, "\n")
	if !strings.Contains(env, promptEnv+"=") || !strings.Contains(env, `mind the "quotes"`) {
		t.Errorf("the prompt did not reach the container's environment:\n%s", env)
	}
	if !strings.Contains(env, "ANTHROPIC_API_KEY=sk-test") {
		t.Errorf("the credential did not reach the container's environment:\n%s", env)
	}
	script := strings.Join(execs[0], " ")
	if strings.Contains(script, "mind the") {
		t.Errorf("the prompt was interpolated into the command:\n%s", script)
	}
	for _, want := range []string{"--safe-mode", "--strict-mcp-config", "--tools", "--output-format json",
		"--json-schema \"$" + schemaEnv + "\"", "--model \"$" + modelEnv + "\""} {
		if !strings.Contains(script, want) {
			t.Errorf("%q is missing from the command:\n%s", want, script)
		}
	}
	// AGENTS.md: nothing Hexagon starts runs as root.
	if specs[0].User != "1000:1000" {
		t.Errorf("user = %q, want the same host user sessions run as", specs[0].User)
	}
	if len(removed) != 1 || removed[0] != "container-1" {
		t.Errorf("removed = %v, want the container gone once the call returned", removed)
	}
}

// The daemon folds an exec's two streams into one, so the diagnostics go to a
// file and are read back only when the call failed. Without that, a warning
// printed beside a good answer would make it unparseable.
func TestContainerReadsTheDiagnosticsOfAFailedCall(t *testing.T) {
	docker := &fakeDocker{execCode: 1, stderr: "Invalid API key\nfollowed by a stack trace"}
	runner := NewContainer(docker, readyImage(t, docker), "1000:1000", "", slog.New(slog.DiscardHandler))

	err := runner.Check(context.Background(), Credential{Kind: KindAPIKey, Secret: "nope"})
	if err == nil {
		t.Fatal("Check succeeded on an exit code of 1")
	}
	if !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("error = %q, want the CLI's own first line", err)
	}

	_, execs, removed := docker.calls()
	if len(execs) != 2 || execs[1][0] != "cat" {
		t.Errorf("execs = %v, want the diagnostics file read after the failure", execs)
	}
	if len(removed) != 1 {
		t.Errorf("removed = %v, want the container removed even after a failure", removed)
	}
}

// The CLI reports a refused credential in its exit status and in an answer that
// says which of several things went wrong. The answer is the half a user can act
// on, so it wins: "Not logged in" beats "exit status 1".
func TestContainerPrefersTheCLIsOwnWordsOverItsExitStatus(t *testing.T) {
	docker := &fakeDocker{
		execCode:   1,
		execOutput: `{"is_error":true,"result":"Not logged in · Please run /login"}`,
		stderr:     "",
	}
	runner := NewContainer(docker, readyImage(t, docker), "1000:1000", "", slog.New(slog.DiscardHandler))

	err := runner.Check(context.Background(), Credential{})
	if err == nil {
		t.Fatal("Check succeeded on an answer that reported an error")
	}
	if !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("error = %q, want the CLI's own explanation", err)
	}
	if _, execs, _ := docker.calls(); len(execs) != 1 {
		t.Errorf("execs = %v, want no diagnostics read: the answer already said it", execs)
	}
}

// A call that arrives before the image is built is told to ask again, which is
// something a user can act on. Blocking would hold the request open for as long
// as a build takes.
func TestContainerSaysWhenTheImageIsNotReadyYet(t *testing.T) {
	docker := &fakeDocker{}
	// A build that never returns, so the state stays "building".
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	slow := &slowDocker{fakeDocker: docker, block: blocked}
	image := NewDefaultImage(slow, "FROM busybox\n", slog.New(slog.DiscardHandler))
	runner := NewContainer(slow, image, "1000:1000", "", slog.New(slog.DiscardHandler))

	err := runner.Check(context.Background(), Credential{})
	if err == nil {
		t.Fatal("Check succeeded with no image to run in")
	}
	if !strings.Contains(err.Error(), "still being built") {
		t.Errorf("error = %q, want it to say the image is on its way", err)
	}
	if specs, _, _ := docker.calls(); len(specs) != 0 {
		t.Errorf("created %d containers with no image to create them from", len(specs))
	}
}

// slowDocker is a fakeDocker whose build does not return until the test says so.
type slowDocker struct {
	*fakeDocker
	block chan struct{}
}

func (s *slowDocker) BuildImage(ctx context.Context, dockerfile, tag string, logs io.Writer) error {
	<-s.block
	return s.fakeDocker.BuildImage(ctx, dockerfile, tag, logs)
}
