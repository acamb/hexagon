package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Image source types.
const (
	ImageSourceDockerfile = "dockerfile"
	ImageSourceRegistry   = "registry"
	// ImageSourceCompose is a Dockerfile and a compose file together: the
	// Dockerfile still describes the container Claude Code runs in, and the
	// compose file the services that have to be there beside it. It builds
	// exactly as ImageSourceDockerfile does — the compose file is a
	// session-time concern.
	ImageSourceCompose = "compose"
)

// Image statuses.
const (
	ImageStatusPending  = "pending"
	ImageStatusBuilding = "building"
	ImageStatusReady    = "ready"
	ImageStatusFailed   = "failed"
)

// Image is a base image sessions can be started from: built here from a
// Dockerfile, pulled from a registry, or a Dockerfile with a compose file
// naming the services a session from it also needs.
type Image struct {
	ID         string
	UserID     string
	Name       string
	SourceType string
	Dockerfile string
	// Compose is the user's compose file, for a compose image. It describes the
	// services beside the agent; the agent's own service is rendered by
	// internal/session and never stored here.
	Compose string
	// RegistryRef is what the user asked to pull, for registry images.
	RegistryRef string
	// ImageRef is the local reference sessions run from, known once the image
	// is ready.
	ImageRef  string
	Status    string
	BuildLog  string
	Error     string
	CreatedAt time.Time
}

const imageColumns = `id, user_id, name, source_type, dockerfile, compose, registry_ref, image_ref, status, build_log, error, created_at`

// CreateImage inserts a new image, assigning it an id. It returns ErrConflict
// if the user already has an image with that name.
func (s *Store) CreateImage(ctx context.Context, img *Image) (*Image, error) {
	if img.ID == "" {
		img.ID = uuid.NewString()
	}
	img.CreatedAt = time.Now()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO images (`+imageColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		img.ID, img.UserID, img.Name, img.SourceType, img.Dockerfile, img.Compose, img.RegistryRef,
		img.ImageRef, img.Status, img.BuildLog, img.Error, formatTime(img.CreatedAt))
	switch {
	case isUniqueViolation(err):
		return nil, ErrConflict
	case err != nil:
		return nil, fmt.Errorf("create image: %w", err)
	}
	return img, nil
}

// ListImages returns the user's images, newest first.
func (s *Store) ListImages(ctx context.Context, userID string) ([]*Image, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+imageColumns+` FROM images WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	defer rows.Close()

	images := []*Image{}
	for rows.Next() {
		img, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, rows.Err()
}

// ImageByID returns one of the user's images, or ErrNotFound. Scoping the
// lookup to the owner means a wrong id and someone else's id look the same.
func (s *Store) ImageByID(ctx context.Context, userID, id string) (*Image, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+imageColumns+` FROM images WHERE id = ? AND user_id = ?`, id, userID)
	img, err := scanImage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return img, err
}

// SetImageBuildLog stores the output produced so far, so the UI can follow a
// build while it runs.
func (s *Store) SetImageBuildLog(ctx context.Context, id, log string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE images SET build_log = ? WHERE id = ?`, log, id); err != nil {
		return fmt.Errorf("update build log: %w", err)
	}
	return nil
}

// FinishImage records the outcome of a build or pull.
func (s *Store) FinishImage(ctx context.Context, id, status, imageRef, log, errMessage string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE images SET status = ?, image_ref = ?, build_log = ?, error = ? WHERE id = ?`,
		status, imageRef, log, errMessage, id)
	if err != nil {
		return fmt.Errorf("finish image: %w", err)
	}
	return nil
}

// FailInterruptedImageBuilds marks builds that were running when the server
// stopped. Nothing is going to finish them, so leaving them "building" would
// show a spinner that never resolves.
func (s *Store) FailInterruptedImageBuilds(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE images SET status = ?, error = ? WHERE status = ?`,
		ImageStatusFailed, "interrupted by a server restart", ImageStatusBuilding)
	if err != nil {
		return 0, fmt.Errorf("fail interrupted image builds: %w", err)
	}
	return res.RowsAffected()
}

// DeleteImage removes one of the user's images. It returns ErrNotFound when
// there is nothing to delete.
func (s *Store) DeleteImage(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM images WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete image: %w", err)
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

// CountSessionsUsingImage reports how many sessions still reference an image.
func (s *Store) CountSessionsUsingImage(ctx context.Context, imageID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE image_id = ?`, imageID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count sessions using image: %w", err)
	}
	return n, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanImage(row scanner) (*Image, error) {
	var (
		img       Image
		createdAt string
	)
	err := row.Scan(&img.ID, &img.UserID, &img.Name, &img.SourceType, &img.Dockerfile, &img.Compose,
		&img.RegistryRef, &img.ImageRef, &img.Status, &img.BuildLog, &img.Error, &createdAt)
	if err != nil {
		return nil, err
	}
	if img.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	return &img, nil
}

// CountBuildingImages reports how many of a user's builds are in flight. A
// build runs for as long as its Dockerfile takes and pulls whatever that
// Dockerfile names, so the number of them at once is worth bounding.
func (s *Store) CountBuildingImages(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM images WHERE user_id = ? AND status IN (?, ?)`,
		userID, ImageStatusPending, ImageStatusBuilding).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count building images: %w", err)
	}
	return n, nil
}
