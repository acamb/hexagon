package httpapi

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/backup"
	"github.com/andrea/hexagon/internal/store"
)

// transferLifetime is how long a staged transfer — a finished backup nobody
// has downloaded yet, or a restore upload nobody has imported — is kept before
// the janitor removes it. A constant rather than a setting: an operator who
// wants the file kept has already downloaded it, and the machine this runs on
// is somebody's laptop, where a multi-gigabyte file left around forever is a
// real cost.
const transferLifetime = 24 * time.Hour

// backupTimeout bounds a backup job, the same way imageBuildTimeout bounds a
// build: `docker save` of a large development image over a slow disk is the
// case to accommodate.
const backupTimeout = 45 * time.Minute

type transferResponse struct {
	ID        string    `json:"id"`
	Direction string    `json:"direction"`
	ImageID   string    `json:"imageId,omitempty"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	WithImage bool      `json:"withImage"`
	Size      int64     `json:"size,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func newTransferResponse(t *store.ImageTransfer) transferResponse {
	return transferResponse{
		ID:        t.ID,
		Direction: t.Direction,
		ImageID:   t.ImageID,
		Name:      t.Name,
		Status:    t.Status,
		WithImage: t.WithImage,
		Size:      t.Size,
		Error:     t.Error,
		CreatedAt: t.CreatedAt,
		ExpiresAt: t.ExpiresAt,
	}
}

// backupRequest is what to save, selected independently: the Dockerfile and
// compose file, and the image export. At least one is required — a backup of
// neither has nothing in it.
type backupRequest struct {
	WithSpec  bool `json:"withSpec"`
	WithImage bool `json:"withImage"`
}

// handleCreateBackup starts backing up one of the caller's images. The
// request returns as soon as the transfer row exists; progress is followed
// through the transfers list.
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	img, ok := s.imageOr404(w, r)
	if !ok {
		return
	}

	var req backupRequest
	if err := decodeJSON(w, r, maxImageRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if !req.WithSpec && !req.WithImage {
		writeError(w, http.StatusBadRequest, "select at least one of the Dockerfile/compose or the image export")
		return
	}
	if req.WithImage && img.Status != store.ImageStatusReady {
		writeError(w, http.StatusConflict, "the image is not ready yet: there is nothing to export")
		return
	}

	now := time.Now()
	created, err := s.store.CreateTransfer(r.Context(), &store.ImageTransfer{
		UserID:    img.UserID,
		Direction: store.TransferDirectionBackup,
		ImageID:   img.ID,
		Name:      img.Name,
		Status:    store.TransferStatusRunning,
		WithImage: req.WithImage,
		CreatedAt: now,
		ExpiresAt: now.Add(transferLifetime),
	})
	if err != nil {
		s.log.Error("create image transfer", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot start backup")
		return
	}

	s.startBackup(created, img, req.WithSpec)
	writeJSON(w, http.StatusAccepted, newTransferResponse(created))
}

// startBackup builds the archive in the background, under its own timeout and
// its own context: the browser navigating away must not cancel a job that can
// run for many minutes. Any failure removes the transfer's directory — a
// failed job leaves no file behind.
func (s *Server) startBackup(t *store.ImageTransfer, img *store.Image, withSpec bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
		defer cancel()

		path, size, err := s.buildBackupArchive(ctx, t, img, withSpec)
		finishCtx, finishCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer finishCancel()

		if err != nil {
			s.log.Error("image backup failed", "id", t.ID, "name", img.Name, "err", err)
			if rmErr := os.RemoveAll(filepath.Join(s.cfg.TransfersDir, t.ID)); rmErr != nil {
				s.log.Warn("remove failed backup directory", "id", t.ID, "err", rmErr)
			}
			if ferr := s.store.FinishTransfer(finishCtx, t.ID, store.TransferStatusFailed, "", "", 0, err.Error()); ferr != nil {
				s.log.Error("record failed backup", "id", t.ID, "err", ferr)
			}
			return
		}

		s.log.Info("image backup ready", "id", t.ID, "name", img.Name, "size", size)
		if err := s.store.FinishTransfer(finishCtx, t.ID, store.TransferStatusReady, "", path, size, ""); err != nil {
			s.log.Error("record finished backup", "id", t.ID, "err", err)
		}
	}()
}

