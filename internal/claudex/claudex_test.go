package claudex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a script that stands in for the CLI: it records the
// arguments, the environment and the prompt it was given, then prints body and
// exits with code. The binary being a path is what makes this possible, so
// none of these tests reach the network or spend anything.
func fakeClaude(t *testing.T, body string, code int) (*Runner, func() (args, stdin string)) {
	t.Helper()
	runner, recordedEnv := fakeClaudeEnv(t, body, code)
	return runner, func() (string, string) {
		t.Helper()
		args, _ := os.ReadFile(recordedEnv.argsPath)
		stdin, _ := os.ReadFile(recordedEnv.stdinPath)
		return string(args), string(stdin)
	}
}

// recordedFiles names where fakeClaudeEnv's script writes what it saw.
type recordedFiles struct {
	dir, argsPath, stdinPath, envPath string
}

// fakeClaudeEnv is fakeClaude plus access to the environment the script ran
// with, for the tests that care what credential reached the child process.
func fakeClaudeEnv(t *testing.T, body string, code int) (*Runner, recordedFiles) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do printf '%%s\n' "$arg" >> %[1]s/args; done
cat >> %[1]s/stdin
env > %[1]s/env
cat <<'BODY'
%[2]s
BODY
exit %[3]d
`, dir, body, code)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	runner, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return runner, recordedFiles{
		dir:       dir,
		argsPath:  filepath.Join(dir, "args"),
		stdinPath: filepath.Join(dir, "stdin"),
		envPath:   filepath.Join(dir, "env"),
	}
}

func (r recordedFiles) env(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(r.envPath)
	return string(data)
}

// success is the envelope the CLI prints with --output-format json and a schema.
const success = `{"is_error":false,"result":"{\"dockerfile\":\"FROM busybox\\nRUN true\",\"summary\":\"Added a RUN\"}"}`

func TestEditDockerfileReturnsTheEditedFile(t *testing.T) {
	runner, recorded := fakeClaude(t, success, 0)

	edit, err := runner.EditDockerfile(context.Background(), Credential{}, "FROM busybox", "add a RUN")
	if err != nil {
		t.Fatalf("EditDockerfile: %v", err)
	}
	if edit.Dockerfile != "FROM busybox\nRUN true" {
		t.Errorf("dockerfile = %q", edit.Dockerfile)
	}
	if edit.Summary != "Added a RUN" {
		t.Errorf("summary = %q", edit.Summary)
	}

	args, stdin := recorded()
	if !strings.Contains(stdin, "add a RUN") || !strings.Contains(stdin, "FROM busybox") {
		t.Errorf("the prompt did not carry the instruction and the Dockerfile:\n%s", stdin)
	}
	// Every one of these is load bearing, so the test says so rather than
	// leaving a later reader to decide one of them looks optional.
	for _, want := range []string{"-p", "--safe-mode", "--strict-mcp-config", "--tools", "--json-schema"} {
		if !strings.Contains(args, want+"\n") {
			t.Errorf("%s is missing from the arguments:\n%s", want, args)
		}
	}
	// --tools is followed by an empty value: no built-in tool is available, so
	// the agent has no shell, no file access and no fetch.
	if !strings.Contains(args, "--tools\n\n") {
		t.Errorf("--tools was not passed an empty list:\n%q", args)
	}
	if strings.Contains(args, "--model") {
		t.Errorf("a model was chosen when none was configured:\n%s", args)
	}
}

func TestEditDockerfilePassesTheConfiguredModel(t *testing.T) {
	runner, recorded := fakeClaude(t, success, 0)

	if _, err := runner.WithModel("sonnet").EditDockerfile(context.Background(), Credential{}, "FROM busybox", "x"); err != nil {
		t.Fatalf("EditDockerfile: %v", err)
	}
	args, _ := recorded()
	if !strings.Contains(args, "--model\nsonnet\n") {
		t.Errorf("the model was not passed:\n%s", args)
	}
}

func TestEditDockerfileReportsFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
		want string
	}{
		{"a non-zero exit", "not logged in", 1, "claude code failed"},
		{"output that is not JSON", "hello", 0, "unreadable"},
		{"an error the CLI reports itself", `{"is_error":true,"result":"credit balance too low"}`, 0, "reported an error"},
		{"an answer that is not a Dockerfile", `{"is_error":false,"result":"sorry"}`, 0, "did not answer with a Dockerfile"},
		{"an empty Dockerfile", `{"is_error":false,"result":"{\"dockerfile\":\"  \",\"summary\":\"\"}"}`, 0, "empty Dockerfile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runner, _ := fakeClaude(t, c.body, c.code)

			_, err := runner.EditDockerfile(context.Background(), Credential{}, "FROM busybox", "x")
			if err == nil {
				t.Fatal("EditDockerfile accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestEditDockerfileGivesUpWhenTheContextEnds(t *testing.T) {
	runner, _ := fakeClaude(t, success, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	if _, err := runner.EditDockerfile(ctx, Credential{}, "FROM busybox", "x"); err == nil {
		t.Fatal("EditDockerfile ignored a context that was already over")
	}
}

func TestEditDockerfilePassesTheCredentialByKind(t *testing.T) {
	cases := []struct {
		name string
		cred Credential
		want string
		none string
	}{
		{"api key", Credential{Kind: KindAPIKey, Secret: "sk-ant-x"}, "ANTHROPIC_API_KEY=sk-ant-x", "CLAUDE_CODE_OAUTH_TOKEN="},
		{"oauth token", Credential{Kind: KindOAuthToken, Secret: "oauth-x"}, "CLAUDE_CODE_OAUTH_TOKEN=oauth-x", "ANTHROPIC_API_KEY="},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runner, recorded := fakeClaudeEnv(t, success, 0)
			if _, err := runner.EditDockerfile(context.Background(), c.cred, "FROM busybox", "x"); err != nil {
				t.Fatalf("EditDockerfile: %v", err)
			}
			env := recorded.env(t)
			if !strings.Contains(env, c.want+"\n") {
				t.Errorf("environment did not carry %q:\n%s", c.want, env)
			}
			if strings.Contains(env, c.none) {
				t.Errorf("environment carried the other credential's variable:\n%s", env)
			}
		})
	}
}

func TestEditDockerfileWithNoCredentialLeavesTheEnvironmentAlone(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "server-key")
	runner, recorded := fakeClaudeEnv(t, success, 0)

	if _, err := runner.EditDockerfile(context.Background(), Credential{}, "FROM busybox", "x"); err != nil {
		t.Fatalf("EditDockerfile: %v", err)
	}
	env := recorded.env(t)
	if !strings.Contains(env, "ANTHROPIC_API_KEY=server-key\n") {
		t.Errorf("the server's own environment did not reach the child:\n%s", env)
	}
}

func TestEditDockerfileCredentialReplacesTheServersOwn(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "server-key")
	runner, recorded := fakeClaudeEnv(t, success, 0)

	cred := Credential{Kind: KindAPIKey, Secret: "stored-key"}
	if _, err := runner.EditDockerfile(context.Background(), cred, "FROM busybox", "x"); err != nil {
		t.Fatalf("EditDockerfile: %v", err)
	}
	env := recorded.env(t)
	if strings.Count(env, "ANTHROPIC_API_KEY=") != 1 {
		t.Fatalf("ANTHROPIC_API_KEY appears %d times, want exactly one:\n%s", strings.Count(env, "ANTHROPIC_API_KEY="), env)
	}
	if !strings.Contains(env, "ANTHROPIC_API_KEY=stored-key\n") {
		t.Errorf("the stored credential did not win over the server's own:\n%s", env)
	}
}

func TestCheckReportsWhatTheCLISaid(t *testing.T) {
	runner, _ := fakeClaude(t, `{"is_error":false,"result":"ok"}`, 0)
	if err := runner.Check(context.Background(), Credential{}); err != nil {
		t.Errorf("Check: %v", err)
	}

	runner, _ = fakeClaude(t, "not logged in", 1)
	err := runner.Check(context.Background(), Credential{})
	if err == nil || !strings.Contains(err.Error(), "claude code failed") {
		t.Errorf("Check error = %v, want it to name the failure", err)
	}
}

func TestNewRefusesSomethingThatIsNotExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := New(path)
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable for a path that does not exist", err)
	}
}
