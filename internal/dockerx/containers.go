package dockerx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// AgentHome is the writable home directory a session container runs with. It is
// a bind mount too: the container runs as the host user, who has no entry in
// the image's /etc/passwd and therefore no home of their own.
const AgentHome = "/home/agent"

// Labels Hexagon puts on the containers it owns, so they can be found again
// after a restart and told apart from everything else on the machine.
const (
	LabelManaged   = "hexagon.managed"
	LabelSessionID = "hexagon.session.id"
)

// ErrContainerNotFound reports a container the daemon does not know about.
var ErrContainerNotFound = errors.New("container not found")

// ContainerSpec describes a session container.
type ContainerSpec struct {
	Name       string
	Image      string
	Cmd        []string
	Env        []string
	WorkingDir string
	// User is the uid:gid the container runs as. Sessions run as the host user
	// so the files written into the bind mounted clone stay owned by them.
	User   string
	Labels map[string]string
	// Binds are host:container[:ro] mount specifications.
	Binds []string
	// AutoRestart brings the container back after a Docker or machine restart.
	AutoRestart bool
}

// ContainerState is what Hexagon needs to know about a container's real state.
type ContainerState struct {
	ID      string
	Running bool
	// Status is the daemon's own word: created, running, paused, restarting,
	// removing, exited or dead.
	Status string
}

// ManagedContainer is one of Hexagon's containers as the daemon sees it.
type ManagedContainer struct {
	ID        string
	SessionID string
	Running   bool
	Status    string
}

// CreateContainer creates a session container without starting it.
func (c *Client) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	config := &container.Config{
		Image:      spec.Image,
		Cmd:        spec.Cmd,
		Env:        spec.Env,
		WorkingDir: spec.WorkingDir,
		User:       spec.User,
		Labels:     spec.Labels,
		Tty:        false,
	}
	hostConfig := &container.HostConfig{Binds: spec.Binds}
	if spec.AutoRestart {
		hostConfig.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
	}

	created, err := c.cli.ContainerCreate(ctx, config, hostConfig, nil, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	return created.ID, nil
}

// StartContainer starts a created or stopped container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	if err := c.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", translateNotFound(err))
	}
	return nil
}

// StopContainer stops a container, giving it timeout to exit before it is
// killed.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	seconds := int(timeout.Seconds())
	if err := c.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &seconds}); err != nil {
		return fmt.Errorf("stop container: %w", translateNotFound(err))
	}
	return nil
}

// RemoveContainer deletes a container, killing it first when force is set.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	err := c.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
	if err != nil && !client.IsErrNotFound(err) {
		return fmt.Errorf("remove container: %w", err)
	}
	return nil
}

// InspectContainer reports a container's real state, or ErrContainerNotFound.
func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerState, error) {
	inspected, err := c.cli.ContainerInspect(ctx, id)
	if err != nil {
		return ContainerState{}, fmt.Errorf("inspect container: %w", translateNotFound(err))
	}
	state := ContainerState{ID: inspected.ID}
	if inspected.State != nil {
		state.Running = inspected.State.Running
		state.Status = string(inspected.State.Status)
	}
	return state, nil
}

// ListManagedContainers returns every container Hexagon has created, running or
// not. It is how a restarted server finds its sessions again.
func (c *Client) ListManagedContainers(ctx context.Context) ([]ManagedContainer, error) {
	summaries, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelManaged+"=true")),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]ManagedContainer, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, ManagedContainer{
			ID:        summary.ID,
			SessionID: summary.Labels[LabelSessionID],
			Running:   summary.State == "running",
			Status:    summary.State,
		})
	}
	return out, nil
}

// RunExec runs a command to completion inside a container and returns its
// combined output and exit code. It is how a session is bootstrapped once its
// container is up.
func (c *Client) RunExec(ctx context.Context, containerID string, cmd []string) (string, int, error) {
	created, err := c.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", 0, fmt.Errorf("create exec: %w", translateNotFound(err))
	}

	attached, err := c.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("attach exec: %w", err)
	}
	defer attached.Close()

	// Without a TTY the daemon multiplexes stdout and stderr into one stream,
	// so it has to be demultiplexed before it reads as text.
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, attached.Reader); err != nil {
		return out.String(), 0, fmt.Errorf("read exec output: %w", err)
	}

	inspected, err := c.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return out.String(), 0, fmt.Errorf("inspect exec: %w", err)
	}
	return strings.TrimSpace(out.String()), inspected.ExitCode, nil
}

func translateNotFound(err error) error {
	if client.IsErrNotFound(err) {
		return ErrContainerNotFound
	}
	return err
}
