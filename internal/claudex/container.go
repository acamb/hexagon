package claudex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
)

// Docker is the part of the Docker Engine API this package needs to run the CLI
// without a binary on the host. It is declared here, by the consumer, so the
// container path can be tested without a daemon; dockerx.API satisfies it.
type Docker interface {
	BuildImage(ctx context.Context, dockerfile, tag string, logs io.Writer) error
	CreateContainer(ctx context.Context, spec dockerx.ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	RunExec(ctx context.Context, containerID string, cmd []string) (string, int, error)
	RemoveContainer(ctx context.Context, id string, force bool) error
}

// defaultImageTimeout bounds the build. It is the image every session is
// expected to be built from, so it installs a Debian base and an npm package;
// on a slow link that is minutes, and this is the point at which it has stopped
// rather than slowed.
const defaultImageTimeout = 30 * time.Minute

// DefaultImage is the image Hexagon builds for itself, from the same reference
// Dockerfile the Images page offers as a starting point.
//
// It exists because two features need a container with Claude Code in it before
// the user has built anything: the browser login, which runs the CLI's sign-in
// flow, and the source editor on a server whose host has no claude binary. Both
// used to be dead ends on a fresh install — one of them silently, since a page
// cannot explain a button it does not show.
//
// The tag carries a hash of the Dockerfile, so editing the reference file
// produces a different image rather than a stale one under the same name.
type DefaultImage struct {
	docker     Docker
	dockerfile string
	tag        string
	log        *slog.Logger

	mu       sync.Mutex
	ready    bool
	building bool
	// lastError is why the last build failed, kept so the UI can say that
	// rather than "not ready yet" forever.
	lastError string
	// lastAttempt is when the last build started, and it is what stops a failure
	// from becoming a build storm: the state is asked for on every poll of the
	// Accounts page, and a daemon that is out of disk would otherwise be asked
	// to try again several times a minute, forever.
	lastAttempt time.Time
}

// retryAfter is how long a failed build is left alone. Long enough that polling
// does not retry it, short enough that a daemon which was briefly unreachable
// gets another chance without a restart of the server.
const retryAfter = time.Minute

// NewDefaultImage prepares the builder. Nothing is built until somebody asks
// for the image: a server whose user only ever pulls registry images should not
// spend a build on this.
func NewDefaultImage(docker Docker, dockerfile string, log *slog.Logger) *DefaultImage {
	sum := sha256.Sum256([]byte(dockerfile))
	return &DefaultImage{
		docker:     docker,
		dockerfile: dockerfile,
		tag:        "hexagon-default:" + hex.EncodeToString(sum[:6]),
		log:        log,
	}
}

// State is what the image is, and what can be said about it.
type State struct {
	// Ref is the tag a container is created from. It is meaningful only when
	// Ready is set.
	Ref   string
	Ready bool
	// Building is true while a build is running, which is the difference
	// between "ask again in a minute" and "this failed".
	Building bool
	// Error is why the last build failed, empty when none has.
	Error string
}

// State reports the image, and starts building it when there is no build
// running and none has succeeded.
//
// Asking is what starts the work, and the call never waits for it. A build is
// minutes long: a request that blocked on one would be a browser waiting on a
// socket for as long as an image takes, behind whatever proxy is in front of
// this server. So a caller that finds the image missing is told so, and the
// answer is to ask again shortly rather than to do anything.
func (d *DefaultImage) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch {
	case d.ready:
		return State{Ref: d.tag, Ready: true}
	case d.building:
		return State{Ref: d.tag, Building: true}
	case time.Since(d.lastAttempt) < retryAfter:
		// A build failed a moment ago. Report why rather than starting the same
		// one again on the next poll.
		return State{Ref: d.tag, Error: d.lastError}
	}

	d.building, d.lastAttempt = true, time.Now()
	go d.build()
	return State{Ref: d.tag, Building: true}
}

// Ref is the tag the image is built under, whether or not it has ever been
// built. Unlike State it starts nothing: it is what a caller asks for when it
// needs to recognise the image — the image prune protects it by name — and
// starting a build as a side effect of listing images would be absurd.
func (d *DefaultImage) Ref() string { return d.tag }

// Invalidate says the image behind the tag is gone from the daemon, and starts
// building it again.
//
// It exists because the daemon is not Hexagon's alone: an image prune, here or
// from a terminal, can remove an image this builder has already recorded as
// ready, and the failure that follows is a container creation refused with "No
// such image" — a dead end for a user who cannot ask for the build themselves.
//
// The build starts here rather than on the next State call: lastAttempt is
// recent after a successful build, so State would spend retryAfter reporting
// neither ready, nor building, nor an error.
func (d *DefaultImage) Invalidate() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.ready, d.lastError = false, ""
	if d.building {
		return
	}
	d.building, d.lastAttempt = true, time.Now()
	go d.build()
}

