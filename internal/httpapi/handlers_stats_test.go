package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/hostinfo"
	"github.com/andrea/hexagon/internal/store"
)

// fakeHostSampler reports a canned Snapshot rather than reading /proc, so a
// test can exercise "the host half is unavailable" without a non-Linux build.
type fakeHostSampler struct {
	snap hostinfo.Snapshot
	err  error
}

func (f *fakeHostSampler) Read([]string) (hostinfo.Snapshot, error) { return f.snap, f.err }

func TestStatsEndpointsRequireASession(t *testing.T) {
	env := newTestEnv(t, "alice")

	if got := env.do(http.MethodGet, "/api/stats", nil).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("GET /api/stats = %d, want 401", got)
	}
	if got := env.postJSON("/api/stats/prune/images", "").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("POST /api/stats/prune/images = %d, want 401", got)
	}
	if got := env.postJSON("/api/stats/prune/containers", "").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("POST /api/stats/prune/containers = %d, want 401", got)
	}
}

func TestGetStatsReportsHostUnavailable(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.without(func(d *Deps) {
		d.HostSampler = &fakeHostSampler{snap: hostinfo.Snapshot{Available: false}}
	})
	env.signIn()

	var resp statsResponse
	env.decode(env.do(http.MethodGet, "/api/stats", nil), &resp)
	if resp.Host.Available {
		t.Error("Host.Available = true, want false: this platform has no host reader")
	}
	if resp.Host.CPU != nil || resp.Host.Memory != nil || resp.Host.Filesystems != nil {
		t.Errorf("Host = %+v, want everything but Available left zero", resp.Host)
	}
}

func TestGetStatsUnreachableDockerIs503(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.docker.pingErr = context.DeadlineExceeded

	if got := env.do(http.MethodGet, "/api/stats", nil).StatusCode; got != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", got)
	}
}

func TestGetStatsMarksRegisteredAndInUseImages(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	ctx := context.Background()

	// Registered: a row in images points at it, and no container uses it.
	if _, err := env.store.CreateImage(ctx, &store.Image{
		UserID: env.userID(), Name: "registered", SourceType: store.ImageSourceDockerfile,
		ImageRef: "hexagon/img-registered:latest", Status: store.ImageStatusReady,
	}); err != nil {
		t.Fatalf("create registered image: %v", err)
	}

	env.docker.setImages([]dockerx.ImageSummary{
		{ID: "sha256:registered", Tags: []string{"hexagon/img-registered:latest"}, Size: 100, Created: time.Now()},
		{ID: "sha256:inuse", Tags: []string{"leftover:latest"}, Size: 200, Created: time.Now(), Containers: 1},
		{ID: "sha256:dangling", Tags: nil, Size: 50, Created: time.Now()},
	})

	var resp statsResponse
	env.decode(env.do(http.MethodGet, "/api/stats", nil), &resp)

	byID := map[string]dockerImageResponse{}
	for _, img := range resp.Docker.Images {
		byID[img.ID] = img
	}

	if got := byID["sha256:registered"]; !got.Registered || got.InUse {
		t.Errorf("registered image = %+v, want registered and not in use", got)
	}
	if got := byID["sha256:inuse"]; got.Registered || !got.InUse {
		t.Errorf("in-use image = %+v, want in use and not registered", got)
	}
	if got := byID["sha256:dangling"]; !got.Dangling || got.Registered {
		t.Errorf("dangling image = %+v, want dangling and not registered", got)
	}
}

