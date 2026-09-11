package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/backup"
	"github.com/andrea/hexagon/internal/store"
)

// postRaw sends a request with a raw body and explicit headers, which is what
// the restore upload needs and postJSON cannot do.
func (e *testEnv) postRaw(path string, body []byte, headers map[string]string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Origin", testOrigin)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("POST %s: %v", path, err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// waitForTransferStatus polls until the background job has recorded its
// outcome.
func (e *testEnv) waitForTransferStatus(id, want string) transferResponse {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var tr transferResponse
	for time.Now().Before(deadline) {
		e.decode(e.do(http.MethodGet, "/api/transfers/"+id, nil), &tr)
		if tr.Status == want {
			return tr
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("transfer %s stayed in status %q, want %q", id, tr.Status, want)
	return tr
}

// testArchiveBytes builds a valid backup archive the way internal/backup
// writes one, for tests that upload it.
func testArchiveBytes(t *testing.T, spec backup.Spec, image []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var r io.Reader
	var size int64
	if image != nil {
		r = bytes.NewReader(image)
		size = int64(len(image))
	}
	if err := backup.WriteArchive(&buf, spec, r, size); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	return buf.Bytes()
}

func TestBackupOfAReadyImageReachesReadyWithAFile(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	img := env.readyImage("base")

	var created transferResponse
	env.decode(env.postJSON("/api/images/"+img.ID+"/backup", `{"withImage":true}`), &created)
	if created.Status == "" {
		t.Fatalf("create backup: got %+v", created)
	}

	ready := env.waitForTransferStatus(created.ID, store.TransferStatusReady)
	if ready.Size == 0 {
		t.Errorf("ready transfer has size 0")
	}

	tr, err := env.store.TransferByID(context.Background(), env.userID(), created.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if _, err := os.Stat(tr.Path); err != nil {
		t.Errorf("backup file missing on disk: %v", err)
	}

	if len(env.docker.saved) != 1 || env.docker.saved[0] != img.ImageRef {
		t.Errorf("saved = %v, want [%s]", env.docker.saved, img.ImageRef)
	}
}

// A backup asked to export the image is refused when there is nothing ready
// to export yet.
func TestBackupWithImageRefusedWhileTheImageIsNotReady(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	// Inserted directly through the store, which starts no build goroutine: the
	// fake daemon finishes a build fast enough that going through the handler
	// races the very state this test wants to hold still.
	img, err := env.store.CreateImage(context.Background(), &store.Image{
		UserID: env.userID(), Name: "base", SourceType: store.ImageSourceDockerfile,
		Dockerfile: "FROM x", ImageRef: "hexagon/img-notready:latest", Status: store.ImageStatusBuilding,
	})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	resp := env.postJSON("/api/images/"+img.ID+"/backup", `{"withImage":true}`)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// A spec-only backup needs no image at all, so it can run against an image
// that is still building.
func TestBackupSpecOnlyDoesNotRequireAReadyImage(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var img imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"base","sourceType":"dockerfile","dockerfile":"FROM x"}`), &img)

	resp := env.postJSON("/api/images/"+img.ID+"/backup", `{"withSpec":true,"withImage":false}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var created transferResponse
	env.decode(resp, &created)
	env.waitForTransferStatus(created.ID, store.TransferStatusReady)
}

// Neither half selected has nothing to back up.
func TestBackupRequiresAtLeastOneHalf(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	img := env.readyImage("base")

	resp := env.postJSON("/api/images/"+img.ID+"/backup", `{"withSpec":false,"withImage":false}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDownloadTransferServesTheArchive(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	img := env.readyImage("base")

	var created transferResponse
	env.decode(env.postJSON("/api/images/"+img.ID+"/backup", `{"withSpec":true,"withImage":false}`), &created)
	env.waitForTransferStatus(created.ID, store.TransferStatusReady)

	resp := env.do(http.MethodGet, "/api/transfers/"+created.ID+"/file", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	body := make([]byte, 4)
	if _, err := resp.Body.Read(body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body[:2]) != "\x1f\x8b" {
		t.Errorf("body does not start with a gzip magic number: %x", body)
	}
}

// While the job is still running there is no file to serve yet.
func TestDownloadTransferRefusedWhileRunning(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	now := time.Now()
	created, err := env.store.CreateTransfer(context.Background(), &store.ImageTransfer{
		UserID: env.userID(), Direction: store.TransferDirectionBackup, ImageID: "img-x", Name: "base",
		Status: store.TransferStatusRunning, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	resp := env.do(http.MethodGet, "/api/transfers/"+created.ID+"/file", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// Another user's transfer is 404, for the row and for the file.
func TestTransfersAreScopedToTheirOwner(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	ctx := context.Background()
	other, err := env.store.UpsertUser(ctx, &store.User{GitHubLogin: "bob", GitHubID: 7})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	now := time.Now()
	theirs, err := env.store.CreateTransfer(ctx, &store.ImageTransfer{
		UserID: other.ID, Direction: store.TransferDirectionBackup, ImageID: "img-x", Name: "theirs",
		Status: store.TransferStatusReady, Path: os.DevNull, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	if got := env.do(http.MethodGet, "/api/transfers/"+theirs.ID, nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("GET a foreign transfer = %d, want 404", got)
	}
	if got := env.do(http.MethodGet, "/api/transfers/"+theirs.ID+"/file", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("GET a foreign transfer's file = %d, want 404", got)
	}
	if got := env.do(http.MethodGet, "/api/transfers/"+theirs.ID+"/inspection", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("GET a foreign transfer's inspection = %d, want 404", got)
	}
}

func TestBackupEndpointsRequireASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	for _, path := range []string{"/api/transfers", "/api/transfers/x", "/api/transfers/x/file", "/api/transfers/x/inspection"} {
		if got := env.do(http.MethodGet, path, nil).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, got)
		}
	}
	if got := env.postJSON("/api/images/x/backup", `{"withImage":false}`).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("POST backup without a session = %d, want 401", got)
	}
	if got := env.postRaw("/api/images/restore", []byte("x"), map[string]string{"Content-Type": "application/gzip"}).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("POST restore without a session = %d, want 401", got)
	}
}

// --- Restore ---

func specOnlyArchive(t *testing.T, name string) []byte {
	t.Helper()
	return testArchiveBytes(t, backup.Spec{
		Manifest:   backup.Manifest{FormatVersion: backup.FormatVersion, Name: name, SourceType: "dockerfile"},
		Dockerfile: "FROM busybox\n",
	}, nil)
}

func fullArchive(t *testing.T, name, imageRef string, image []byte) []byte {
	t.Helper()
	return testArchiveBytes(t, backup.Spec{
		Manifest:   backup.Manifest{FormatVersion: backup.FormatVersion, Name: name, SourceType: "dockerfile", ImageRef: imageRef},
		Dockerfile: "FROM busybox\n",
	}, image)
}

func TestRestoreUploadInspectsASpecOnlyArchive(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	raw := specOnlyArchive(t, "restored")
	resp := env.postRaw("/api/images/restore", raw, map[string]string{"Content-Type": "application/gzip"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var insp restoreInspectionResponse
	env.decode(resp, &insp)
	if insp.Name != "restored" || insp.SourceType != "dockerfile" || insp.HasImage {
		t.Errorf("inspection = %+v", insp)
	}
	if !strings.Contains(insp.Dockerfile, "busybox") {
		t.Errorf("dockerfile = %q", insp.Dockerfile)
	}
}

func TestRestoreUploadWithoutTheContentTypeCarveOutIsRefused(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	raw := specOnlyArchive(t, "restored")
	resp := env.postRaw("/api/images/restore", raw, map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

// The carve-out is exactly one path: a mutating request to any other path with
// the archive content type is still refused.
func TestGzipContentTypeIsRefusedOnAnyOtherPath(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	resp := env.postRaw("/api/images", []byte(`{"name":"x"}`), map[string]string{"Content-Type": "application/gzip"})
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

func TestRestoreUploadOverTheLimitIsRefusedWithoutBeingWritten(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	req := httptest.NewRequest(http.MethodPost, "/api/images/restore", bytes.NewReader([]byte("short")))
	req.ContentLength = maxRestoreUpload + 1
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(env.sessionCookie())

	rr := httptest.NewRecorder()
	New(env.deps).ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}

	entries, err := os.ReadDir(env.cfg.TransfersDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read transfers dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused upload staged %d entries", len(entries))
	}
}

func TestImportRequiresOverwriteForAnExistingName(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	existing := env.readyImage("base")

	raw := fullArchive(t, "base", "some/other:latest", []byte("layer-bytes"))
	var insp restoreInspectionResponse
	env.decode(env.postRaw("/api/images/restore", raw, map[string]string{"Content-Type": "application/gzip"}), &insp)
	if !insp.NameExists || insp.ExistingImageID != existing.ID {
		t.Fatalf("inspection = %+v, want nameExists for the existing image", insp)
	}

	resp := env.postJSON("/api/images/restore/"+insp.ID+"/import", `{"name":"base","overwrite":false}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status without overwrite = %d, want 409", resp.StatusCode)
	}

	resp = env.postJSON("/api/images/restore/"+insp.ID+"/import", `{"name":"base","overwrite":true}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status with overwrite = %d, want 202", resp.StatusCode)
	}
	var imported imageResponse
	env.decode(resp, &imported)
	if imported.ID != existing.ID {
		t.Errorf("imported id = %q, want the existing image's id %q kept", imported.ID, existing.ID)
	}

	ready := env.waitForImageStatus(imported.ID, store.ImageStatusReady)
	if ready.ImageRef != existing.ImageRef {
		t.Errorf("image ref = %q, want the same local tag as before: %q", ready.ImageRef, existing.ImageRef)
	}

	if len(env.docker.loaded) != 1 || env.docker.loaded[0] != "layer-bytes" {
		t.Errorf("loaded = %v, want the archive's image content", env.docker.loaded)
	}
	foundTag := false
	for _, pair := range env.docker.tagged {
		if pair[0] == "some/other:latest" && pair[1] == existing.ImageRef {
			foundTag = true
		}
	}
	if !foundTag {
		t.Errorf("tagged = %v, want the manifest's ref retagged to %s", env.docker.tagged, existing.ImageRef)
	}
}

// A fresh name with no conflict creates a new image row.
func TestImportCreatesANewImageForAFreshName(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	raw := fullArchive(t, "irrelevant", "some/other:latest", []byte("layer-bytes"))
	var insp restoreInspectionResponse
	env.decode(env.postRaw("/api/images/restore", raw, map[string]string{"Content-Type": "application/gzip"}), &insp)

	resp := env.postJSON("/api/images/restore/"+insp.ID+"/import", `{"name":"brand-new","overwrite":false}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var imported imageResponse
	env.decode(resp, &imported)

	ready := env.waitForImageStatus(imported.ID, store.ImageStatusReady)
	if !strings.HasPrefix(ready.ImageRef, "hexagon/img-") {
		t.Errorf("image ref = %q, want a fresh hexagon/img- tag", ready.ImageRef)
	}
}

// A failed load leaves the image row failed and does not overwrite the
// previous image_ref; the staged transfer is left alone, so the same import
// can be retried without uploading again.
func TestImportWhoseLoadFailsLeavesTheImageFailed(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	existing := env.readyImage("base")
	previousRef := existing.ImageRef

	raw := fullArchive(t, "base", "some/other:latest", []byte("layer-bytes"))
	var insp restoreInspectionResponse
	env.decode(env.postRaw("/api/images/restore", raw, map[string]string{"Content-Type": "application/gzip"}), &insp)

	env.docker.loadErr = errors.New("simulated load failure")
	resp := env.postJSON("/api/images/restore/"+insp.ID+"/import", `{"name":"base","overwrite":true}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	failed := env.waitForImageStatus(existing.ID, store.ImageStatusFailed)
	if failed.ImageRef != previousRef {
		t.Errorf("image ref after a failed import = %q, want the previous ref %q kept", failed.ImageRef, previousRef)
	}

	tr, err := env.store.TransferByID(context.Background(), env.userID(), insp.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if tr.Status != store.TransferStatusReady {
		t.Errorf("transfer status after a failed import = %q, want ready: the staged archive is still good", tr.Status)
	}
}

// --- Inspecting and restoring a staged transfer: M3.4 ---

func TestInspectTransferAnswersAReadyBackupsSpec(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	img := env.readyImage("base")

	var created transferResponse
	env.decode(env.postJSON("/api/images/"+img.ID+"/backup", `{"withSpec":true,"withImage":true}`), &created)
	env.waitForTransferStatus(created.ID, store.TransferStatusReady)

	var insp restoreInspectionResponse
	env.decode(env.do(http.MethodGet, "/api/transfers/"+created.ID+"/inspection", nil), &insp)
	if insp.Name != "base" || insp.SourceType != "dockerfile" || !insp.HasImage {
		t.Errorf("inspection = %+v", insp)
	}
	if !strings.Contains(insp.Dockerfile, "busybox") {
		t.Errorf("dockerfile = %q", insp.Dockerfile)
	}
}

// A registry image has no Dockerfile to lose, so it is the image most likely
// to be backed up spec-only, and a spec-only registry backup is nothing but
// the reference: the inspection has to carry it.
func TestInspectTransferCarriesTheRegistryReference(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	var img imageResponse
	env.decode(env.postJSON("/api/images", `{"name":"pulled","sourceType":"registry","registryRef":"node:22-bookworm-slim"}`), &img)
	env.waitForImageStatus(img.ID, store.ImageStatusReady)

	var created transferResponse
	env.decode(env.postJSON("/api/images/"+img.ID+"/backup", `{"withSpec":true,"withImage":false}`), &created)
	env.waitForTransferStatus(created.ID, store.TransferStatusReady)

	var insp restoreInspectionResponse
	env.decode(env.do(http.MethodGet, "/api/transfers/"+created.ID+"/inspection", nil), &insp)
	if insp.RegistryRef != "node:22-bookworm-slim" {
		t.Errorf("registryRef = %q, want the pulled reference", insp.RegistryRef)
	}
}

// A backup still being written has no archive to read yet.
func TestInspectTransferRefusedWhileNotReady(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	now := time.Now()
	created, err := env.store.CreateTransfer(context.Background(), &store.ImageTransfer{
		UserID: env.userID(), Direction: store.TransferDirectionBackup, ImageID: "img-x", Name: "base",
		Status: store.TransferStatusRunning, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	resp := env.do(http.MethodGet, "/api/transfers/"+created.ID+"/inspection", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// Importing a backup over the image it was taken from is a restore in place:
// same id, same local tag, and the tag the load just applied must not be the
// one this removes as the "foreign" tag left behind by the load.
func TestImportAcceptsABackupOverItsOwnSourceImage(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	img := env.readyImage("base")
	originalRef := img.ImageRef

	var created transferResponse
	env.decode(env.postJSON("/api/images/"+img.ID+"/backup", `{"withSpec":true,"withImage":true}`), &created)
	env.waitForTransferStatus(created.ID, store.TransferStatusReady)

	resp := env.postJSON("/api/images/restore/"+created.ID+"/import", `{"name":"base","overwrite":true}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var imported imageResponse
	env.decode(resp, &imported)
	if imported.ID != img.ID {
		t.Errorf("imported id = %q, want the same image kept: %q", imported.ID, img.ID)
	}

	ready := env.waitForImageStatus(imported.ID, store.ImageStatusReady)
	if ready.ImageRef != originalRef {
		t.Errorf("image ref = %q, want the same local tag kept: %q", ready.ImageRef, originalRef)
	}
	for _, ref := range env.docker.removed {
		if ref == originalRef {
			t.Errorf("removed = %v, must not remove the tag the load just applied", env.docker.removed)
		}
	}

	tr, err := env.store.TransferByID(context.Background(), env.userID(), created.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if tr.ImageID != img.ID {
		t.Errorf("backup row's imageId = %q, want the source image %q kept", tr.ImageID, img.ID)
	}
}

// Restoring a backup under a new name must not untag the image it was taken
// from. The manifest's ImageRef is that image's own local tag, not a foreign
// one: only a restore from elsewhere can safely lose the tag it arrived
// under after retagging.
func TestImportingABackupUnderANewNameKeepsTheSourceImageIntact(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	img := env.readyImage("base")
	sourceRef := img.ImageRef

	var created transferResponse
	env.decode(env.postJSON("/api/images/"+img.ID+"/backup", `{"withSpec":true,"withImage":true}`), &created)
	env.waitForTransferStatus(created.ID, store.TransferStatusReady)

	resp := env.postJSON("/api/images/restore/"+created.ID+"/import", `{"name":"base-copy","overwrite":false}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var imported imageResponse
	env.decode(resp, &imported)
	if imported.ID == img.ID {
		t.Fatalf("import under a new name reused the source image's id")
	}
	env.waitForImageStatus(imported.ID, store.ImageStatusReady)

	source, err := env.store.ImageByID(context.Background(), env.userID(), img.ID)
	if err != nil {
		t.Fatalf("ImageByID: %v", err)
	}
	if source.ImageRef != sourceRef || source.Status != store.ImageStatusReady {
		t.Errorf("source image = %+v, want it untouched at %q", source, sourceRef)
	}
	for _, ref := range env.docker.removed {
		if ref == sourceRef {
			t.Errorf("removed = %v, must not untag the still-live source image %q", env.docker.removed, sourceRef)
		}
	}

	tr, err := env.store.TransferByID(context.Background(), env.userID(), created.ID)
	if err != nil {
		t.Fatalf("TransferByID: %v", err)
	}
	if tr.ImageID != img.ID {
		t.Errorf("backup row's imageId = %q, want the source image %q kept", tr.ImageID, img.ID)
	}
}

// A spec-only backup has no image.tar.gz to import, whichever direction asked
// for the archive.
func TestImportingASpecOnlyBackupIsRefused(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	img := env.readyImage("base")

	var created transferResponse
	env.decode(env.postJSON("/api/images/"+img.ID+"/backup", `{"withSpec":true,"withImage":false}`), &created)
	env.waitForTransferStatus(created.ID, store.TransferStatusReady)

	resp := env.postJSON("/api/images/restore/"+created.ID+"/import", `{"name":"base-copy","overwrite":false}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// A restore upload the reader cancelled, or whose import failed, is staged and
// good for 24 hours: it can be inspected again from the transfers list and
// imported later, without uploading again.
func TestAnUploadNeverImportedCanBeInspectedAndImportedLater(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	raw := fullArchive(t, "uploaded", "some/other:latest", []byte("layer-bytes"))
	var insp restoreInspectionResponse
	env.decode(env.postRaw("/api/images/restore", raw, map[string]string{"Content-Type": "application/gzip"}), &insp)

	var again restoreInspectionResponse
	env.decode(env.do(http.MethodGet, "/api/transfers/"+insp.ID+"/inspection", nil), &again)
	if again.Name != "uploaded" || !again.HasImage {
		t.Errorf("re-inspection = %+v", again)
	}

	resp := env.postJSON("/api/images/restore/"+insp.ID+"/import", `{"name":"uploaded","overwrite":false}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var imported imageResponse
	env.decode(resp, &imported)
	env.waitForImageStatus(imported.ID, store.ImageStatusReady)
}
