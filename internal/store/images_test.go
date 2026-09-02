package store

import (
	"context"
	"errors"
	"testing"
)

func testUser(t *testing.T, s *Store, login string, githubID int64) *User {
	t.Helper()
	user, err := s.UpsertUser(context.Background(), &User{
		GitHubLogin: login, GitHubID: githubID,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", login, err)
	}
	return user
}

func TestCreateImageAssignsAnID(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	img, err := s.CreateImage(ctx, &Image{
		UserID: user.ID, Name: "base", SourceType: ImageSourceDockerfile,
		Dockerfile: "FROM busybox", ImageRef: "hexagon/img-1:latest", Status: ImageStatusBuilding,
	})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if img.ID == "" || img.CreatedAt.IsZero() {
		t.Fatalf("CreateImage returned %+v, want an id and a timestamp", img)
	}

	got, err := s.ImageByID(ctx, user.ID, img.ID)
	if err != nil {
		t.Fatalf("ImageByID: %v", err)
	}
	if got.Name != "base" || got.Dockerfile != "FROM busybox" || got.Status != ImageStatusBuilding {
		t.Errorf("stored image = %+v", got)
	}
}

func TestCreateImageRejectsADuplicateNamePerUser(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	alice := testUser(t, s, "alice", 1)
	bob := testUser(t, s, "bob", 2)

	base := func(userID string) *Image {
		return &Image{UserID: userID, Name: "base", SourceType: ImageSourceRegistry,
			RegistryRef: "busybox", ImageRef: "docker.io/library/busybox:latest", Status: ImageStatusReady}
	}

	if _, err := s.CreateImage(ctx, base(alice.ID)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.CreateImage(ctx, base(alice.ID)); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name for the same user: err = %v, want ErrConflict", err)
	}
	// The name only has to be unique per user.
	if _, err := s.CreateImage(ctx, base(bob.ID)); err != nil {
		t.Errorf("same name for another user: %v", err)
	}
}

func TestImageLookupsAreScopedToTheOwner(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	alice := testUser(t, s, "alice", 1)
	bob := testUser(t, s, "bob", 2)

	img, err := s.CreateImage(ctx, &Image{UserID: alice.ID, Name: "base",
		SourceType: ImageSourceDockerfile, Status: ImageStatusReady})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	if _, err := s.ImageByID(ctx, bob.ID, img.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user read the image: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteImage(ctx, bob.ID, img.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user deleted the image: err = %v, want ErrNotFound", err)
	}

	images, err := s.ListImages(ctx, bob.ID)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("listed %d images for a user who owns none", len(images))
	}
}

func TestFinishImageRecordsTheOutcome(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	img, err := s.CreateImage(ctx, &Image{UserID: user.ID, Name: "base",
		SourceType: ImageSourceDockerfile, ImageRef: "hexagon/img-1:latest", Status: ImageStatusBuilding})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	if err := s.SetImageBuildLog(ctx, img.ID, "Step 1/1"); err != nil {
		t.Fatalf("SetImageBuildLog: %v", err)
	}
	if err := s.FinishImage(ctx, img.ID, ImageStatusFailed, img.ImageRef, "Step 1/1\nboom", "boom"); err != nil {
		t.Fatalf("FinishImage: %v", err)
	}

	got, err := s.ImageByID(ctx, user.ID, img.ID)
	if err != nil {
		t.Fatalf("ImageByID: %v", err)
	}
	if got.Status != ImageStatusFailed || got.Error != "boom" || got.BuildLog != "Step 1/1\nboom" {
		t.Errorf("finished image = %+v", got)
	}
}

// A build interrupted by a restart has nobody left to finish it, so it must not
// stay in "building" for ever.
func TestFailInterruptedImageBuilds(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	building, err := s.CreateImage(ctx, &Image{UserID: user.ID, Name: "building",
		SourceType: ImageSourceDockerfile, Status: ImageStatusBuilding})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	ready, err := s.CreateImage(ctx, &Image{UserID: user.ID, Name: "ready",
		SourceType: ImageSourceDockerfile, Status: ImageStatusReady})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	n, err := s.FailInterruptedImageBuilds(ctx)
	if err != nil {
		t.Fatalf("FailInterruptedImageBuilds: %v", err)
	}
	if n != 1 {
		t.Errorf("marked %d images, want 1", n)
	}

	got, _ := s.ImageByID(ctx, user.ID, building.ID)
	if got.Status != ImageStatusFailed || got.Error == "" {
		t.Errorf("interrupted image = %+v, want failed with a reason", got)
	}
	if got, _ := s.ImageByID(ctx, user.ID, ready.ID); got.Status != ImageStatusReady {
		t.Errorf("a ready image was touched: %+v", got)
	}
}

func TestUpdateImageSourceResetsStatusLogAndError(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	img, err := s.CreateImage(ctx, &Image{UserID: user.ID, Name: "base",
		SourceType: ImageSourceDockerfile, ImageRef: "hexagon/img-1:latest", Status: ImageStatusBuilding})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := s.FinishImage(ctx, img.ID, ImageStatusFailed, img.ImageRef, "Step 1/1\nboom", "boom"); err != nil {
		t.Fatalf("FinishImage: %v", err)
	}

	if err := s.UpdateImageSource(ctx, user.ID, img.ID, "FROM busybox\nRUN true", ""); err != nil {
		t.Fatalf("UpdateImageSource: %v", err)
	}

	got, err := s.ImageByID(ctx, user.ID, img.ID)
	if err != nil {
		t.Fatalf("ImageByID: %v", err)
	}
	if got.Dockerfile != "FROM busybox\nRUN true" {
		t.Errorf("Dockerfile = %q, want the new content", got.Dockerfile)
	}
	if got.Status != ImageStatusBuilding || got.BuildLog != "" || got.Error != "" {
		t.Errorf("updated image = %+v, want status building with a cleared log and error", got)
	}
}

func TestUpdateImageSourceReturnsNotFoundForAnotherUsersImage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	alice := testUser(t, s, "alice", 1)
	bob := testUser(t, s, "bob", 2)

	img, err := s.CreateImage(ctx, &Image{UserID: alice.ID, Name: "base",
		SourceType: ImageSourceDockerfile, Status: ImageStatusReady})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	if err := s.UpdateImageSource(ctx, bob.ID, img.ID, "FROM busybox", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user updated the image: err = %v, want ErrNotFound", err)
	}
}

func TestCountSessionsUsingImage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user := testUser(t, s, "alice", 1)

	img, err := s.CreateImage(ctx, &Image{UserID: user.ID, Name: "base",
		SourceType: ImageSourceDockerfile, ImageRef: "hexagon/img-1:latest", Status: ImageStatusReady})
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	n, err := s.CountSessionsUsingImage(ctx, img.ID)
	if err != nil {
		t.Fatalf("CountSessionsUsingImage: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}

	_, err = s.DB().Exec(`
		INSERT INTO sessions (id, user_id, title, repo_full_name, repo_clone_url, branch, image_id,
			image_ref, workspace_dir, repo_dir, container_id, status, error, created_at, updated_at)
		VALUES ('s1', ?, 'demo', 'o/r', 'https://example.test/o/r.git', 'main', ?, ?, '/w', '/w/repo', '', 'running', '',
			'2026-01-01 00:00:00.000', '2026-01-01 00:00:00.000')`, user.ID, img.ID, img.ImageRef)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if n, _ := s.CountSessionsUsingImage(ctx, img.ID); n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}