// build runs one build to completion on its own context: whoever asked for the
// image is not waiting, and may well be gone.
func (d *DefaultImage) build() {
	ctx, cancel := context.WithTimeout(context.Background(), defaultImageTimeout)
	defer cancel()

	d.log.Info("building the default image claude code runs in", "tag", d.tag)
	var logs bytes.Buffer
	err := d.docker.BuildImage(ctx, d.dockerfile, d.tag, &logs)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.building = false
	if err != nil {
		// The daemon's own last words, which are the only useful part of a
		// build log in a one-line error.
		d.lastError = firstLine(err.Error())
		if tail := lastLine(logs.String()); tail != "" {
			d.lastError += ": " + tail
		}
		d.log.Error("build the default image", "tag", d.tag, "err", err, "log", lastLine(logs.String()))
		return
	}
	d.ready, d.lastError = true, ""
	d.log.Info("the default image is ready", "tag", d.tag)
}

// lastLine is the tail of a build log: a build fails at its end, and the
// beginning is the part that worked.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return firstLine(lines[len(lines)-1])
}

// Container runs the Claude Code CLI inside a container built from the default
// image, for a server that has no claude binary of its own.
//
// It is slower than the binary — a container is created, started and removed
// around every call — and that is worth saying in the UI rather than hiding,
// because the alternative on such a server is not a faster answer but no answer
// at all.
type Container struct {
	docker Docker
	image  *DefaultImage
	// user is the uid:gid the container runs as, the same one sessions use:
	// nothing Hexagon starts runs as root.
	user string
	// credentials is the host file a browser login writes, and the same one
	// every session mounts. It is what this runs as when the user has pasted no
	// credential of its own; empty means there is none to fall back to.
	credentials string
	model       string
	log         *slog.Logger
}

// NewContainer wires the container runner.
func NewContainer(docker Docker, image *DefaultImage, user, credentials, model string, log *slog.Logger) *Container {
	return &Container{docker: docker, image: image, user: user,
		credentials: credentials, model: model, log: log}
}

// Edit and Check are the same calls the binary makes, run somewhere else.
func (c *Container) Edit(ctx context.Context, cred Credential, kind, content, instruction string) (Edit, error) {
	return edit(ctx, c, cred, kind, content, instruction)
}

func (c *Container) Check(ctx context.Context, cred Credential) error {
	return check(ctx, c, cred)
}

// Where the CLI's own state goes inside the container, and the names the
// invocation's values travel under.
//
// Everything variable is passed in the environment and nothing is interpolated
// into the script: the schema is a JSON document and a prompt is whatever a user
// typed, and a shell command built out of either is a quoting bug waiting for
// the input that triggers it.
const (
	containerHome = "/tmp/hexagon-claude"
	promptEnv     = "HEXAGON_PROMPT"
	schemaEnv     = "HEXAGON_SCHEMA"
	modelEnv      = "HEXAGON_MODEL"
	// credentialsMount is where the host's credentials file is bind mounted,
	// outside $HOME on purpose. Mounted straight at $HOME/.claude/... it would
	// have Docker create that directory as root, in a home the container's own
	// user has to be able to write; the script copies it into place instead.
	credentialsMount = "/tmp/hexagon-claude-credentials.json"
	// stderrFile keeps the CLI's diagnostics out of its answer. The daemon
	// folds an exec's two output streams into one, and the answer is parsed as
	// a JSON document: a warning printed beside it would make a good reply
	// unreadable.
	stderrFile = "/tmp/hexagon-claude.err"
	// containerRole labels the container so the session reconciler can tell it
	// from one that lost its session. It carries no hexagon.managed label for
	// the same reason the browser login's container does not: there is no
	// session row to match it against.
	containerRole = "claude-editor"
)

