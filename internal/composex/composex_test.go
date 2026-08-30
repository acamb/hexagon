package composex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDocker writes a script that stands in for the CLI: it records the
// arguments, the environment and the stdin it was given, then prints body and
// exits with code. The binary being a path is what makes this possible, so none
// of these tests need a Docker daemon.
func fakeDocker(t *testing.T, body string, code int) (*Runner, recordedFiles) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do printf '%%s\n' "$arg" >> %[1]s/args; done
cat >> %[1]s/stdin 2>/dev/null
env > %[1]s/env
pwd > %[1]s/pwd
cat <<'BODY'
%[2]s
BODY
exit %[3]d
`, dir, body, code)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return New(path, ""), recordedFiles{dir: dir}
}

type recordedFiles struct{ dir string }

func (r recordedFiles) read(t *testing.T, name string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(r.dir, name))
	return string(data)
}

// A two-service file, normalized the way `docker compose config --format json`
// hands it back.
const twoServices = `{"services":{
	"cache":{"image":"redis:7"},
	"db":{"image":"postgres:16","volumes":[{"type":"volume","source":"data","target":"/var/lib/postgresql/data"}],
	      "ports":[{"mode":"ingress","target":5432,"protocol":"tcp"}]}
}}`

func TestValidateSendsTheFileAndReturnsItsServices(t *testing.T) {
	runner, recorded := fakeDocker(t, twoServices, 0)

	services, err := runner.Validate(context.Background(), "services:\n  db:\n    image: postgres:16\n")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Sorted, so a caller can rely on the order and a file with two problems is
	// always refused for the same one.
	if len(services) != 2 || services[0] != "cache" || services[1] != "db" {
		t.Errorf("services = %v, want [cache db]", services)
	}

	args := recorded.read(t, "args")
	for _, want := range []string{"compose", "--file", "-", "config", "--format", "json"} {
		if !strings.Contains(args, want+"\n") {
			t.Errorf("%q is missing from the arguments:\n%s", want, args)
		}
	}
	if stdin := recorded.read(t, "stdin"); !strings.Contains(stdin, "postgres:16") {
		t.Errorf("the file did not reach the CLI's stdin:\n%s", stdin)
	}
}

func TestProjectCommandsCarryBothFilesAndTheProjectName(t *testing.T) {
	dir := t.TempDir()
	project := Project{
		Name:  "hexagon-s1",
		Dir:   dir,
		Files: []string{filepath.Join(dir, "user.yaml"), filepath.Join(dir, "hexagon.yaml")},
	}

	cases := []struct {
		name string
		call func(*Runner) error
		want []string
	}{
		{"up", func(r *Runner) error { return r.Up(context.Background(), project) }, []string{"up", "--detach"}},
		{"start", func(r *Runner) error { return r.Start(context.Background(), project) }, []string{"start"}},
		{"stop", func(r *Runner) error { return r.Stop(context.Background(), project) }, []string{"stop"}},
		{"down", func(r *Runner) error { return r.Down(context.Background(), project, false) }, []string{"down"}},
		{"down with volumes", func(r *Runner) error { return r.Down(context.Background(), project, true) }, []string{"down", "--volumes"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runner, recorded := fakeDocker(t, "", 0)
			if err := c.call(runner); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			args := recorded.read(t, "args")
			want := append([]string{"compose", "--project-name", "hexagon-s1"}, c.want...)
			for _, w := range append(want, project.Files...) {
				if !strings.Contains(args, w+"\n") {
					t.Errorf("%q is missing from the arguments:\n%s", w, args)
				}
			}
			// The project directory is what relative paths in the files resolve
			// against, so the command has to run there.
			if got := strings.TrimSpace(recorded.read(t, "pwd")); got != dir {
				t.Errorf("ran in %q, want the project directory %q", got, dir)
			}
		})
	}
}

// The CLI has to talk to the daemon internal/dockerx talks to, not to whichever
// one the server process happened to inherit.
func TestDockerHostIsPassedThrough(t *testing.T) {
	runner, recorded := fakeDocker(t, twoServices, 0)
	runner.host = "tcp://192.0.2.10:2375"

	if _, err := runner.Validate(context.Background(), "services: {}"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if env := recorded.read(t, "env"); !strings.Contains(env, "DOCKER_HOST=tcp://192.0.2.10:2375\n") {
		t.Errorf("DOCKER_HOST did not reach the CLI:\n%s", env)
	}
}

func TestANonZeroExitCarriesWhatTheCLISaid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	script := "#!/bin/sh\necho 'services.db.image: required' >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	_, err := New(path, "").Validate(context.Background(), "services:\n  db: {}\n")
	if err == nil {
		t.Fatal("Validate accepted a file the CLI refused")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %q, want it to carry what the CLI said", err)
	}
}

func TestAvailableIsFalseWithNoBinary(t *testing.T) {
	if New(filepath.Join(t.TempDir(), "missing"), "").Available(context.Background()) {
		t.Error("Available said yes for a binary that does not exist")
	}
	runner, _ := fakeDocker(t, "Docker Compose version v2.30.0", 0)
	if !runner.Available(context.Background()) {
		t.Error("Available said no for a working binary")
	}
}
