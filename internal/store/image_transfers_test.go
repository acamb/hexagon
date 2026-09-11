package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testTransfer(userID string) *ImageTransfer {
	now := time.Now()
	return &ImageTransfer{
		UserID:    userID,
		Direction: TransferDirectionBackup,
		ImageID:   "img-1",
		Name:      "base",
		Status:    TransferStatusRunning,
		WithImage: true,
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
	}
}

func TestCreateTransferAssignsAnID(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	created, err := s.CreateTransfer(ctx, testTransfer(user.ID))
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateTransfer returned no id")
	}

	got, err := s.TransferByID(ctx, user.ID, created.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if got.Name != "base" || got.ImageID != "img-1" || got.Status != TransferStatusRunning || !got.WithImage {
		t.Errorf("stored transfer = %+v", got)
	}
}

// A restore's transfer starts with no image, because nothing has been
// imported yet.
func TestCreateTransferAllowsNoImage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	tr := testTransfer(user.ID)
	tr.Direction = TransferDirectionRestore
	tr.ImageID = ""

	created, err := s.CreateTransfer(ctx, tr)
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	got, err := s.TransferByID(ctx, user.ID, created.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if got.ImageID != "" {
		t.Errorf("image id = %q, want empty until an import links it", got.ImageID)
	}
}

func TestFinishTransferRecordsTheOutcome(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	created, err := s.CreateTransfer(ctx, testTransfer(user.ID))
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	if err := s.FinishTransfer(ctx, created.ID, TransferStatusReady, "", "/data/transfers/x/backup.tar.gz", 1024, ""); err != nil {
		t.Fatalf("FinishTransfer: %v", err)
	}
	got, err := s.TransferByID(ctx, user.ID, created.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if got.Status != TransferStatusReady || got.Path != "/data/transfers/x/backup.tar.gz" || got.Size != 1024 {
		t.Errorf("finished transfer = %+v", got)
	}
	// image_id was left alone: an empty imageID must not erase what CreateTransfer set.
	if got.ImageID != "img-1" {
		t.Errorf("image id = %q, want it left untouched", got.ImageID)
	}
}

// A restore's import links the transfer to the image it produced.
func TestFinishTransferLinksAnImportedImage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	tr := testTransfer(user.ID)
	tr.Direction, tr.ImageID = TransferDirectionRestore, ""
	created, err := s.CreateTransfer(ctx, tr)
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	if err := s.FinishTransfer(ctx, created.ID, TransferStatusReady, "img-imported", created.Path, created.Size, ""); err != nil {
		t.Fatalf("FinishTransfer: %v", err)
	}
	got, err := s.TransferByID(ctx, user.ID, created.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if got.ImageID != "img-imported" {
		t.Errorf("image id = %q, want the imported image linked", got.ImageID)
	}
}

func TestDeleteTransfer(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	created, err := s.CreateTransfer(ctx, testTransfer(user.ID))
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if err := s.DeleteTransfer(ctx, user.ID, created.ID); err != nil {
		t.Fatalf("DeleteTransfer: %v", err)
	}
	if _, err := s.TransferByID(ctx, user.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("TransferByID after delete: err = %v, want ErrNotFound", err)
	}
}

func TestTransfersAreScopedToTheirOwner(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	alice := testUser(t, s, "alice", 1)
	bob := testUser(t, s, "bob", 2)

	if _, err := s.CreateTransfer(ctx, testTransfer(alice.ID)); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if _, err := s.CreateTransfer(ctx, testTransfer(bob.ID)); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	mine, err := s.TransfersByUser(ctx, alice.ID)
	if err != nil {
		t.Fatalf("TransfersByUser: %v", err)
	}
	if len(mine) != 1 {
		t.Errorf("listed %d transfers, want only the caller's", len(mine))
	}

	if _, err := s.TransferByID(ctx, alice.ID, mine[0].ID+"nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("lookup of a foreign id: err = %v, want ErrNotFound", err)
	}
}

// FailInterruptedTransfers moves pending and running rows to failed and leaves
// finished ones alone, the same rule FailInterruptedImageBuilds follows for
// builds.
func TestFailInterruptedTransfersMovesRunningRowsToFailed(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	running, err := s.CreateTransfer(ctx, testTransfer(user.ID))
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	ready := testTransfer(user.ID)
	ready.Status = TransferStatusReady
	readyCreated, err := s.CreateTransfer(ctx, ready)
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	interrupted, err := s.FailInterruptedTransfers(ctx)
	if err != nil {
		t.Fatalf("FailInterruptedTransfers: %v", err)
	}
	if len(interrupted) != 1 || interrupted[0].ID != running.ID {
		t.Errorf("interrupted = %+v, want just the running transfer", interrupted)
	}

	got, err := s.TransferByID(ctx, user.ID, running.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if got.Status != TransferStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}

	stillReady, err := s.TransferByID(ctx, user.ID, readyCreated.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if stillReady.Status != TransferStatusReady {
		t.Errorf("a finished transfer's status changed to %q", stillReady.Status)
	}
}

// ExpiredTransfers selects on expires_at using formatTime, which is the test
// that says the fixed-width UTC layout still orders correctly as a string.
func TestExpiredTransfersSelectsOnExpiresAt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	past := testTransfer(user.ID)
	past.ExpiresAt = time.Now().Add(-time.Hour)
	expired, err := s.CreateTransfer(ctx, past)
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	future := testTransfer(user.ID)
	future.ExpiresAt = time.Now().Add(time.Hour)
	if _, err := s.CreateTransfer(ctx, future); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	got, err := s.ExpiredTransfers(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpiredTransfers: %v", err)
	}
	if len(got) != 1 || got[0].ID != expired.ID {
		t.Errorf("expired = %+v, want just the one whose lifetime is over", got)
	}
}
