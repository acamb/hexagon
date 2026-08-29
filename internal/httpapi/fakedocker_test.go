package httpapi

import (
	"context"
	"fmt"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
)

// The container half of the Docker fake: enough state to answer the questions
// the session manager asks, and to let a test see what it was told to create.

type fakeContainer struct {
	spec    dockerx.ContainerSpec
	running bool
	// ports is the host binding InspectContainer reports for each of the
	// container's published ports, set by a test through setContainerPort.
	ports map[int]int
}

func (f *fakeDocker) CreateContainer(_ context.Context, spec dockerx.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.nextContainer++
	id := fmt.Sprintf("container-%d", f.nextContainer)
	if f.containers == nil {
		f.containers = map[string]*fakeContainer{}
	}
	f.containers[id] = &fakeContainer{spec: spec}
	return id, nil
}

func (f *fakeDocker) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	container, ok := f.containers[id]
	if !ok {
		return dockerx.ErrContainerNotFound
	}
	container.running = true
	return nil
}

func (f *fakeDocker) StopContainer(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	container, ok := f.containers[id]
	if !ok {
		return dockerx.ErrContainerNotFound
	}
	container.running = false
	return nil
}

func (f *fakeDocker) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
	f.removedContainers = append(f.removedContainers, id)
	return nil
}

func (f *fakeDocker) InspectContainer(_ context.Context, id string) (dockerx.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	container, ok := f.containers[id]
	if !ok {
		return dockerx.ContainerState{}, dockerx.ErrContainerNotFound
	}
	status := "exited"
	if container.running {
		status = "running"
	}
	return dockerx.ContainerState{ID: id, Running: container.running, Status: status, Ports: container.ports}, nil
}

func (f *fakeDocker) ListManagedContainers(context.Context) ([]dockerx.ManagedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dockerx.ManagedContainer, 0, len(f.containers))
	for id, container := range f.containers {
		out = append(out, dockerx.ManagedContainer{
			ID:        id,
			SessionID: container.spec.Labels[dockerx.LabelSessionID],
			Running:   container.running,
		})
	}
	return out, nil
}

func (f *fakeDocker) RunExec(_ context.Context, containerID string, cmd []string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ranExecs = append(f.ranExecs, cmd)
	if f.runExecErr != nil {
		return "", 0, f.runExecErr
	}
	if _, ok := f.containers[containerID]; !ok {
		return "", 0, dockerx.ErrContainerNotFound
	}
	return f.runExecOutput, f.runExecCode, nil
}

// addContainer registers a container that already exists, for tests whose
// fixtures are written straight into the database.
func (f *fakeDocker) addContainer(id string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containers == nil {
		f.containers = map[string]*fakeContainer{}
	}
	f.containers[id] = &fakeContainer{running: running}
}

// setContainerPort makes InspectContainer report a host binding for one of a
// container's published ports, the way a running container would once Docker
// has picked one.
func (f *fakeDocker) setContainerPort(id string, containerPort, hostPort int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	container, ok := f.containers[id]
	if !ok {
		return
	}
	if container.ports == nil {
		container.ports = map[int]int{}
	}
	container.ports[containerPort] = hostPort
}

// containerSpecs returns what was asked of CreateContainer, by container id.
func (f *fakeDocker) containerSpecs() map[string]dockerx.ContainerSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]dockerx.ContainerSpec{}
	for id, container := range f.containers {
		out[id] = container.spec
	}
	return out
}

func (f *fakeDocker) bootstrapCommands() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.ranExecs...)
}