func TestPruneImagesLeavesRegisteredInUseAndOtherUsersImages(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	ctx := context.Background()
	alice := env.userID()
	bob := testUser2(t, env)

	if _, err := env.store.CreateImage(ctx, &store.Image{
		UserID: alice, Name: "registered", SourceType: store.ImageSourceDockerfile,
		ImageRef: "hexagon/img-registered:latest", Status: store.ImageStatusReady,
	}); err != nil {
		t.Fatalf("create alice's image: %v", err)
	}
	if _, err := env.store.CreateImage(ctx, &store.Image{
		UserID: bob, Name: "bobs", SourceType: store.ImageSourceDockerfile,
		ImageRef: "hexagon/img-bob:latest", Status: store.ImageStatusReady,
	}); err != nil {
		t.Fatalf("create bob's image: %v", err)
	}

	env.docker.setImages([]dockerx.ImageSummary{
		{ID: "sha256:registered", Tags: []string{"hexagon/img-registered:latest"}, Size: 10},
		{ID: "sha256:bobs", Tags: []string{"hexagon/img-bob:latest"}, Size: 20},
		{ID: "sha256:inuse", Tags: []string{"leftover:latest"}, Size: 30, Containers: 1},
		{ID: "sha256:unused", Tags: []string{"stale:latest"}, Size: 40},
		{ID: "sha256:danglingunused", Tags: nil, Size: 5},
	})

	var result pruneResponse
	env.decode(env.postJSON("/api/stats/prune/images", ""), &result)

	if result.Removed != 2 || result.Reclaimed != 45 {
		t.Errorf("result = %+v, want {Removed:2 Reclaimed:45}", result)
	}
	removed := env.docker.removedImages()
	for _, ref := range []string{"sha256:registered", "sha256:bobs", "sha256:inuse"} {
		for _, r := range removed {
			if r == ref {
				t.Errorf("removed protected image %s", ref)
			}
		}
	}
	if !contains(removed, "sha256:unused") || !contains(removed, "sha256:danglingunused") {
		t.Errorf("removed = %v, want the two unused, unregistered images", removed)
	}
}

// The image Hexagon builds for itself to run Claude Code without a host binary
// has no row in the images table, so without this it looks unused to prune the
// moment its own "ask Claude" container — created and removed around every
// call — is gone. Deleting it here is what used to leave the next "ask Claude"
// failing against the daemon with "No such image".
func TestPruneImagesLeavesTheDefaultClaudeImage(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()

	env.docker.setImages([]dockerx.ImageSummary{
		{ID: "sha256:default", Tags: []string{env.defaultImage.Tag()}, Size: 100},
		{ID: "sha256:unused", Tags: []string{"stale:latest"}, Size: 40},
	})

	var result pruneResponse
	env.decode(env.postJSON("/api/stats/prune/images", ""), &result)

	if result.Removed != 1 || result.Reclaimed != 40 {
		t.Errorf("result = %+v, want {Removed:1 Reclaimed:40}", result)
	}
	if contains(env.docker.removedImages(), "sha256:default") {
		t.Error("pruned the image Hexagon runs Claude Code's editor in")
	}

	var stats statsResponse
	env.decode(env.do(http.MethodGet, "/api/stats", nil), &stats)
	for _, img := range stats.Docker.Images {
		if img.ID == "sha256:default" && !img.Registered {
			t.Errorf("default image = %+v, want it reported as registered", img)
		}
	}
}

func TestPruneContainersGoesThroughTheFilteredCall(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.docker.pruneContainers = dockerx.Pruned{Removed: 3, Reclaimed: 999}

	var result pruneResponse
	env.decode(env.postJSON("/api/stats/prune/containers", ""), &result)
	if result.Removed != 3 || result.Reclaimed != 999 {
		t.Errorf("result = %+v, want {Removed:3 Reclaimed:999}", result)
	}
	// The fake's RemoveContainer log must stay empty: a container prune has to
	// go through dockerx.PruneContainers, never a list-then-remove that could
	// pass a Hexagon container to RemoveContainer.
	if len(env.docker.removedContainers) != 0 {
		t.Errorf("removedContainers = %v, want none: prune must not enumerate and remove by hand", env.docker.removedContainers)
	}
}

func TestPruneEndpointsUnreachableDockerIs503(t *testing.T) {
	env := newTestEnv(t, "alice")
	env.signIn()
	env.docker.pingErr = context.DeadlineExceeded

	if got := env.postJSON("/api/stats/prune/images", "").StatusCode; got != http.StatusServiceUnavailable {
		t.Errorf("prune images status = %d, want 503", got)
	}
	if got := env.postJSON("/api/stats/prune/containers", "").StatusCode; got != http.StatusServiceUnavailable {
		t.Errorf("prune containers status = %d, want 503", got)
	}
}

// testUser2 registers a second user directly in the store, the way another
// signed-in account would exist without this test session ever being them.
func testUser2(t *testing.T, env *testEnv) string {
	t.Helper()
	user, err := env.store.UpsertUser(context.Background(), &store.User{GitHubLogin: "bob", GitHubID: 99})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	return user.ID
}
