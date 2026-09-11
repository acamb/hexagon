package dockerx

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
)

// DiskUsage is what images, containers, volumes and build cache cost on the
// daemon, the way `docker system df` reports it. Containers is the writable
// layer only (SizeRw): the read-only layers underneath are already counted in
// Images, and summing both would double the same bytes.
type DiskUsage struct {
	Images     int64
	Containers int64
	Volumes    int64
	BuildCache int64
}

// DiskUsage asks the daemon what its own accounting says images, containers,
// volumes and build cache cost. It is not derived from ListImages or from a
// walk of the host filesystem: a size is the daemon's to know.
func (c *Client) DiskUsage(ctx context.Context) (DiskUsage, error) {
	usage, err := c.cli.DiskUsage(ctx, types.DiskUsageOptions{})
	if err != nil {
		return DiskUsage{}, fmt.Errorf("disk usage: %w", err)
	}

	var out DiskUsage
	for _, img := range usage.Images {
		out.Images += img.Size
	}
	for _, ctr := range usage.Containers {
		out.Containers += ctr.SizeRw
	}
	for _, vol := range usage.Volumes {
		if vol.UsageData != nil && vol.UsageData.Size > 0 {
			out.Volumes += vol.UsageData.Size
		}
	}
	for _, cache := range usage.BuildCache {
		out.BuildCache += cache.Size
	}
	return out, nil
}

// Pruned is what a prune call removed.
type Pruned struct {
	Removed   int
	Reclaimed uint64
}

// PruneContainers removes every stopped container the daemon holds except the
// ones Hexagon manages. Every container Hexagon creates carries
// LabelManaged=true, so the exclusion is expressible in one filter — nothing
// has to be listed and matched by hand, and a container created between a list
// and a delete cannot slip through a race that does not exist here.
func (c *Client) PruneContainers(ctx context.Context) (Pruned, error) {
	report, err := c.cli.ContainersPrune(ctx, filters.NewArgs(filters.Arg("label!", LabelManaged+"=true")))
	if err != nil {
		return Pruned{}, fmt.Errorf("prune containers: %w", err)
	}
	return Pruned{Removed: len(report.ContainersDeleted), Reclaimed: report.SpaceReclaimed}, nil
}
