// Package claudex runs the Claude Code CLI for the one thing Hexagon asks of it
// outside a session: rewriting a Dockerfile from an instruction in English.
//
// The call is a pure text transformation. It runs with every built-in tool
// removed, so the agent has no shell, no file access and no network of its own;
// the Dockerfile goes in through the prompt and comes back in the answer. That
// is what makes running it as the server user uninteresting to attack.
package claudex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrUnavailable reports that no Claude Code binary was found, so the feature
// cannot be offered at all.
var ErrUnavailable = errors.New("claude code is not available")

// Credential kinds, mirrored by the CHECK constraint in
// 006_claude_credentials.sql. The kind exists to choose a variable name, which
// is why it is defined here rather than beside the table.
const (
	KindAPIKey     = "api_key"
	KindOAuthToken = "oauth_token"
)

// Credential is how the CLI is authenticated. The zero value means "whatever
// the server process already has", which is the host user's own login and the
// behaviour this package had before there was anywhere to configure one.
type Credential struct {
	Kind   string
	Secret string
}

// Env returns the assignments that hand this credential over, and nothing at
// all for the zero value.
func (c Credential) Env() []string {
	if c.Secret == "" {
		return nil
	}
	if c.Kind == KindOAuthToken {
		return []string{"CLAUDE_CODE_OAUTH_TOKEN=" + c.Secret}
	}
	return []string{"ANTHROPIC_API_KEY=" + c.Secret}
}

// environment returns the server's own environment with any Claude Code
// credential in it replaced by cred, or nil to inherit it untouched.
//
// Appending would not do. With the same name present twice it is the C library
// that decides which one the child sees, and a server started with its own
// ANTHROPIC_API_KEY would go on using it about as often as not — a bug that
// only shows up on the machine where the variable happens to be set.
func environment(cred Credential) []string {
	assignments := cred.Env()
	if len(assignments) == 0 {
		return nil
	}
	var out []string
	for _, kv := range os.Environ() {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN":
			continue
		default:
			out = append(out, kv)
		}
	}
	return append(out, assignments...)
}

// Runner invokes the CLI. The zero value is unusable: build one with New.
type Runner struct {
	binary string
	// model is empty unless configured, in which case the CLI picks its own.
	model string
}

// New resolves the binary to run. Preference goes to an explicitly configured
// path; otherwise the PATH, and finally the location the official installer
// uses, which a server started outside a login shell may not have on its PATH.
func New(configured string) (*Runner, error) {
	if configured != "" {
		if err := executable(configured); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, configured, err)
		}
		return &Runner{binary: configured}, nil
	}
	if path, err := exec.LookPath("claude"); err == nil {
		return &Runner{binary: path}, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		fallback := filepath.Join(home, ".local", "bin", "claude")
		if err := executable(fallback); err == nil {
			return &Runner{binary: fallback}, nil
		}
	}
	return nil, fmt.Errorf("%w: no claude binary on PATH", ErrUnavailable)
}

func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("not an executable file")
	}
	return nil
}

// WithModel returns a runner that asks for a specific model. An empty name
// leaves the choice to the CLI's own configuration.
func (r *Runner) WithModel(model string) *Runner {
	return &Runner{binary: r.binary, model: model}
}

// DockerfileEdit is what the model came back with.
type DockerfileEdit struct {
	Dockerfile string `json:"dockerfile"`
	Summary    string `json:"summary"`
}

// dockerfileSchema forces the answer into a shape that can be used directly.
// Without it the reply arrives wrapped in a Markdown fence however plainly the
// prompt asks for the file alone, and unwrapping that is guesswork.
const dockerfileSchema = `{
	"type": "object",
	"properties": {
		"dockerfile": {"type": "string"},
		"summary": {"type": "string"}
	},
	"required": ["dockerfile", "summary"],
	"additionalProperties": false
}`

const dockerfilePrompt = `You are editing a Dockerfile for a container that runs Claude Code on a
repository. The image must keep working for that: git, tmux and claude on the
PATH, a long-running CMD, and no repository baked in — it arrives as a bind
mount on /workspace.

Apply the requested change and return the complete resulting Dockerfile, not a
patch. Change nothing the request did not ask for. Summarise what you changed in
one short sentence.

Requested change:
%s

Current Dockerfile:
%s
`

// result is the envelope --output-format json wraps the answer in.
type result struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// EditDockerfile asks for dockerfile with instruction applied to it. cred is
// the credential to authenticate with, or the zero value to inherit the
// server's own login.
func (r *Runner) EditDockerfile(ctx context.Context, cred Credential, dockerfile, instruction string) (DockerfileEdit, error) {
	args := []string{
		"-p",
		// The developer's own Claude Code setup — CLAUDE.md, skills, plugins,
		// hooks, MCP servers — must not change what a Hexagon request does.
		"--safe-mode",
		"--strict-mcp-config",
		// No tools at all: there is nothing to run, read or fetch here.
		"--tools", "",
		"--output-format", "json",
		"--json-schema", dockerfileSchema,
	}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}

	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Stdin = strings.NewReader(fmt.Sprintf(dockerfilePrompt, instruction, dockerfile))
	cmd.Env = environment(cred)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return DockerfileEdit{}, fmt.Errorf("claude code took too long: %w", ctx.Err())
		}
		return DockerfileEdit{}, fmt.Errorf("claude code failed: %w: %s", err, firstLine(stderr.String()))
	}

	var res result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return DockerfileEdit{}, fmt.Errorf("claude code returned something unreadable: %w", err)
	}
	if res.IsError {
		return DockerfileEdit{}, fmt.Errorf("claude code reported an error: %s", firstLine(res.Result))
	}

	// With --json-schema the answer is a JSON document inside the envelope's
	// result string.
	var edit DockerfileEdit
	if err := json.Unmarshal([]byte(res.Result), &edit); err != nil {
		return DockerfileEdit{}, fmt.Errorf("claude code did not answer with a Dockerfile: %w", err)
	}
	if strings.TrimSpace(edit.Dockerfile) == "" {
		return DockerfileEdit{}, errors.New("claude code answered with an empty Dockerfile")
	}
	return edit, nil
}

// Check reports whether the CLI can authenticate with cred. It is the cheapest
// call that proves the thing that matters: a credential the API would accept
// but Claude Code does not know how to use is still one that fails in a
// session, an hour later, where nobody can see why.
//
// Every failure is one error. The CLI does not distinguish a refused credential
// from an unreachable API in its exit status, and a distinction invented here
// would send the user off to check the wrong thing.
func (r *Runner) Check(ctx context.Context, cred Credential) error {
	args := []string{
		"-p",
		"--safe-mode",
		"--strict-mcp-config",
		"--tools", "",
		"--output-format", "json",
	}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}

	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Stdin = strings.NewReader("Reply with the single word: ok")
	cmd.Env = environment(cred)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("claude code took too long: %w", ctx.Err())
		}
		return fmt.Errorf("claude code failed: %w: %s", err, firstLine(stderr.String()))
	}

	var res result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return fmt.Errorf("claude code returned something unreadable: %w", err)
	}
	if res.IsError {
		return fmt.Errorf("claude code reported an error: %s", firstLine(res.Result))
	}
	return nil
}

// firstLine keeps an error message to one line: these end up in a JSON error
// body and in the UI, and a CLI failure can be pages of it.
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
