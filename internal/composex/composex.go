// Package composex wraps the Docker Compose CLI, for sessions whose image
// carries a compose file describing the services that have to run beside the
// agent container.
//
// It shells out rather than talking to the Engine API, because compose is a
// client-side format: networks, depends_on, healthchecks, profiles, extends and
// interpolation are all resolved before the daemon sees anything, and the
// Engine API has no compose in it at all. Reimplementing that is a project
// rather than a feature, and internal/dockerx is explicitly a wrapper with no
// business logic in it, so it is not where such a thing would go either.
//
// Like internal/gitops and internal/claudex, this package holds no business
// logic of its own beyond the refusals in Validate, which are a security
// boundary and belong next to the parser that makes them enforceable.
package composex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrUnavailable reports that this machine has no `docker compose`, so advanced
// images cannot be offered at all.
var ErrUnavailable = errors.New("docker compose is not available")

// composeValidateProjectName satisfies `docker compose config`'s requirement
// for a project name, even though Validate's content never corresponds to a
// running project — it arrives over stdin, sometimes before any image or
// session exists, so there is no directory to infer one from. It cannot
// collide with a real project's name: those are "hexagon-" followed by a
// session id (see internal/session/compose.go), never this literal word.
const composeValidateProjectName = "hexagon-validate"

// Runner invokes the CLI.
type Runner struct {
	binary string
	// host is the Docker endpoint, passed through as DOCKER_HOST so the CLI
	// talks to the daemon internal/dockerx talks to rather than to whichever one
	// the server process happened to inherit.
	host string
}

// New builds a runner over the docker binary. An empty binary means `docker` on
// the PATH; an empty host leaves DOCKER_HOST as the process has it.
func New(binary, host string) *Runner {
	if binary == "" {
		binary = "docker"
	}
	return &Runner{binary: binary, host: host}
}

// Project is one compose project: the name it runs under, the directory
// relative paths in its files resolve against, and the files themselves.
type Project struct {
	Name  string
	Dir   string
	Files []string
}

// Available reports whether this machine can run compose at all. It is asked
// once, at startup, so a server without the plugin does not offer advanced mode
// rather than offering one that fails at the first session.
func (r *Runner) Available(ctx context.Context) bool {
	_, err := r.run(ctx, "version", "", nil, "compose", "version")
	return err == nil
}

// Up creates and starts the project's containers.
func (r *Runner) Up(ctx context.Context, p Project) error {
	_, err := r.run(ctx, "up", p.Dir, nil, append(r.projectArgs(p), "up", "--detach")...)
	return err
}

// Create makes the project's containers without starting them, recreating the
// ones whose definition has changed since they were made. It is how a stopped
// session takes a container built to a new specification: compose owns these
// containers, so replacing one by hand would leave the project describing
// something that is no longer there.
func (r *Runner) Create(ctx context.Context, p Project) error {
	_, err := r.run(ctx, "create", p.Dir, nil, append(r.projectArgs(p), "create")...)
	return err
}

// Start brings a stopped project's containers back up. It does not create
// anything: the containers are the ones Up made.
func (r *Runner) Start(ctx context.Context, p Project) error {
	_, err := r.run(ctx, "start", p.Dir, nil, append(r.projectArgs(p), "start")...)
	return err
}

// Stop shuts the project's containers down, keeping them and their volumes.
func (r *Runner) Stop(ctx context.Context, p Project) error {
	_, err := r.run(ctx, "stop", p.Dir, nil, append(r.projectArgs(p), "stop")...)
	return err
}

// Down removes the project's containers and networks, and its named volumes
// when volumes is set. Those volumes hold whatever the services wrote, which is
// work rather than machinery: they go with the workspace, not with the
// container.
func (r *Runner) Down(ctx context.Context, p Project, volumes bool) error {
	args := append(r.projectArgs(p), "down", "--remove-orphans")
	if volumes {
		args = append(args, "--volumes")
	}
	_, err := r.run(ctx, "down", p.Dir, nil, args...)
	return err
}

func (r *Runner) projectArgs(p Project) []string {
	args := []string{"compose", "--project-name", p.Name}
	for _, f := range p.Files {
		args = append(args, "--file", f)
	}
	return args
}

// Validate normalizes content with the compose CLI, refuses the keys that would
// let a service out of its container, and returns the names of the services it
// describes.
//
// The names come back because the caller needs them: Hexagon's own service
// depends_on every one of them, and every one of them is labelled with the
// session it belongs to.
func (r *Runner) Validate(ctx context.Context, content string) ([]string, error) {
	// `config` resolves aliases, extends, merge keys, interpolation and the
	// short and long form of every key into one document, so a refusal below
	// cannot be dodged by writing the same request another way. Reading it as
	// JSON is also what keeps a YAML parser out of go.mod.
	normalized, err := r.run(ctx, "config", "", strings.NewReader(content),
		"compose", "--project-name", composeValidateProjectName, "--file", "-", "config", "--format", "json")
	if err != nil {
		return nil, err
	}
	return check(normalized)
}

// run executes the CLI and turns a non-zero exit into an error carrying what it
// said, which is the only useful thing to show a user whose compose file the
// CLI did not like. op names the subcommand for that message: the arguments
// carry it too, but buried among the files and the project name.
func (r *Runner) run(ctx context.Context, op, dir string, stdin *strings.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if r.host != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+r.host)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("docker compose took too long: %w", ctx.Err())
		}
		return nil, fmt.Errorf("docker compose %s: %w: %s", op, err, firstLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// firstLine keeps an error message to one line: these end up in a JSON error
// body and in the UI, and a compose failure can be pages of it.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	const max = 300
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
