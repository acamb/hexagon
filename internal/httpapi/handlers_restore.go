package httpapi

import (
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
	"github.com/google/uuid"
)

// maxRestoreUpload bounds a restore archive. It sits beside
// maxImageRequestBody and is several orders of magnitude larger: an archive
// carries a whole exported image, not a typed Dockerfile.
const maxRestoreUpload = 10 << 30

// restoreUploadPath is the one route that is not JSON: an archive cannot be
// JSON without base64 and a third more bytes. guardStateChanges carves out
// exactly this path and nothing else.
const restoreUploadPath = "/api/images/restore"

// isRestoreUploadPath reports whether p is the restore upload route.
func isRestoreUploadPath(p string) bool { return p == restoreUploadPath }

type restoreInspectionResponse struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	SourceType      string `json:"sourceType"`
	Dockerfile      string `json:"dockerfile,omitempty"`
	Compose         string `json:"compose,omitempty"`
	HasImage        bool   `json:"hasImage"`
	ImageSize       int64  `json:"imageSize,omitempty"`
	NameExists      bool   `json:"nameExists"`
	ExistingImageID string `json:"existingImageId,omitempty"`
}

// handleRestoreUpload stages an uploaded backup archive under DataDir,
// inspects its spec, and answers with what it found. Nothing is imported yet:
// that is the whole of the spec-only restore path, and the only thing the
// import endpoint has left to do is the image half.
func (s *Server) handleRestoreUpload(w http.ResponseWriter, r *http.Request) {
	length := r.ContentLength
	if length <= 0 {
		writeError(w, http.StatusBadRequest, "Content-Length is required")
		return
	}
	if length > maxRestoreUpload {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("archive too large: at most %d bytes", int64(maxRestoreUpload)))
		return
	}
	if err := s.checkFreeSpaceFor(length); err != nil {
		writeError(w, http.StatusInsufficientStorage, err.Error())
		return
	}

	id := uuid.NewString()
	dir := filepath.Join(s.cfg.TransfersDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.log.Error("create transfer directory", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot stage the upload")
		return
	}
	path := filepath.Join(dir, "upload.tar.gz")

	if err := stageUpload(w, r, path, length); err != nil {
		os.RemoveAll(dir)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "upload exceeded the declared size")
			return
		}
		writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
		return
	}

	insp, err := inspectArchive(path)
	if err != nil {
		os.RemoveAll(dir)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		os.RemoveAll(dir)
		s.log.Error("stat staged upload", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot stage the upload")
		return
	}

	now := time.Now()
	created, err := s.store.CreateTransfer(r.Context(), &store.ImageTransfer{
		ID:        id,
		UserID:    s.user(r).ID,
		Direction: store.TransferDirectionRestore,
		Name:      insp.Name,
		Status:    store.TransferStatusReady,
		WithImage: insp.HasImage,
		Path:      path,
		Size:      info.Size(),
		CreatedAt: now,
		ExpiresAt: now.Add(transferLifetime),
	})
	if err != nil {
		os.RemoveAll(dir)
		s.log.Error("create image transfer", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot stage the upload")
		return
	}

	nameExists, existingID := false, ""
	switch existing, err := s.store.ImageByName(r.Context(), s.user(r).ID, insp.Name); {
	case err == nil:
		nameExists, existingID = true, existing.ID
	case !errors.Is(err, store.ErrNotFound):
		s.log.Error("check existing image name", "err", err)
	}

	writeJSON(w, http.StatusOK, restoreInspectionResponse{
		ID:              created.ID,
		Name:            insp.Name,
		SourceType:      insp.SourceType,
		Dockerfile:      insp.Dockerfile,
		Compose:         insp.Compose,
		HasImage:        insp.HasImage,
		ImageSize:       insp.ImageSize,
		NameExists:      nameExists,
		ExistingImageID: existingID,
	})
}

// stageUpload writes the request body to path, bounded to declared bytes: a
// second cap beside the Content-Length check already made, for a client that
// lied about the header.
func stageUpload(w http.ResponseWriter, r *http.Request, path string, declared int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("stage upload: %w", err)
	}
	body := http.MaxBytesReader(w, r.Body, declared)
	_, copyErr := io.Copy(f, body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// checkFreeSpaceFor refuses an upload the disk holding DataDir cannot fit.
// Filling the disk under SQLite takes the whole application down, and it is
// the failure this feature is most likely to cause. TransfersDir may not exist
// yet on a server's first restore, so DataDir — always there, since Load
// creates it — is what gets statfs'd; both live on the same filesystem.
func (s *Server) checkFreeSpaceFor(declared int64) error {
	snap, err := s.hostSampler.Read([]string{s.cfg.DataDir})
	if err != nil {
		return fmt.Errorf("cannot check free disk space: %w", err)
	}
	if !snap.Available || len(snap.Filesystems) == 0 {
		// A platform this process cannot read /proc or statfs on: nothing to
		// check against, so the upload is not blocked on it.
		return nil
	}
	if free := snap.Filesystems[0].Free; free < uint64(declared) {
		return fmt.Errorf("not enough free space to stage this upload: %d bytes free, %d needed", free, declared)
	}
	return nil
}

// inspectArchive opens the staged file fresh, so the reader Inspect walks has
// nothing left over from staging it.
func inspectArchive(path string) (backup.Inspection, error) {
	f, err := os.Open(path)
	if err != nil {
		return backup.Inspection{}, fmt.Errorf("open staged upload: %w", err)
	}
	defer f.Close()
	return backup.Inspect(f)
}

type importRequest struct {
	Name      string `json:"name"`
	Overwrite bool   `json:"overwrite"`
}

// handleImportTransfer imports a staged restore's image: loads it into the
// daemon, retags it, and links the row that just went ready back to the
// transfer. The request returns as soon as the image row exists; progress is
// followed through the image's own log, exactly as a build's is — see
// plans/M3/02-image-backup-restore.md for why this reuses "building" rather
// than adding a status.
func (s *Server) handleImportTransfer(w http.ResponseWriter, r *http.Request) {
	t, ok := s.transferOr404(w, r)
	if !ok {
		return
	}
	if t.Direction != store.TransferDirectionRestore {
		writeError(w, http.StatusBadRequest, "not a restore")
		return
	}
	if t.Status != store.TransferStatusReady {
		writeError(w, http.StatusConflict, "this upload is not staged yet")
		return
	}
	if !t.WithImage {
		writeError(w, http.StatusBadRequest, "this backup has no image to import")
		return
	}

	var req importRequest
	if err := decodeJSON(w, r, maxImageRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if !imageNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "name must be 1-64 characters of letters, digits, spaces, dots, dashes or underscores")
		return
	}

	insp, err := inspectArchive(t.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read the staged backup: "+err.Error())
		return
	}

	existing, err := s.store.ImageByName(r.Context(), s.user(r).ID, name)
	switch {
	case err == nil && !req.Overwrite:
		writeError(w, http.StatusConflict, fmt.Sprintf("an image named %q already exists", name))
		return
	case err != nil && !errors.Is(err, store.ErrNotFound):
		s.log.Error("check existing image name", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot import backup")
		return
	}

	switch building, err := s.store.CountBuildingImages(r.Context(), s.user(r).ID); {
	case err != nil:
		s.log.Error("count builds in flight", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot import backup")
		return
	case building >= s.cfg.MaxConcurrentBuilds:
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf(
			"at most %d builds at a time: wait for one to finish", s.cfg.MaxConcurrentBuilds))
		return
	}
	if err := s.docker.Ping(r.Context()); err != nil {
		s.log.Error("docker unreachable", "err", err)
		writeError(w, http.StatusServiceUnavailable, "docker is unreachable")
		return
	}

	var img *store.Image
	if existing != nil {
		// Overwrite updates the row in place: same id, so every session that
		// points at it goes on pointing at it, and the same local tag, so the
		// tag the new layers get is the tag the row already claims.
		if err := s.store.UpdateImageForRestore(r.Context(), s.user(r).ID, existing.ID,
			insp.SourceType, insp.Dockerfile, insp.Compose, insp.RegistryRef); err != nil {
			s.log.Error("update image for restore", "id", existing.ID, "err", err)
			writeError(w, http.StatusInternalServerError, "cannot import backup")
			return
		}
		existing.SourceType, existing.Dockerfile, existing.Compose, existing.RegistryRef =
			insp.SourceType, insp.Dockerfile, insp.Compose, insp.RegistryRef
		existing.Status = store.ImageStatusBuilding
		// A restored image is always local, loaded content, whatever the row's
		// previous source type: a pulled image being overwritten this way did
		// not carry a Hexagon tag before, and does now, exactly like a freshly
		// imported one.
		existing.ImageRef = imageTag(existing.ID)
		img = existing
	} else {
		newID := newImageID()
		created, err := s.store.CreateImage(r.Context(), &store.Image{
			ID:          newID,
			UserID:      s.user(r).ID,
			Name:        name,
			SourceType:  insp.SourceType,
			Dockerfile:  insp.Dockerfile,
			Compose:     insp.Compose,
			RegistryRef: insp.RegistryRef,
			ImageRef:    imageTag(newID),
			Status:      store.ImageStatusBuilding,
		})
		switch {
		case errors.Is(err, store.ErrConflict):
			writeError(w, http.StatusConflict, fmt.Sprintf("an image named %q already exists", name))
			return
		case err != nil:
			s.log.Error("create image", "err", err)
			writeError(w, http.StatusInternalServerError, "cannot import backup")
			return
		}
		img = created
	}

	s.startImportImage(t, img, insp.ImageRef)
	writeJSON(w, http.StatusAccepted, newImageResponse(img))
}

// startImportImage loads the staged archive's image into the daemon and
// retags it, in the background and on its own timeout, exactly as
// startImageBuild runs a build.
func (s *Server) startImportImage(t *store.ImageTransfer, img *store.Image, sourceRef string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), imageBuildTimeout)
		defer cancel()

		sink := &logSink{limit: maxBuildLog}
		stopFlush := s.flushBuildLog(ctx, img.ID, sink)
		err := s.importImage(ctx, t, img, sourceRef, sink)
		stopFlush()

		status, message := store.ImageStatusReady, ""
		if err != nil {
			status, message = store.ImageStatusFailed, err.Error()
			fmt.Fprintf(sink, "\nerror: %s\n", err)
			s.log.Error("image import failed", "name", img.Name, "id", img.ID, "err", err)
		} else {
			s.log.Info("image imported", "name", img.Name, "ref", img.ImageRef)
		}

		finishCtx, finishCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer finishCancel()
		if err := s.store.FinishImage(finishCtx, img.ID, status, img.ImageRef, sink.String(), message); err != nil {
			s.log.Error("record image import result", "id", img.ID, "err", err)
		}
		if status == store.ImageStatusReady {
			if err := s.store.FinishTransfer(finishCtx, t.ID, store.TransferStatusReady, img.ID, t.Path, t.Size, ""); err != nil {
				s.log.Error("link restore transfer to image", "id", t.ID, "err", err)
			}
		}
		// A failed import leaves the transfer row alone: the staged archive is
		// still good, and the reason for the failure is Docker's, not the
		// archive's — leaving it in place is what lets the same import be
		// retried without uploading again.
	}()
}

