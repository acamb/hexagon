// Package dockerx wraps the parts of the Docker Engine API that Hexagon uses.
//
// Everything goes through the API interface so handlers can be tested without a
// running daemon.
package dockerx

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/client"
)

// WorkspaceMount is where a session's repository clone appears inside its
// container. The image does not define it: the clone is bind mounted here.
const WorkspaceMount = "/workspace"

// VSCodeMount is where the host's code-server release appears inside a session
// container, and VSCodePort the port it listens on there. 8443 rather than
// code-server's own 8080, which is the port a project under /workspace is
// likeliest to want for itself.
const VSCodeMount = "/opt/code-server"
const VSCodePort = 8443

// API is the slice of the Docker Engine API Hexagon needs. It grows with each
// milestone; today it covers base images.
type API interface {
	// Ping checks that the daemon is reachable.
	Ping(ctx context.Context) error
	// BuildImage builds dockerfile into tag, writing build output to logs.
	BuildImage(ctx context.Context, dockerfile, tag string, logs io.Writer) error
	// PullImage fetches ref from its registry, writing progress to logs.
	PullImage(ctx context.Context, ref string, logs io.Writer) error
	// RemoveImage deletes a local image.
	RemoveImage(ctx context.Context, ref string) error
	// InspectImage returns the content-addressable id ref currently resolves to.
	InspectImage(ctx context.Context, ref string) (string, error)
	// SaveImage writes ref as a `docker save` tar stream to w, uncompressed:
	// the caller decides whether and how to compress it.
	SaveImage(ctx context.Context, ref string, w io.Writer) error
	// LoadImage reads a `docker save` tar stream from r and loads it into the
	// daemon, writing progress to logs. The tag the loaded image ends up under
	// is whatever it was saved with — the caller learns it from the backup's own
	// manifest, not from this call, and retags it with TagImage.
	LoadImage(ctx context.Context, r io.Reader, logs io.Writer) error
	// TagImage adds target as a second name for the image source already
	// resolves to.
	TagImage(ctx context.Context, source, target string) error
	// ListImages returns every image the daemon holds, with its size and the
	// number of containers based on it.
	ListImages(ctx context.Context) ([]ImageSummary, error)
	// DiskUsage reports what images, containers, volumes and build cache cost
	// on the daemon, the way `docker system df` does.
	DiskUsage(ctx context.Context) (DiskUsage, error)
	// PruneContainers removes every stopped container the daemon holds except
	// Hexagon's own, in one call: the exclusion is a label filter, not a list
	// this package enumerates and deletes by hand.
	PruneContainers(ctx context.Context) (Pruned, error)

	// AttachExec runs an interactive command in a container and returns the
	// attached streams.
	AttachExec(ctx context.Context, req ExecRequest) (*Exec, error)
	// ResizeExec updates the geometry of a running exec's TTY.
	ResizeExec(ctx context.Context, execID string, size TerminalSize) error
	// RunExec runs a command to completion and returns its output and exit code.
	RunExec(ctx context.Context, containerID string, cmd []string) (string, int, error)

	// CreateContainer creates a session container without starting it.
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	// StartContainer starts a created or stopped container.
	StartContainer(ctx context.Context, id string) error
	// StopContainer stops a container, killing it after timeout.
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	// RemoveContainer deletes a container.
	RemoveContainer(ctx context.Context, id string, force bool) error
	// InspectContainer reports a container's real state, or ErrContainerNotFound.
	InspectContainer(ctx context.Context, id string) (ContainerState, error)
	// ListManagedContainers returns every container Hexagon owns.
	ListManagedContainers(ctx context.Context) ([]ManagedContainer, error)
}

// Client talks to a Docker daemon.
type Client struct {
	cli *client.Client
}

var _ API = (*Client)(nil)

// New connects to the daemon. host overrides DOCKER_HOST when set; otherwise
// the usual environment and default socket apply.
func New(host string) (*Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to docker: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Ping checks that the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.cli.Ping(ctx); err != nil {
		return fmt.Errorf("ping docker: %w", err)
	}
	return nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.cli.Close() }
