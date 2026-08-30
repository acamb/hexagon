package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/store"
	"github.com/distribution/reference"
	"github.com/google/uuid"
)

const (
	// maxImageRequestBody bounds a Dockerfile submission.
	maxImageRequestBody = 256 << 10
	// maxBuildLog keeps the tail of long builds; the interesting part of a
	// failure is at the end.
	maxBuildLog = 256 << 10
	// imageBuildTimeout stops a build that will never finish. Pulling a large
	// base image over a slow link is the case to accommodate here.
	imageBuildTimeout = 45 * time.Minute
	// buildLogFlush is how often a running build's output reaches the database,
	// and therefore the UI.
	buildLogFlush = time.Second
	// sourceEditTimeout bounds a call to Claude Code. It answers in seconds;
	// this is the point at which something has gone wrong rather than slow.
	sourceEditTimeout = 3 * time.Minute
	// composeValidateTimeout bounds the `docker compose config` a submitted
	// compose file goes through. It is a local parse: this is the point at which
	// the CLI is not answering rather than being slow.
	composeValidateTimeout = 30 * time.Second
	// maxInstruction bounds what the user can ask for. It is a sentence or two.
	maxInstruction = 4 << 10
)

// imageNamePattern keeps names readable and safe to show anywhere.
var imageNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 ._-]{0,63}$`)

type imageResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	SourceType  string    `json:"sourceType"`
	Dockerfile  string    `json:"dockerfile,omitempty"`
	Compose     string    `json:"compose,omitempty"`
	RegistryRef string    `json:"registryRef,omitempty"`
	ImageRef    string    `json:"imageRef,omitempty"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

func newImageResponse(img *store.Image) imageResponse {
	return imageResponse{
		ID:          img.ID,
		Name:        img.Name,
		SourceType:  img.SourceType,
		Dockerfile:  img.Dockerfile,
		Compose:     img.Compose,
		RegistryRef: img.RegistryRef,
		ImageRef:    img.ImageRef,
		Status:      img.Status,
		Error:       img.Error,
		CreatedAt:   img.CreatedAt,
	}
}

// handleListImages returns the caller's images.
func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	images, err := s.store.ListImages(r.Context(), s.user(r).ID)
	if err != nil {
		s.log.Error("list images", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot list images")
		return
	}

	out := make([]imageResponse, 0, len(images))
	for _, img := range images {
		out = append(out, newImageResponse(img))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetImage returns one image.
func (s *Server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	img, ok := s.imageOr404(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newImageResponse(img))
}

// handleImageLog returns the build output collected so far. The UI polls it
// while an image is building.
func (s *Server) handleImageLog(w http.ResponseWriter, r *http.Request) {
	img, ok := s.imageOr404(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": img.Status,
		"log":    img.BuildLog,
		"error":  img.Error,
	})
}

// handleImageTemplate hands the UI the files to start a new image from, and
// says what this server can do besides building one: with no Claude Code binary
// there is no ask-Claude control, and with no `docker compose` there is no
// advanced mode. Both are left out of the page rather than offered as buttons
// that always fail.
func (s *Server) handleImageTemplate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"dockerfile": s.baseDockerfile,
		"compose":    s.baseCompose,
		"canAsk":     s.editor != nil,
		"canCompose": s.compose != nil,
	})
}

type editSourceRequest struct {
	Kind        string `json:"kind"`
	Content     string `json:"content"`
	Instruction string `json:"instruction"`
}

