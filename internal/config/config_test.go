package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGeneratesAndReusesSecretKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HEXAGON_DATA_DIR", dir)
	t.Setenv("HEXAGON_SECRET_KEY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.SecretKey) != secretKeyLen {
		t.Fatalf("secret key length = %d, want %d", len(cfg.SecretKey), secretKeyLen)
	}

	info, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("stat secret.key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("secret.key permissions = %04o, want 0600", got)
	}

	// A second run must reuse the key, otherwise every restart would invalidate
	// the stored GitHub tokens.
	again, err := Load()
	if err != nil {
		t.Fatalf("Load again: %v", err)
	}
	if string(again.SecretKey) != string(cfg.SecretKey) {
		t.Error("secret key changed across restarts")
	}
}

func TestLoadRejectsMalformedSecretKey(t *testing.T) {
	t.Setenv("HEXAGON_DATA_DIR", t.TempDir())
	t.Setenv("HEXAGON_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte("too short")))

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a secret key of the wrong length")
	}
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HEXAGON_DATA_DIR", dir)
	t.Setenv("HEXAGON_ADDR", "")
	t.Setenv("HEXAGON_WORKSPACE_ROOT", "")
	t.Setenv("HEXAGON_ALLOWED_USERS", " alice , , bob ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("Addr = %q, want the loopback default", cfg.Addr)
	}
	if want := filepath.Join(dir, "workspaces"); cfg.WorkspaceRoot != want {
		t.Errorf("WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, want)
	}
	if got := cfg.AllowedUsers; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("AllowedUsers = %q, want [alice bob]", got)
	}
}
