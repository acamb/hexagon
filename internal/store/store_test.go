package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// Migration 007 rebuilds the images table, which is the one thing a migration
// here has never had to do: SQLite cannot alter a CHECK constraint. Rebuilding a
// table other rows point at is where data goes missing, so this walks a database
// up to the version before it, fills it, and then finishes the job.
func TestRebuildingImagesKeepsTheRowsAndTheForeignKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexagon.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	s := &Store{db: db}

	// The version the compose columns arrive on top of.
	const before = 6
	applyThrough(t, s, before)

	ctx := t.Context()
	user, err := s.UpsertUser(ctx, &User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO images (id, user_id, name, source_type, dockerfile, image_ref, status, created_at)
		VALUES ('i-1', ?, 'base', 'dockerfile', 'FROM busybox', 'hexagon/img-1:latest', 'ready', ?)`,
		user.ID, formatTime(time.Now())); err != nil {
		t.Fatalf("insert image: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, title, provider, repo_full_name, repo_clone_url, branch,
			image_id, image_ref, workspace_dir, repo_dir, container_id, status, error, created_at, updated_at)
		VALUES ('s-1', ?, 'work', 'github', 'acme/widgets', 'https://example.test/x.git', 'main',
			'i-1', 'hexagon/img-1:latest', '/w/s-1', '/w/s-1/repo', 'c-1', 'running', '', ?, ?)`,
		user.ID, formatTime(time.Now()), formatTime(time.Now())); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	img, err := s.ImageByID(ctx, user.ID, "i-1")
	if err != nil {
		t.Fatalf("the image did not survive the rebuild: %v", err)
	}
	if img.Name != "base" || img.Dockerfile != "FROM busybox" || img.ImageRef != "hexagon/img-1:latest" {
		t.Errorf("image = %+v, want the row as it was", img)
	}
	if img.Compose != "" {
		t.Errorf("compose = %q, want empty on a row that predates the column", img.Compose)
	}

	session, err := s.SessionByID(ctx, user.ID, "s-1")
	if err != nil {
		t.Fatalf("the session did not survive the rebuild: %v", err)
	}
	if session.ImageID != "i-1" || session.Compose || len(session.Ports) != 0 {
		t.Errorf("session = %+v, want its image and the new columns at their defaults", session)
	}

	// The point of the rebuild: a third source type is now allowed, and the
	// foreign key sessions.image_id still holds.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO images (id, user_id, name, source_type, dockerfile, compose, status, created_at)
		VALUES ('i-2', ?, 'advanced', 'compose', 'FROM busybox', 'services: {}', 'ready', ?)`,
		user.ID, formatTime(time.Now())); err != nil {
		t.Errorf("a compose image was refused after the rebuild: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET image_id = 'nothing' WHERE id = 's-1'`); err == nil {
		t.Error("sessions.image_id no longer references images: the rebuild lost the foreign key")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM images WHERE id = 'i-1'`); err == nil {
		t.Error("an image still in use could be deleted: the rebuild lost the foreign key")
	}
}

// applyThrough runs the migrations up to and including version, the way an
// older Hexagon would have left the database.
func applyThrough(t *testing.T, s *Store, version int) {
	t.Helper()
	ctx := t.Context()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("open a connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range mustLoadMigrations(t) {
		if m.version > version {
			break
		}
		if err := apply(ctx, conn, m); err != nil {
			t.Fatalf("migration %s: %v", m.name, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("re-enable foreign keys: %v", err)
	}
}
