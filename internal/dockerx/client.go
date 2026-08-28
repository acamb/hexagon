// Package dockerx wraps the parts of the Docker Engine API that Hexagon uses.
//
// Everything goes through the API interface so handlers can be tested without a
// running daemon.
package dockerx

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/client"
)

// WorkspaceMount is where a session's repository clone appears inside its
// container. The image does not define it: the clone is bind mounted here.
const WorkspaceMount = "/workspace"

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

	// AttachExec runs an interactive command in a container and returns the
	// attached streams.
	AttachExec(ctx context.Context, req ExecRequest) (*Exec, error)
	// ResizeExec updates the geometry of a running exec's TTY.
	ResizeExec(ctx context.Context, execID string, size TerminalSize) error
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
