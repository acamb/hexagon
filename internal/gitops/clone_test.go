package gitops

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolateGit keeps the developer's own git configuration out of the test: no
// global identity, no credential helpers, no hooks.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "gitconfig-system"))
}

// gitTry runs git and hands back the outcome instead of failing the test.
func gitTry(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// sourceRepo builds a repository to clone from, with a second branch so branch
// selection can be exercised.
func sourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	git(t, dir, "init", "--initial-branch=main", dir)
	git(t, dir, "config", "user.name", "Source Author")
	git(t, dir, "config", "user.email", "source@example.test")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "-m", "initial commit")

	git(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	git(t, dir, "add", "feature.txt")
	git(t, dir, "commit", "-m", "feature commit")
	git(t, dir, "checkout", "main")

	return dir
}

func TestCloneCheckoutsTheDefaultBranch(t *testing.T) {
	isolateGit(t)
	source := sourceRepo(t)
	dest := filepath.Join(t.TempDir(), "workspace", "repo")

	err := Clone(context.Background(), Options{
		CloneURL:  source,
		Dest:      dest,
		Token:     "gho_secret_token",
		UserName:  "Hexagon User",
		UserEmail: "user@example.test",
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Fatalf("no repository at the destination: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(dest, "README.md")); err != nil || string(body) != "hello\n" {
		t.Errorf("working tree content = %q, %v", body, err)
	}
	if got := git(t, dest, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("checked out branch = %q, want main", got)
	}

	if got := git(t, dest, "config", "--local", "user.name"); got != "Hexagon User" {
		t.Errorf("user.name = %q", got)
	}
	if got := git(t, dest, "config", "--local", "user.email"); got != "user@example.test" {
		t.Errorf("user.email = %q", got)
	}
}

func TestCloneSelectsABranch(t *testing.T) {
	isolateGit(t)
	source := sourceRepo(t)
	dest := filepath.Join(t.TempDir(), "repo")

	if err := Clone(context.Background(), Options{CloneURL: source, Branch: "feature", Dest: dest}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if got := git(t, dest, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature" {
		t.Errorf("checked out branch = %q, want feature", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "feature.txt")); err != nil {
		t.Errorf("the branch content is missing: %v", err)
	}
}

// The whole point of the credential helper is that the token stays in the
// environment. If it ever reaches .git — through the remote URL or a persisted
// helper — anyone with the workspace has the user's GitHub account.
func TestCloneLeavesNoTokenOnDisk(t *testing.T) {
	isolateGit(t)
	source := sourceRepo(t)
	dest := filepath.Join(t.TempDir(), "repo")
	const token = "gho_a_very_secret_token"

	if err := Clone(context.Background(), Options{CloneURL: source, Dest: dest, Token: token}); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	err := filepath.WalkDir(filepath.Join(dest, ".git"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil // packfiles and the like may be unreadable mid-write; not our concern
		}
		if strings.Contains(string(body), token) {
			t.Errorf("the token is stored in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk .git: %v", err)
	}

	// The remote must be the clean URL, ready to be used from the container.
	if got := git(t, dest, "config", "--local", "remote.origin.url"); got != source {
		t.Errorf("remote.origin.url = %q, want %q", got, source)
	}
	// git exits non-zero when the key is unset, which is the outcome we want.
	if out, err := gitTry(dest, "config", "--local", "--get-all", "credential.helper"); err == nil {
		t.Errorf("a credential helper was persisted: %q", out)
	}
}

func TestCloneReportsFailuresWithoutLeakingTheToken(t *testing.T) {
	isolateGit(t)
	dest := filepath.Join(t.TempDir(), "repo")
	const token = "gho_a_very_secret_token"

	err := Clone(context.Background(), Options{
		CloneURL: filepath.Join(t.TempDir(), "does-not-exist"),
		Dest:     dest,
		Token:    token,
	})
	if err == nil {
		t.Fatal("Clone succeeded against a repository that does not exist")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the error carries the token: %v", err)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}

func TestCloneRejectsAnIncompleteRequest(t *testing.T) {
	if err := Clone(context.Background(), Options{Dest: "/tmp/x"}); err == nil {
		t.Error("Clone accepted a request with no URL")
	}
	if err := Clone(context.Background(), Options{CloneURL: "https://example.test/x.git"}); err == nil {
		t.Error("Clone accepted a request with no destination")
	}
}

// deepRepo builds a repository with a history worth truncating: sourceRepo's
// main branch is one commit, which cannot tell a depth of 1 from all of it.
func deepRepo(t *testing.T, commits int) string {
	t.Helper()
	dir := t.TempDir()

	git(t, dir, "init", "--initial-branch=main", dir)
	git(t, dir, "config", "user.name", "Source Author")
	git(t, dir, "config", "user.email", "source@example.test")

	for i := range commits {
		name := fmt.Sprintf("file-%d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		git(t, dir, "add", name)
		git(t, dir, "commit", "-m", "commit "+name)
	}
	return dir
}

// localURL is what a depth test has to clone through. git ignores --depth when
// the source is a path — "--depth is ignored in local clones; use file://
// instead" — so a test that passed the path would pass whatever Clone did.
func localURL(dir string) string { return "file://" + dir }

func TestCloneTruncatesTheHistoryToTheDepth(t *testing.T) {
	isolateGit(t)
	source := deepRepo(t, 3)
	dest := filepath.Join(t.TempDir(), "repo")

	if err := Clone(context.Background(), Options{CloneURL: localURL(source), Depth: 1, Dest: dest}); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	if got := git(t, dest, "rev-list", "--count", "HEAD"); got != "1" {
		t.Errorf("commits in the clone = %s, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git", "shallow")); err != nil {
		t.Errorf("the clone is not shallow: %v", err)
	}
	// The depth is a number, not a switch: two means two.
	deeper := filepath.Join(t.TempDir(), "repo")
	if err := Clone(context.Background(), Options{CloneURL: localURL(source), Depth: 2, Dest: deeper}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if got := git(t, deeper, "rev-list", "--count", "HEAD"); got != "2" {
		t.Errorf("commits in the clone = %s, want 2", got)
	}
}

func TestCloneWithoutADepthKeepsTheWholeHistory(t *testing.T) {
	isolateGit(t)
	source := deepRepo(t, 3)
	dest := filepath.Join(t.TempDir(), "repo")

	if err := Clone(context.Background(), Options{CloneURL: localURL(source), Dest: dest}); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	if got := git(t, dest, "rev-list", "--count", "HEAD"); got != "3" {
		t.Errorf("commits in the clone = %s, want 3 — the zero depth must clone everything", got)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git", "shallow")); !os.IsNotExist(err) {
		t.Errorf("the clone is shallow, and nothing asked for that: %v", err)
	}
}