// buildBackupArchive is startBackup's body, split out so every early return
// still goes through the same failure handling.
func (s *Server) buildBackupArchive(ctx context.Context, t *store.ImageTransfer, img *store.Image, withSpec bool) (path string, size int64, err error) {
	dir := filepath.Join(s.cfg.TransfersDir, t.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("create transfer directory: %w", err)
	}

	var (
		imageReader io.Reader
		imageSize   int64
	)
	if t.WithImage {
		tmpPath := filepath.Join(dir, "image.tar.gz.tmp")
		f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return "", 0, fmt.Errorf("open temporary image file: %w", err)
		}
		gz := gzip.NewWriter(f)
		saveErr := s.docker.SaveImage(ctx, img.ImageRef, gz)
		closeErr := gz.Close()
		fCloseErr := f.Close()
		defer os.Remove(tmpPath)
		switch {
		case saveErr != nil:
			return "", 0, fmt.Errorf("save image: %w", saveErr)
		case closeErr != nil:
			return "", 0, fmt.Errorf("compress image: %w", closeErr)
		case fCloseErr != nil:
			return "", 0, fmt.Errorf("write image: %w", fCloseErr)
		}

		info, err := os.Stat(tmpPath)
		if err != nil {
			return "", 0, fmt.Errorf("stat temporary image file: %w", err)
		}
		imageSize = info.Size()

		rf, err := os.Open(tmpPath)
		if err != nil {
			return "", 0, fmt.Errorf("reopen temporary image file: %w", err)
		}
		defer rf.Close()
		imageReader = rf
	}

	spec := backup.Spec{
		Manifest: backup.Manifest{
			FormatVersion: backup.FormatVersion,
			Name:          img.Name,
			SourceType:    img.SourceType,
			RegistryRef:   img.RegistryRef,
			ImageRef:      img.ImageRef,
		},
	}
	// The manifest is always written — a restore cannot work without it — but
	// the Dockerfile and compose content it names are only included when asked
	// for.
	if withSpec {
		spec.Dockerfile = img.Dockerfile
		spec.Compose = img.Compose
	}

	archivePath := filepath.Join(dir, backupFilename(img.Name, t.CreatedAt))
	out, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("create archive: %w", err)
	}
	writeErr := backup.WriteArchive(out, spec, imageReader, imageSize)
	closeErr := out.Close()
	if writeErr != nil {
		return "", 0, fmt.Errorf("write archive: %w", writeErr)
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("close archive: %w", closeErr)
	}

	info, err := os.Stat(archivePath)
	if err != nil {
		return "", 0, fmt.Errorf("stat archive: %w", err)
	}
	return archivePath, info.Size(), nil
}

// backupFilename is the name the download carries: hexagon-<name>-<timestamp>.
// A space in an image's name is the one character its own validation allows
// that does not belong in a filename people will download and untar by hand.
func backupFilename(name string, at time.Time) string {
	safe := strings.Map(func(r rune) rune {
		if r == ' ' {
			return '-'
		}
		return r
	}, name)
	return fmt.Sprintf("hexagon-%s-%s.tar.gz", safe, at.UTC().Format("20060102-150405"))
}

// handleListTransfers returns the caller's backups and restores, newest
// first: what the transfers list polls.
func (s *Server) handleListTransfers(w http.ResponseWriter, r *http.Request) {
	transfers, err := s.store.TransfersByUser(r.Context(), s.user(r).ID)
	if err != nil {
		s.log.Error("list image transfers", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot list transfers")
		return
	}
	out := make([]transferResponse, 0, len(transfers))
	for _, t := range transfers {
		out = append(out, newTransferResponse(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetTransfer returns one transfer.
func (s *Server) handleGetTransfer(w http.ResponseWriter, r *http.Request) {
	t, ok := s.transferOr404(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newTransferResponse(t))
}

// handleDownloadTransfer serves a finished backup's archive. This is the
// codebase's first file download: ServeContent gets Content-Length and range
// requests for free, so an interrupted download can resume, and the browser
// side is a plain `<a download>` rather than a fetch into memory.
func (s *Server) handleDownloadTransfer(w http.ResponseWriter, r *http.Request) {
	t, ok := s.transferOr404(w, r)
	if !ok {
		return
	}
	if t.Status != store.TransferStatusReady || t.Path == "" {
		writeError(w, http.StatusConflict, "this backup is not ready yet")
		return
	}

	f, err := os.Open(t.Path)
	if err != nil {
		s.log.Error("open transfer file", "id", t.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read the backup file")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.log.Error("stat transfer file", "id", t.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read the backup file")
		return
	}

	name := filepath.Base(t.Path)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// handleDeleteTransfer removes a transfer row and the directory staging it,
// whether it is a finished backup or a restore upload nobody imported.
func (s *Server) handleDeleteTransfer(w http.ResponseWriter, r *http.Request) {
	t, ok := s.transferOr404(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTransfer(r.Context(), t.UserID, t.ID); err != nil {
		s.log.Error("delete image transfer", "id", t.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot delete transfer")
		return
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.TransfersDir, t.ID)); err != nil {
		s.log.Warn("remove transfer directory", "id", t.ID, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// transferOr404 loads the transfer named in the path, scoped to the caller.
func (s *Server) transferOr404(w http.ResponseWriter, r *http.Request) (*store.ImageTransfer, bool) {
	t, err := s.store.TransferByID(r.Context(), s.user(r).ID, r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such transfer")
		return nil, false
	case err != nil:
		s.log.Error("load image transfer", "id", r.PathValue("id"), "err", err)
		writeError(w, http.StatusInternalServerError, "cannot load transfer")
		return nil, false
	}
	return t, true
}
