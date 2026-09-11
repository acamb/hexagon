package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/store"
)

func testUserForTransfers(t *testing.T, st *store.Store) *store.User {
	t.Helper()
	user, err := st.UpsertUser(context.Background(), &store.User{GitHubLogin: "alice", GitHubID: 1})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

// A row left "running" by a server that stopped mid-job describes work nobody
// is doing, and its file, if any, is incomplete: both are cleared at startup.
func TestFailInterruptedTransfersRemovesTheIncompleteFile(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	user := testUserForTransfers(t, st)

	transfersDir := t.TempDir()
	dir := filepath.Join(transfersDir, "t1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "partial.tar.gz"), []byte("incomplete"), 0o600); err != nil {
		t.Fatalf("write partial file: %v", err)
	}

	now := time.Now()
	created, err := st.CreateTransfer(context.Background(), &store.ImageTransfer{
		ID: "t1", UserID: user.ID, Direction: store.TransferDirectionBackup, ImageID: "img-1", Name: "base",
		Status: store.TransferStatusRunning, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	failInterruptedTransfers(context.Background(), st, transfersDir, slog.New(slog.DiscardHandler))

	got, err := st.TransferByID(context.Background(), user.ID, created.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if got.Status != store.TransferStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("transfer directory still exists after being failed: %v", err)
	}
}

// runTransferJanitor removes an expired transfer's row and file, and leaves a
// transfer whose lifetime is not over alone.
func TestRunTransferJanitorRemovesOnlyExpiredTransfers(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hexagon.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	user := testUserForTransfers(t, st)

	transfersDir := t.TempDir()
	for _, id := range []string{"expired", "fresh"} {
		if err := os.MkdirAll(filepath.Join(transfersDir, id), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	now := time.Now()
	if _, err := st.CreateTransfer(context.Background(), &store.ImageTransfer{
		ID: "expired", UserID: user.ID, Direction: store.TransferDirectionBackup, ImageID: "img-1", Name: "base",
		Status: store.TransferStatusReady, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if _, err := st.CreateTransfer(context.Background(), &store.ImageTransfer{
		ID: "fresh", UserID: user.ID, Direction: store.TransferDirectionBackup, ImageID: "img-1", Name: "base",
		Status: store.TransferStatusReady, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go runTransferJanitor(ctx, st, transfersDir, slog.New(slog.DiscardHandler))
	t.Cleanup(cancel)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := st.TransferByID(context.Background(), user.ID, "expired"); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := st.TransferByID(context.Background(), user.ID, "expired"); err == nil {
		t.Error("the expired transfer row is still there")
	}
	if _, err := os.Stat(filepath.Join(transfersDir, "expired")); !os.IsNotExist(err) {
		t.Errorf("the expired transfer's directory is still there: %v", err)
	}

	if _, err := st.TransferByID(context.Background(), user.ID, "fresh"); err != nil {
		t.Errorf("the fresh transfer was removed too: %v", err)
	}
	if _, err := os.Stat(filepath.Join(transfersDir, "fresh")); err != nil {
		t.Errorf("the fresh transfer's directory was removed: %v", err)
	}
}
