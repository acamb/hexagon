package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrationsAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexagon.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var version int
	if err := s.DB().QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if want := len(mustLoadMigrations(t)); version != want {
		t.Errorf("schema version = %d, want %d", version, want)
	}

	for _, table := range []string{"users", "user_sessions", "images", "sessions"} {
		var name string
		err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must not try to re-apply anything.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	var applied int
	if err := s2.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != version {
		t.Errorf("applied migrations = %d, want %d", applied, version)
	}
}

func TestOpenCreatesDatabaseWithRestrictivePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "hexagon.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("database permissions = %04o, want 0600", got)
	}
}

func TestLoadMigrationsAreOrderedAndUnique(t *testing.T) {
	migrations := mustLoadMigrations(t)
	if len(migrations) == 0 {
		t.Fatal("no migrations embedded")
	}
	for i, m := range migrations {
		if m.version != i+1 {
			t.Errorf("migration %s has version %d, want %d: versions must be contiguous from 1", m.name, m.version, i+1)
		}
		if m.sql == "" {
			t.Errorf("migration %s is empty", m.name)
		}
	}
}

func mustLoadMigrations(t *testing.T) []migration {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	return migrations
}
