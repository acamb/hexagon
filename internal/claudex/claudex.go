// Package claudex runs the Claude Code CLI for the one thing Hexagon asks of it
// outside a session: rewriting an image's source — a Dockerfile or a compose
// file — from an instruction in English.
//
// The call is a pure text transformation. It runs with every built-in tool
// removed, so the agent has no shell, no file access and no network of its own;
// the file goes in through the prompt and comes back in the answer. That is what
// makes running it as the server user uninteresting to attack.
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
// 009_claude_accounts.sql. api_key and oauth_token choose a variable name,
// which is why they are defined here rather than beside the table; KindLogin
// names no variable at all, and its constant is store.ClaudeAccountKindLogin
// instead.
const (
	KindAPIKey     = "api_key"
	KindOAuthToken = "oauth_token"
	// KindLogin means the credential is a file Claude Code itself wrote —
	// Secret is always empty and File names it instead.
	KindLogin = "login"
)

// Credential is how the CLI is authenticated. The zero value means "whatever
// the server process already has", which is the host user's own login and the
// behaviour this package had before there was anywhere to configure one.
type Credential struct {
	Kind   string
	Secret string
	// File is set only for KindLogin: the credentials file an account's own
	// browser login wrote, to be bind mounted wherever this credential runs.
	File string
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

// The two things Claude Code can be asked to rewrite. The values are the
// image source types they belong to, and they are also the field names the
// answer arrives under, which is why there is one constant rather than three.
const (
	SourceDockerfile = "dockerfile"
	SourceCompose    = "compose"
)

// Edit is what the model came back with.
type Edit struct {
	Content string `json:"content"`
	Summary string `json:"summary"`
}

// editKind is one editable file: the prompt that describes the job and the
// schema that forces the answer into a shape which can be used directly.
//
// Everything that makes this call safe is independent of which file it is —
// --safe-mode and --strict-mcp-config so the developer's own Claude Code setup
// cannot change what a Hexagon request does, --tools "" so there is nothing to
// run, read or fetch, and --json-schema so the answer does not arrive wrapped in
// a Markdown fence. Only these two are per kind.
type editKind struct {
	prompt string
	schema string
}

var editKinds = map[string]editKind{
	SourceDockerfile: {prompt: dockerfilePrompt, schema: schemaFor(SourceDockerfile)},
	SourceCompose:    {prompt: composePrompt, schema: schemaFor(SourceCompose)},
}

// schemaFor forces the answer into an object with the file under its own kind's
// name. Without a schema the reply arrives wrapped in a Markdown fence however
// plainly the prompt asks for the file alone, and unwrapping that is guesswork.
func schemaFor(kind string) string {
	return `{
	"type": "object",
	"properties": {
		"` + kind + `": {"type": "string"},
		"summary": {"type": "string"}
	},
	"required": ["` + kind + `", "summary"],
	"additionalProperties": false
}`
}

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

// composePrompt carries the rules the validator enforces. That is a
// convenience and not the check: what comes back goes through exactly the same
// validation as a file typed by hand, and a refusal lands back in the editor for
// the user to ask again. A prompt is not a security boundary, and this is said
// here because a reader who finds the rules stated in two places would otherwise
// have to guess which copy is load-bearing.
const composePrompt = `You are editing a Docker Compose file that describes the services running
beside a container in which Claude Code works on a repository. Hexagon supplies
that container itself, as a service named "hexagon" in a second file, so this
file describes only the services next to it — a database, a cache, a queue — and
must not describe the agent.

The file is refused unless every service obeys all of these: no service named
"hexagon"; no build, so every service comes from a registry image; no bind
mounts of host paths, though named volumes are fine; no fixed host port in
ports, since Hexagon publishes what it needs itself; no privileged, cap_add,
security_opt or devices; no network_mode, pid, ipc or uts set to host; and no
service running as root.

Apply the requested change and return the complete resulting compose file, not a
patch. Change nothing the request did not ask for. Summarise what you changed in
one short sentence.

Requested change:
%s

Current compose file:
%s
`

// result is the envelope --output-format json wraps the answer in.
type result struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// invocation is one call to the CLI: the shape the answer has to take, and the
// prompt that asks for it.
//
// It exists because there are two ways to run that call — the binary on this
// machine, and a container built for the purpose — and everything except the
// running is the same for both. What the two disagree about is how a prompt and
// a schema reach the process, which is why they are carried here as values
// rather than as command-line arguments already rendered.
type invocation struct {
	// schema is the JSON schema the answer must satisfy, or empty for a call
	// whose answer is not read.
	schema string
	prompt string
}

// cli is one way of running Claude Code. Both implementations are in this
// package: Runner, which executes the binary, and Container, which runs the CLI
// inside a container on a server that has no binary at all.
type cli interface {
	// run performs one invocation and returns the CLI's standard output, or an
	// error already worded for somebody who will read it in a browser.
	run(ctx context.Context, cred Credential, in invocation) ([]byte, error)
}

// edit is Edit for either runner: it builds the prompt, runs the call, and
// reads the file out of the answer.
func edit(ctx context.Context, c cli, cred Credential, kind, content, instruction string) (Edit, error) {
	spec, ok := editKinds[kind]
	if !ok {
		return Edit{}, fmt.Errorf("claudex: no such editable file: %q", kind)
	}

	stdout, err := c.run(ctx, cred, invocation{
		schema: spec.schema,
		prompt: fmt.Sprintf(spec.prompt, instruction, content),
	})
	if err != nil {
		return Edit{}, err
	}
	result, err := answerOf(stdout)
	if err != nil {
		return Edit{}, err
	}

	// With --json-schema the answer is a JSON document inside the envelope's
	// result string, with the file under the kind's own name. An answer that
	// carries a different kind is one for a question that was not asked.
	answer := map[string]string{}
	if err := json.Unmarshal([]byte(result), &answer); err != nil {
		return Edit{}, fmt.Errorf("claude code did not answer with a %s: %w", kind, err)
	}
	out := Edit{Content: answer[kind], Summary: answer["summary"]}
	if strings.TrimSpace(out.Content) == "" {
		return Edit{}, fmt.Errorf("claude code answered with an empty %s", kind)
	}
	return out, nil
}

// check is Check for either runner.
func check(ctx context.Context, c cli, cred Credential) error {
	stdout, err := c.run(ctx, cred, invocation{prompt: checkPrompt})
	if err != nil {
		return err
	}
	_, err = answerOf(stdout)
	return err
}

// checkPrompt is the cheapest question that still proves the credential works.
const checkPrompt = "Reply with the single word: ok"

// answerOf unwraps the envelope --output-format json arrives in and returns the
// answer inside it.
func answerOf(stdout []byte) (string, error) {
	var res result
	if err := json.Unmarshal(stdout, &res); err != nil {
		return "", fmt.Errorf("claude code returned something unreadable: %w", err)
	}
	if res.IsError {
		return "", fmt.Errorf("claude code reported an error: %s", firstLine(res.Result))
	}
	return res.Result, nil
}

// flags are the arguments every invocation carries, whichever way it is run.
//
// All of these are what makes the call safe rather than what makes it work:
// --safe-mode and --strict-mcp-config so the machine's own Claude Code setup —
// CLAUDE.md, skills, plugins, hooks, MCP servers — cannot change what a Hexagon
// request does, --tools so there is nothing to run, read or fetch, and
// --output-format json so the answer arrives in a document rather than in prose.
func (in invocation) flags(model string) []string {
	args := []string{"-p", "--safe-mode", "--strict-mcp-config", "--tools", "", "--output-format", "json"}
	if in.schema != "" {
		args = append(args, "--json-schema", in.schema)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// Edit asks for content with instruction applied to it. kind is SourceDockerfile
// or SourceCompose; cred is the credential to authenticate with, or the zero
// value to inherit the server's own login.
func (r *Runner) Edit(ctx context.Context, cred Credential, kind, content, instruction string) (Edit, error) {
	return edit(ctx, r, cred, kind, content, instruction)
}

// run executes the binary, with the prompt on its standard input.
func (r *Runner) run(ctx context.Context, cred Credential, in invocation) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.binary, in.flags(r.model)...)
	cmd.Stdin = strings.NewReader(in.prompt)
	cmd.Env = environment(cred)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("claude code took too long: %w", ctx.Err())
		}
		// A refused credential is reported twice: in the exit status, and in an
		// answer that says which of the several things went wrong. Where there
		// is an answer it is the half worth reading — "Not logged in" beats
		// "exit status 1" — so it is handed on and read by the caller.
		if isEnvelope(stdout.Bytes()) {
			return stdout.Bytes(), nil
		}
		return nil, fmt.Errorf("claude code failed: %w: %s", err, firstLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// isEnvelope reports whether output is the document --output-format json
// produces, rather than whatever a process that failed before it got that far
// left behind.
func isEnvelope(output []byte) bool {
	var res result
	return json.Unmarshal(output, &res) == nil
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
	return check(ctx, r, cred)
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