// importImage loads the staged archive's image.tar.gz, retags the reference it
// names as the row's own local tag, and removes the foreign tag — guarded
// against the case where it is already the tag being applied, which would
// otherwise delete what was just loaded.
func (s *Server) importImage(ctx context.Context, t *store.ImageTransfer, img *store.Image, sourceRef string, logs io.Writer) error {
	if sourceRef == "" {
		return errors.New("backup manifest has no image reference")
	}

	r, err := backup.OpenImage(t.Path)
	if err != nil {
		return fmt.Errorf("open staged backup: %w", err)
	}
	defer r.Close()

	if err := s.docker.LoadImage(ctx, r, logs); err != nil {
		return fmt.Errorf("load image: %w", err)
	}
	if err := s.docker.TagImage(ctx, sourceRef, img.ImageRef); err != nil {
		return fmt.Errorf("tag imported image: %w", err)
	}
	if sourceRef != img.ImageRef {
		if err := s.docker.RemoveImage(ctx, sourceRef); err != nil {
			s.log.Warn("remove the foreign tag left by a restore", "ref", sourceRef, "err", err)
		}
	}
	if _, err := s.docker.InspectImage(ctx, img.ImageRef); err != nil {
		return fmt.Errorf("inspect imported image: %w", err)
	}
	return nil
}