// run creates a container, asks the CLI inside it, and removes the container
// again.
func (c *Container) run(ctx context.Context, cred Credential, in invocation) ([]byte, error) {
	image := c.image.State()
	switch {
	case image.Ready:
	case image.Error != "":
		return nil, fmt.Errorf("the image claude code runs in could not be built: %s", image.Error)
	default:
		return nil, fmt.Errorf("this server has no claude code binary, so it runs the CLI in a container, " +
			"and that image is still being built — try again in a minute")
	}

	env := []string{
		"HOME=" + containerHome,
		promptEnv + "=" + in.prompt,
		schemaEnv + "=" + in.schema,
		modelEnv + "=" + c.model,
	}
	// The credential last, so it wins over anything of the same name above.
	env = append(env, cred.Env()...)

	// With no pasted secret to hand over, a file is what is left: cred.File
	// when the credential resolved to a login account, or the login on this
	// machine when it resolved to nothing at all — which is exactly what every
	// session authenticates with in that case. Without the fallback the two
	// disagree: a session signs in and the editor reports "Not logged in", for
	// the same user, on the same server, five minutes apart.
	var binds []string
	switch {
	case cred.File != "":
		if _, err := os.Stat(cred.File); err == nil {
			// Read-only: the container gets to use the login, not to change it.
			binds = append(binds, cred.File+":"+credentialsMount+":ro")
		}
	case cred.Secret == "" && c.credentials != "":
		if _, err := os.Stat(c.credentials); err == nil {
			binds = append(binds, c.credentials+":"+credentialsMount+":ro")
		}
	}

	id, err := c.docker.CreateContainer(ctx, dockerx.ContainerSpec{
		Image: image.Ref,
		// The container is a place to run one exec in, and it is removed as
		// soon as that exec returns.
		Cmd:        []string{"sleep", "infinity"},
		Env:        env,
		Binds:      binds,
		WorkingDir: "/tmp",
		User:       c.user,
		GroupAdd:   []string{dockerx.AgentGroup},
		Labels:     map[string]string{dockerx.LabelRole: containerRole},
	})
	if errors.Is(err, dockerx.ErrImageNotFound) {
		// The image was there when it was built and is not there now: something
		// removed it behind Hexagon's back. Start it again and say so, rather
		// than repeating the daemon's "No such image" at a user who has no
		// button for it.
		c.log.Warn("the image claude code runs in is gone, rebuilding it", "image", image.Ref)
		c.image.Invalidate()
		return nil, fmt.Errorf("the image claude code runs in was removed, so it is being built again — " +
			"try again in a few minutes")
	}
	if err != nil {
		return nil, fmt.Errorf("create the container claude code runs in: %w", err)
	}
	defer func() {
		if err := c.docker.RemoveContainer(context.WithoutCancel(ctx), id, true); err != nil {
			// Not the caller's problem: they have their answer, or their error.
			c.log.Warn("remove the container claude code ran in", "container", id, "err", err)
		}
	}()

	if err := c.docker.StartContainer(ctx, id); err != nil {
		return nil, fmt.Errorf("start the container claude code runs in: %w", err)
	}

	c.log.Debug("running claude code in a container", "image", image.Ref, "container", id,
		"credential", describe(cred), "model", c.model, "mounted_credentials", len(binds) == 1)
	out, code, err := c.docker.RunExec(ctx, id, []string{"sh", "-c", c.script(in)})
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("claude code took too long: %w", ctx.Err())
		}
		return nil, fmt.Errorf("claude code failed: %w", err)
	}
	if code != 0 {
		// The answer, for the reason the binary's own path gives: the CLI
		// reports a refused credential in its exit status and in a document
		// that says what was wrong with it, and only the second is worth
		// reading.
		answered := isEnvelope([]byte(out))

		// The CLI's diagnostics live in a file, so reading them is another exec
		// into a container that is about to be removed. It is done when it will
		// be used: to explain a failure that has no answer, or because debug
		// logging asked for everything — and what a 401 was really about is in
		// there and in nothing that reaches the user.
		var diagnostics string
		debug := c.log.Enabled(ctx, slog.LevelDebug)
		if debug || !answered {
			diagnostics, _, _ = c.docker.RunExec(context.WithoutCancel(ctx), id, []string{"cat", stderrFile})
		}
		if debug {
			c.log.Debug("claude code failed in its container", "container", id, "exit", code,
				"stdout", firstLine(out), "stderr", firstLine(diagnostics))
		}

		if answered {
			return []byte(out), nil
		}
		return nil, fmt.Errorf("claude code failed (exit %d): %s", code, firstLine(diagnostics))
	}
	return []byte(out), nil
}

// script is what runs inside the container: the CLI, with its arguments named
// here and their values read from the environment.
//
// The seeded configuration file is not decoration. Without it Claude Code finds
// a machine it has never run on and starts its first-run onboarding, which for
// a non-interactive call is an answer that never comes.
func (c *Container) script(in invocation) string {
	script := "set -e\n" +
		"mkdir -p " + containerHome + "/.claude\n" +
		`printf '%s' '{"hasCompletedOnboarding":true}' > ` + containerHome + "/.claude.json\n" +
		// A copy rather than the mount itself: the CLI refreshes an expiring
		// token by rewriting this file, and what it writes belongs to a
		// container that is about to be removed — never to the host's own
		// login, which this call has no business changing.
		"if [ -f " + credentialsMount + " ]; then\n" +
		"  cp " + credentialsMount + " " + containerHome + "/.claude/.credentials.json\n" +
		"  chmod 600 " + containerHome + "/.claude/.credentials.json\n" +
		"fi\n" +
		`claude -p "$` + promptEnv + `" --safe-mode --strict-mcp-config --tools '' --output-format json`
	if in.schema != "" {
		script += ` --json-schema "$` + schemaEnv + `"`
	}
	if c.model != "" {
		script += ` --model "$` + modelEnv + `"`
	}
	return script + " 2>" + stderrFile
}
