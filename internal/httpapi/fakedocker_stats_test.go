package httpapi

import (
	"context"

	"github.com/andrea/hexagon/internal/dockerx"
)

// The Stats half of the Docker fake: a daemon-wide image listing, a disk usage
// summary, and a container prune, each set directly by a test rather than
// derived from f.containers — the handler's decisions about what is in use,
// registered or safe to remove are what these tests are checking, and deriving
// the fixtures from the same state the handler reads would hide a bug in that
// logic rather than exercise it.

func (f *fakeDocker) ListImages(context.Context) ([]dockerx.ImageSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listImagesErr != nil {
		return nil, f.listImagesErr
	}
	return append([]dockerx.ImageSummary(nil), f.images...), nil
}

func (f *fakeDocker) DiskUsage(context.Context) (dockerx.DiskUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diskUsageErr != nil {
		return dockerx.DiskUsage{}, f.diskUsageErr
	}
	return f.diskUsage, nil
}

func (f *fakeDocker) PruneContainers(context.Context) (dockerx.Pruned, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pruneErr != nil {
		return dockerx.Pruned{}, f.pruneErr
	}
	return f.pruneContainers, nil
}

// setImages replaces the daemon-wide image listing GET /api/stats reads.
func (f *fakeDocker) setImages(images []dockerx.ImageSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images = images
}

// removedImages returns which refs went through RemoveImage, for a test that
// checks the prune left the right ones standing.
func (f *fakeDocker) removedImages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}
