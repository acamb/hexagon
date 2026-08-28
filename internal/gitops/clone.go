// Package gitops runs the git commands Hexagon needs on the host: cloning a
// repository into a session workspace and giving it an identity to commit with.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// tokenEnvVar carries the GitHub token to git. It is passed through the
// environment rather than the command line so it never appears in the process
// table, and never through the URL so it never lands in .git/config.
const tokenEnvVar = "HEXAGON_GH_TOKEN"

// credentialHelper answers git's credential prompt from the environment. The
// helper runs for the duration of the clone and nothing is written to disk.
const credentialHelper = `!f(){ echo username=x-access-token; echo "password=$` + tokenEnvVar + `"; }; f`

// Options describes one clone.
type Options struct {
	// CloneURL is the https URL of the repository.
	CloneURL string
	// Branch is checked out after cloning. Empty means the default branch.
	Branch string
	// Dest is the directory to create the working tree in. It must not exist,
	// or must be empty.
	Dest string
	// Token authenticates to GitHub. Empty works for public repositories.
	Token string
	// UserName and UserEmail become the repository's commit identity. When
	// empty, git falls back to whatever the host has configured globally.
	UserName  string
	UserEmail string
}

// Clone checks out a repository into opts.Dest.
func Clone(ctx context.Context, opts Options) error {
	if opts.CloneURL == "" || opts.Dest == "" {
		return errors.New("clone: a clone URL and a destination are required")
	}
	if err := os.MkdirAll(filepath.Dir(opts.Dest), 0o700); err != nil {
		return fmt.Errorf("clone: create workspace: %w", err)
	}

	args := []string{
		// Drop any helper the host has configured, so ours is the only one
		// asked and no keychain or credential store gets involved.
		"-c", "credential.helper=",
		"-c", "credential.helper=" + credentialHelper,
		"clone",
	}
	if opts.Branch != "" {
		args = append(args, "--branch", opts.Branch)
	}
	args = append(args, "--", opts.CloneURL, opts.Dest)

	if err := run(ctx, "", opts.Token, args...); err != nil {
		return fmt.Errorf("clone %s: %w", opts.CloneURL, err)
	}
	return configureIdentity(ctx, opts)
}

// configureIdentity records who commits from inside the container. Values left
// empty are not set at all, so the host's global git identity applies.
func configureIdentity(ctx context.Context, opts Options) error {
	settings := [][2]string{
		{"user.name", opts.UserName},
		{"user.email", opts.UserEmail},
	}
	for _, setting := range settings {
		if setting[1] == "" {
			continue
		}
		if err := run(ctx, opts.Dest, "", "config", "--local", setting[0], setting[1]); err != nil {
			return fmt.Errorf("configure %s: %w", setting[0], err)
		}
	}
	return nil
}

// run executes git, returning its output as part of any error.
func run(ctx context.Context, dir, token string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		tokenEnvVar+"="+token,
		// Without this a repository we cannot authenticate to would sit
		// waiting for a username on a terminal that does not exist.
		"GIT_TERMINAL_PROMPT=0",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, scrub(message, token))
	}
	return nil
}

// scrub keeps a token out of an error that may be logged or shown in the UI.
// git has no reason to echo it, which is exactly why this belongs here: the day
// it does, the leak should not reach a log file.
func scrub(message, token string) string {
	if token == "" {
		return message
	}
	return strings.ReplaceAll(message, token, "[redacted]")
}