// handleEditSource asks Claude Code to apply an instruction to a Dockerfile or a
// compose file and hands back the result. Nothing is stored: this is the
// editor's undo buffer, not an image.
//
// It lives outside /api/images/{id} because the image does not exist yet, and
// may never: the answer is something to read before deciding to build.
func (s *Server) handleEditSource(w http.ResponseWriter, r *http.Request) {
	if s.editor == nil {
		writeError(w, http.StatusServiceUnavailable, "this server has no Claude Code binary to run")
		return
	}

	var req editSourceRequest
	if err := decodeJSON(w, r, maxImageRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	instruction := strings.TrimSpace(req.Instruction)
	switch {
	case req.Kind != claudex.SourceDockerfile && req.Kind != claudex.SourceCompose:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("kind must be %q or %q",
			claudex.SourceDockerfile, claudex.SourceCompose))
		return
	case instruction == "":
		writeError(w, http.StatusBadRequest, "say what to change")
		return
	case len(instruction) > maxInstruction:
		writeError(w, http.StatusBadRequest, "that instruction is too long")
		return
	case strings.TrimSpace(req.Content) == "":
		writeError(w, http.StatusBadRequest, "there is nothing to change")
		return
	}

	cred, err := s.auth.ClaudeCredential(r.Context(), s.user(r).ID)
	if err != nil {
		s.log.Error("read claude credential", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read the stored claude credential")
		return
	}

	// Tied to the request: a browser that has gone away is not going to read
	// the answer, and this call costs money for as long as it runs.
	ctx, cancel := context.WithTimeout(r.Context(), sourceEditTimeout)
	defer cancel()

	edit, err := s.editor.Edit(ctx, cred, req.Kind, req.Content, instruction)
	if err != nil {
		s.log.Error("edit image source", "kind", req.Kind, "login", s.user(r).GitHubLogin, "err", err)
		// 502: the thing that failed is something this server called out to.
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.log.Info("image source edited", "kind", req.Kind, "login", s.user(r).GitHubLogin, "summary", edit.Summary)
	writeJSON(w, http.StatusOK, edit)
}

type createImageRequest struct {
	Name        string `json:"name"`
	SourceType  string `json:"sourceType"`
	Dockerfile  string `json:"dockerfile"`
	Compose     string `json:"compose"`
	RegistryRef string `json:"registryRef"`
}

// handleCreateImage registers an image and starts building or pulling it. The
// request returns as soon as the row exists; progress is followed through the
// image's status and log.
func (s *Server) handleCreateImage(w http.ResponseWriter, r *http.Request) {
	var req createImageRequest
	if err := decodeJSON(w, r, maxImageRequestBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	img, err := imageFromRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	img.UserID = s.user(r).ID

	// While the operator is still looking at the editor, rather than at the
	// first session that fails to start. A file Claude Code wrote comes through
	// here too: the prompt lists the rules, this is what enforces them.
	if img.SourceType == store.ImageSourceCompose {
		if s.compose == nil {
			writeError(w, http.StatusServiceUnavailable, "this server has no docker compose to run a project with")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), composeValidateTimeout)
		defer cancel()
		if _, err := s.compose.Validate(ctx, img.Compose); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// A build runs for as long as its Dockerfile takes and pulls whatever that
	// Dockerfile names, so only so many run at once.
	switch building, err := s.store.CountBuildingImages(r.Context(), img.UserID); {
	case err != nil:
		s.log.Error("count builds in flight", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot create image")
		return
	case building >= s.cfg.MaxConcurrentBuilds:
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf(
			"at most %d builds at a time: wait for one to finish", s.cfg.MaxConcurrentBuilds))
		return
	}

	// Fail here rather than leaving a row that mysteriously never builds.
	if err := s.docker.Ping(r.Context()); err != nil {
		s.log.Error("docker unreachable", "err", err)
		writeError(w, http.StatusServiceUnavailable, "docker is unreachable")
		return
	}

	created, err := s.store.CreateImage(r.Context(), img)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, fmt.Sprintf("an image named %q already exists", img.Name))
		return
	case err != nil:
		s.log.Error("create image", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot create image")
		return
	}

	s.startImageBuild(created)
	writeJSON(w, http.StatusAccepted, newImageResponse(created))
}

// imageFromRequest validates the payload and returns the row to insert.
func imageFromRequest(req createImageRequest) (*store.Image, error) {
	name := strings.TrimSpace(req.Name)
	if !imageNamePattern.MatchString(name) {
		return nil, errors.New("name must be 1-64 characters of letters, digits, spaces, dots, dashes or underscores")
	}

	img := &store.Image{
		ID:     newImageID(),
		Name:   name,
		Status: store.ImageStatusBuilding,
	}

	switch req.SourceType {
	case store.ImageSourceDockerfile:
		dockerfile := strings.TrimSpace(req.Dockerfile)
		if dockerfile == "" {
			return nil, errors.New("a Dockerfile is required")
		}
		img.SourceType = store.ImageSourceDockerfile
		img.Dockerfile = dockerfile
		img.ImageRef = imageTag(img.ID)

	case store.ImageSourceCompose:
		// A Dockerfile and a compose file together: the Dockerfile still
		// describes the container Claude Code runs in, and builds exactly as it
		// does above.
		dockerfile := strings.TrimSpace(req.Dockerfile)
		compose := strings.TrimSpace(req.Compose)
		switch {
		case dockerfile == "":
			return nil, errors.New("a Dockerfile is required")
		case compose == "":
			return nil, errors.New("a compose file is required")
		}
		img.SourceType = store.ImageSourceCompose
		img.Dockerfile = dockerfile
		img.Compose = compose
		img.ImageRef = imageTag(img.ID)

	case store.ImageSourceRegistry:
		ref, err := normalizeRegistryRef(req.RegistryRef)
		if err != nil {
			return nil, err
		}
		img.SourceType = store.ImageSourceRegistry
		img.RegistryRef = strings.TrimSpace(req.RegistryRef)
		img.ImageRef = ref

	default:
		return nil, fmt.Errorf("sourceType must be %q, %q or %q",
			store.ImageSourceDockerfile, store.ImageSourceRegistry, store.ImageSourceCompose)
	}
	return img, nil
}

// newImageID returns the identifier for a new image row.
func newImageID() string { return uuid.NewString() }

// imageTag derives the local tag for an image Hexagon builds. It comes from the
// row id, so two images with similar names never collide and deleting one never
// removes another's tag.
func imageTag(id string) string {
	short := strings.ReplaceAll(id, "-", "")
	if len(short) > 12 {
		short = short[:12]
	}
	return "hexagon/img-" + short + ":latest"
}

// normalizeRegistryRef validates a registry reference and returns its canonical
// form, so "node:22" and "docker.io/library/node:22" end up as one image.
func normalizeRegistryRef(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("a registry reference is required")
	}
	named, err := reference.ParseNormalizedNamed(raw)
	if err != nil {
		return "", fmt.Errorf("invalid image reference: %w", err)
	}
	return reference.TagNameOnly(named).String(), nil
}

// handleDeleteImage removes an image, and the Docker image behind it when
// Hexagon is the one that built it.
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	img, ok := s.imageOr404(w, r)
	if !ok {
		return
	}

	inUse, err := s.store.CountSessionsUsingImage(r.Context(), img.ID)
	if err != nil {
		s.log.Error("count sessions using image", "id", img.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot delete image")
		return
	}
	if inUse > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("image is used by %d session(s)", inUse))
		return
	}

	if err := s.store.DeleteImage(r.Context(), img.UserID, img.ID); err != nil {
		s.log.Error("delete image", "id", img.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot delete image")
		return
	}

	// Only tags Hexagon created are ours to remove; a pulled image may well be
	// in use by something else on this machine.
	if img.SourceType != store.ImageSourceRegistry && img.ImageRef != "" {
		if err := s.docker.RemoveImage(r.Context(), img.ImageRef); err != nil {
			s.log.Warn("image row deleted but the docker image remains", "ref", img.ImageRef, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// imageOr404 loads the image named in the path, scoped to the caller.
func (s *Server) imageOr404(w http.ResponseWriter, r *http.Request) (*store.Image, bool) {
	img, err := s.store.ImageByID(r.Context(), s.user(r).ID, r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such image")
		return nil, false
	case err != nil:
		s.log.Error("load image", "id", r.PathValue("id"), "err", err)
		writeError(w, http.StatusInternalServerError, "cannot load image")
		return nil, false
	}
	return img, true
}

// startImageBuild builds or pulls the image in the background. It deliberately
// does not use the request context: the browser navigating away must not cancel
// a ten minute build.
func (s *Server) startImageBuild(img *store.Image) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), imageBuildTimeout)
		defer cancel()

		sink := &logSink{limit: maxBuildLog}
		stopFlush := s.flushBuildLog(ctx, img.ID, sink)

		var err error
		switch img.SourceType {
		// An advanced image builds exactly as a plain one does: the compose
		// file is a session-time concern and there is nothing here to build
		// from it.
		case store.ImageSourceDockerfile, store.ImageSourceCompose:
			err = s.docker.BuildImage(ctx, img.Dockerfile, img.ImageRef, sink)
		case store.ImageSourceRegistry:
			err = s.docker.PullImage(ctx, img.ImageRef, sink)
		}
		stopFlush()

		status, message := store.ImageStatusReady, ""
		if err != nil {
			status, message = store.ImageStatusFailed, err.Error()
			fmt.Fprintf(sink, "\nerror: %s\n", err)
			s.log.Error("image build failed", "name", img.Name, "ref", img.ImageRef, "err", err)
		} else {
			s.log.Info("image ready", "name", img.Name, "ref", img.ImageRef)
		}

		// A fresh context: the build one may have expired, and the result must
		// be recorded either way.
		finishCtx, finishCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer finishCancel()
		if err := s.store.FinishImage(finishCtx, img.ID, status, img.ImageRef, sink.String(), message); err != nil {
			s.log.Error("record image result", "id", img.ID, "err", err)
		}
	}()
}

// flushBuildLog copies the running build's output into the database on a timer
// and returns a function that stops it.
func (s *Server) flushBuildLog(ctx context.Context, imageID string, sink *logSink) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(buildLogFlush)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.store.SetImageBuildLog(ctx, imageID, sink.String()); err != nil {
					s.log.Warn("flush build log", "id", imageID, "err", err)
				}
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// logSink collects build output, keeping only the tail once it grows past
// limit. It is written from the Docker stream and read by the flusher, so every
// access is guarded.
type logSink struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.data = append(l.data, p...)
	if l.limit > 0 && len(l.data) > l.limit {
		l.data = append(l.data[:0], l.data[len(l.data)-l.limit:]...)
		l.truncated = true
	}
	return len(p), nil
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.truncated {
		return string(l.data)
	}
	var buf bytes.Buffer
	buf.WriteString("[earlier output truncated]\n")
	buf.Write(l.data)
	return buf.String()
}

var _ io.Writer = (*logSink)(nil)
