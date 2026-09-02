package session

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/andrea/hexagon/internal/dockerx"
)

// loginFakeDocker is just enough of dockerx.API for the login container: create,
// start and remove. Anything else panics, which is the right outcome for a call
// these tests do not expect.
type loginFakeDocker struct {
	dockerx.API
	mu         sync.Mutex
	containers map[string]dockerx.ContainerSpec
	removed    []string
	createErr  error
}

func (f *loginFakeDocker) CreateContainer(_ context.Context, spec dockerx.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.containers == nil {
		f.containers = map[string]dockerx.ContainerSpec{}
	}
	f.containers[spec.Name] = spec
	return "container-" + spec.Name, nil
}

func (f *loginFakeDocker) StartContainer(context.Context, string) error { return nil }

func (f *loginFakeDocker) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func TestStartClaudeLoginBindsTheCredentialsDirectoryReadWrite(t *testing.T) {
	docker := &loginFakeDocker{}
	manager, _, _ := testManager(t, docker)
	dataDir := t.TempDir()
	credentialsPath := filepath.Join(dataDir, "claude", ".credentials.json")
	manager.cfg.ClaudeCredentials = credentialsPath
	manager.cfg.ClaudeLoginDir = filepath.Join(dataDir, "claude-login")

	containerID, err := manager.StartClaudeLogin(context.Background(), "user-1", "image-ref")
	if err != nil {
		t.Fatalf("StartClaudeLogin: %v", err)
	}
	if containerID == "" {
		t.Fatal("no container id returned")
	}

	spec, ok := docker.containers["hexagon-claude-login-user-1"]
	if !ok {
		t.Fatalf("no container created with the expected name; got %v", docker.containers)
	}
	wantBind := filepath.Dir(credentialsPath) + ":" + dockerx.AgentHome + "/.claude"
	if !slices.Contains(spec.Binds, wantBind) {
		t.Errorf("binds = %v, want %q", spec.Binds, wantBind)
	}
	// A container that already had an Anthropic variable would consider itself
	// authenticated, and the login the user came here for would be theatre
	// performed on the credential they are replacing.
	for _, e := range spec.Env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") || strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Errorf("login container env carried an anthropic credential: %v", spec.Env)
		}
	}
	if spec.Labels[dockerx.LabelManaged] != "" {
		t.Errorf("login container carries hexagon.managed, so the reconciler would report it as an orphan")
	}

	if err := manager.StopClaudeLogin(context.Background(), "user-1"); err != nil {
		t.Fatalf("StopClaudeLogin: %v", err)
	}
	if !slices.Contains(docker.removed, "hexagon-claude-login-user-1") {
		t.Errorf("removed = %v, want the login container", docker.removed)
	}
}

func TestStartClaudeLoginRefusesWithNoCredentialsPathConfigured(t *testing.T) {
	manager, _, _ := testManager(t, &loginFakeDocker{})
	if _, err := manager.StartClaudeLogin(context.Background(), "user-1", "image-ref"); !errors.Is(err, ErrClaudeLoginUnavailable) {
		t.Errorf("error = %v, want ErrClaudeLoginUnavailable", err)
	}
}

// An account's login binds its own directory read-write rather than the
// machine-wide one, which is the whole mechanism that lets two accounts sign
// in at once without either overwriting the other's credential.
func TestStartAccountClaudeLoginBindsTheAccountsOwnDirectory(t *testing.T) {
	docker := &loginFakeDocker{}
	manager, _, _ := testManager(t, docker)
	dataDir := t.TempDir()
	manager.cfg.ClaudeLoginDir = filepath.Join(dataDir, "claude-login")
	manager.cfg.ClaudeAccountsDir = filepath.Join(dataDir, "claude-accounts")

	containerID, err := manager.StartAccountClaudeLogin(context.Background(), "acc-1", "image-ref")
	if err != nil {
		t.Fatalf("StartAccountClaudeLogin: %v", err)
	}
	if containerID == "" {
		t.Fatal("no container id returned")
	}

	spec, ok := docker.containers["hexagon-claude-login-acc-1"]
	if !ok {
		t.Fatalf("no container created with the expected name; got %v", docker.containers)
	}
	wantBind := filepath.Join(manager.cfg.ClaudeAccountsDir, "acc-1") + ":" + dockerx.AgentHome + "/.claude"
	if !slices.Contains(spec.Binds, wantBind) {
		t.Errorf("binds = %v, want %q", spec.Binds, wantBind)
	}
	for _, e := range spec.Env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") || strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Errorf("login container env carried an anthropic credential: %v", spec.Env)
		}
	}

	// A second account's login binds a different directory, so signing in to
	// both at once does not make either overwrite the other.
	if _, err := manager.StartAccountClaudeLogin(context.Background(), "acc-2", "image-ref"); err != nil {
		t.Fatalf("StartAccountClaudeLogin for a second account: %v", err)
	}
	other := docker.containers["hexagon-claude-login-acc-2"]
	wantOtherBind := filepath.Join(manager.cfg.ClaudeAccountsDir, "acc-2") + ":" + dockerx.AgentHome + "/.claude"
	if !slices.Contains(other.Binds, wantOtherBind) {
		t.Errorf("binds = %v, want %q", other.Binds, wantOtherBind)
	}

	if err := manager.StopAccountClaudeLogin(context.Background(), "acc-1"); err != nil {
		t.Fatalf("StopAccountClaudeLogin: %v", err)
	}
	if !slices.Contains(docker.removed, "hexagon-claude-login-acc-1") {
		t.Errorf("removed = %v, want the login container", docker.removed)
	}
}
