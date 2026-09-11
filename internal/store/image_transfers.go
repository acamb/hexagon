package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Transfer directions.
const (
	TransferDirectionBackup  = "backup"
	TransferDirectionRestore = "restore"
)

// Transfer statuses.
const (
	TransferStatusPending = "pending"
	TransferStatusRunning = "running"
	TransferStatusReady   = "ready"
	TransferStatusFailed  = "failed"
)

// ImageTransfer is an image's backup on its way out of Hexagon, or a restore
// on its way in: a file staged under DataDir, a job that is or is not
// finished with it, and a lifetime after which the janitor removes both. See
// plans/M3/02-image-backup-restore.md.
type ImageTransfer struct {
	ID        string
	UserID    string
	Direction string
	// ImageID is the image a backup was taken from, or the image a restore
	// produced. Empty for a restore that has not been imported yet.
	ImageID string
	Name    string
	Status  string
	// WithImage says whether the archive carries image.tar.gz: asked for, on a
	// backup, or present in the upload, on a restore.
	WithImage bool
	// Path is where the archive is staged under DataDir. Empty until a backup's
	// job produces it; set from the first request, for a restore.
	Path      string
	Size      int64
	Error     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

const imageTransferColumns = `id, user_id, direction, image_id, name, status, with_image, path, size, error, created_at, expires_at`

// CreateTransfer inserts a new transfer row, assigning it an id when it has
// none.
func (s *Store) CreateTransfer(ctx context.Context, t *ImageTransfer) (*ImageTransfer, error) {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO image_transfers (`+imageTransferColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Direction, nullableString(t.ImageID), t.Name, t.Status, t.WithImage,
		t.Path, t.Size, t.Error, formatTime(t.CreatedAt), formatTime(t.ExpiresAt))
	if err != nil {
		return nil, fmt.Errorf("create image transfer: %w", err)
	}
	return t, nil
}

// TransfersByUser returns the user's transfers, newest first: what the
// transfers list polls.
func (s *Store) TransfersByUser(ctx context.Context, userID string) ([]*ImageTransfer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+imageTransferColumns+` FROM image_transfers WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list image transfers: %w", err)
	}
	defer rows.Close()

	transfers := []*ImageTransfer{}
	for rows.Next() {
		t, err := scanImageTransfer(rows)
		if err != nil {
			return nil, err
		}
		transfers = append(transfers, t)
	}
	return transfers, rows.Err()
}

// TransferByID returns one of the user's transfers, or ErrNotFound. Scoping the
// lookup to the owner means a wrong id and someone else's id look the same.
func (s *Store) TransferByID(ctx context.Context, userID, id string) (*ImageTransfer, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+imageTransferColumns+` FROM image_transfers WHERE id = ? AND user_id = ?`, id, userID)
	t, err := scanImageTransfer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// FinishTransfer records the outcome of a transfer's job: a backup archive
// completing or failing in the background, or a restore's import linking the
// transfer to the image it produced. imageID left empty leaves the column
// alone, which is how a failed import leaves a restore's row pointing at
// nothing.
func (s *Store) FinishTransfer(ctx context.Context, id, status, imageID, path string, size int64, errMessage string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE image_transfers
		SET status = ?, image_id = COALESCE(?, image_id), path = ?, size = ?, error = ?
		WHERE id = ?`,
		status, nullableString(imageID), path, size, errMessage, id)
	if err != nil {
		return fmt.Errorf("finish image transfer: %w", err)
	}
	return nil
}

// DeleteTransfer removes one of the user's transfer rows. It returns
// ErrNotFound when there is nothing to delete; the caller owns removing the
// file at Path.
func (s *Store) DeleteTransfer(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM image_transfers WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete image transfer: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailInterruptedTransfers marks transfers that were pending or running when
// the server stopped as failed, and returns the rows that were moved so the
// caller can remove the incomplete file each one left behind.
func (s *Store) FailInterruptedTransfers(ctx context.Context) ([]*ImageTransfer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+imageTransferColumns+` FROM image_transfers WHERE status IN (?, ?)`,
		TransferStatusPending, TransferStatusRunning)
	if err != nil {
		return nil, fmt.Errorf("list interrupted transfers: %w", err)
	}
	interrupted := []*ImageTransfer{}
	for rows.Next() {
		t, err := scanImageTransfer(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		interrupted = append(interrupted, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if len(interrupted) == 0 {
		return interrupted, nil
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE image_transfers SET status = ?, error = ? WHERE status IN (?, ?)`,
		TransferStatusFailed, "interrupted by a server restart", TransferStatusPending, TransferStatusRunning); err != nil {
		return nil, fmt.Errorf("fail interrupted transfers: %w", err)
	}
	return interrupted, nil
}

// ExpiredTransfers returns every transfer whose lifetime is over, across every
// user: the janitor that removes them runs independently of who is looking at
// the page.
func (s *Store) ExpiredTransfers(ctx context.Context, now time.Time) ([]*ImageTransfer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+imageTransferColumns+` FROM image_transfers WHERE expires_at <= ?`, formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("list expired transfers: %w", err)
	}
	defer rows.Close()

	transfers := []*ImageTransfer{}
	for rows.Next() {
		t, err := scanImageTransfer(rows)
		if err != nil {
			return nil, err
		}
		transfers = append(transfers, t)
	}
	return transfers, rows.Err()
}

func scanImageTransfer(row scanner) (*ImageTransfer, error) {
	var (
		t                    ImageTransfer
		imageID              sql.NullString
		createdAt, expiresAt string
	)
	err := row.Scan(&t.ID, &t.UserID, &t.Direction, &imageID, &t.Name, &t.Status, &t.WithImage,
		&t.Path, &t.Size, &t.Error, &createdAt, &expiresAt)
	if err != nil {
		return nil, err
	}
	t.ImageID = imageID.String
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if t.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}
	return &t, nil
}

// nullableString turns an empty string into a NULL, which is what image_id
// means before a restore has been imported.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
