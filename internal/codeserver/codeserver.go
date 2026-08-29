// Package codeserver keeps one code-server release on the host, so a session
// can run VS Code in the browser without every image carrying a copy of it.
package codeserver

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// DefaultVersion is the code-server release fetched when none is configured.
// Verified to exist and to ship code-server-4.135.0-linux-<arch>/bin/code-server
// and a bundled lib/node, so the container needs no Node of its own.
const DefaultVersion = "4.135.0"

// defaultBaseURL is where code-server publishes its releases.
const defaultBaseURL = "https://github.com/coder/code-server/releases/download"

// Config is where the release lives and which one to fetch if it is not there.
type Config struct {
	Dir     string // install root: <Dir>/bin/code-server is what runs
	Version string // release to download, without the leading v
	BaseURL string // release download root; empty means the project's own
}

// Source hands out the directory holding the release.
type Source struct {
	cfg Config
	mu  sync.Mutex
}

// New builds a Source over cfg.
func New(cfg Config) *Source {
	return &Source{cfg: cfg}
}

// binaryPath is where a usable install's code-server binary lives.
func (s *Source) binaryPath() string {
	return filepath.Join(s.cfg.Dir, "bin", "code-server")
}

// Ensure returns the directory holding a usable code-server, downloading the
// configured release the first time. It is safe to call concurrently.
//
// A directory that already holds an install is never deleted, overwritten or
// version-checked: that is what makes a hand-placed release work on a machine
// with no route to GitHub, and what makes upgrading an explicit act, deleting
// the directory, rather than something this method would ever do on its own.
func (s *Source) Ensure(ctx context.Context) (string, error) {
	if _, err := os.Stat(s.binaryPath()); err == nil {
		return s.cfg.Dir, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Another call may have finished the download while this one waited for
	// the lock.
	if _, err := os.Stat(s.binaryPath()); err == nil {
		return s.cfg.Dir, nil
	}

	if _, err := exec.LookPath("tar"); err != nil {
		return "", fmt.Errorf("code-server: tar is required to extract the release: %w", err)
	}

	arch, err := releaseArch()
	if err != nil {
		return "", err
	}
	version := s.cfg.Version
	if version == "" {
		version = DefaultVersion
	}
	baseURL := s.cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	url := fmt.Sprintf("%s/v%s/code-server-%s-linux-%s.tar.gz", baseURL, version, version, arch)

	if err := s.download(ctx, url); err != nil {
		return "", err
	}
	return s.cfg.Dir, nil
}

// releaseArch maps the Go architecture to the one code-server's release names
// use. Anything else is not published, so it is an error naming what was
// asked for rather than a guess.
func releaseArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH, nil
	default:
		return "", fmt.Errorf("code-server: no release published for %s", runtime.GOARCH)
	}
}

// download fetches and extracts the release into <Dir>.partial, then renames it
// onto Dir, so a download that is killed midway never leaves a half-extracted
// tree that Ensure would mistake for an install.
func (s *Source) download(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("code-server: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("code-server: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("code-server: download %s: %s", url, resp.Status)
	}

	partial := s.cfg.Dir + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return fmt.Errorf("code-server: clear %s: %w", partial, err)
	}
	if err := os.MkdirAll(partial, 0o700); err != nil {
		return fmt.Errorf("code-server: create %s: %w", partial, err)
	}

	cmd := exec.CommandContext(ctx, "tar", "-xz", "--strip-components=1", "-C", partial)
	cmd.Stdin = resp.Body
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("code-server: extract release: %w: %s", err, output)
	}

	binary := filepath.Join(partial, "bin", "code-server")
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("code-server: extracted release has no %s: %w", binary, err)
	}

	if err := os.Rename(partial, s.cfg.Dir); err != nil {
		return fmt.Errorf("code-server: install to %s: %w", s.cfg.Dir, err)
	}
	return nil
}
