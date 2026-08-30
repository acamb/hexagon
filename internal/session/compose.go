package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/andrea/hexagon/internal/composex"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/store"
)

// Compose runs the project a session made from an advanced image lives in. It
// is nil when the server has no `docker compose`, which is a state advanced
// images are refused for rather than one that fails at the first session.
type Compose interface {
	// Validate refuses a compose file that asks for something a session must
	// not be able to have, and returns the names of the services it describes.
	Validate(ctx context.Context, content string) ([]string, error)
	Up(ctx context.Context, p composex.Project) error
	Start(ctx context.Context, p composex.Project) error
	Stop(ctx context.Context, p composex.Project) error
	Down(ctx context.Context, p composex.Project, volumes bool) error
}

// serviceRole is the dockerx.LabelRole value the user's own services carry. It
// is what stops the reconciler reporting every database as a container that
// lost its session.
const serviceRole = "service"

// composeProject is where a session's project lives and what it is called.
//
// The name is the container name prefix and the session id, so `docker compose
// ls` on the host lines up with `docker ps`, and two sessions from one image
// never share a project.
func (m *Manager) composeProject(session *store.Session) composex.Project {
	dir := filepath.Join(session.WorkspaceDir, "compose")
	return composex.Project{
		Name: containerNamePrefix + session.ID,
		Dir:  dir,
		// The user's file first: compose merges later files over earlier ones,
		// so Hexagon's own service and the labels it puts on the user's
		// services win.
		Files: []string{filepath.Join(dir, "user.yaml"), filepath.Join(dir, "hexagon.yaml")},
	}
}

// writeComposeFiles puts the two halves of the project on disk: the user's file
// as it was written, and Hexagon's own, rendered from the very ContainerSpec
// the plain path would have created a container from.
//
// That last part is the whole point of this function. Two hand-written lists of
// binds, of environment variables and of labels would drift inside a single
// milestone; one list rendered two ways cannot.
func writeComposeFiles(dir, userFile string, spec dockerx.ContainerSpec, services []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create compose directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user.yaml"), []byte(userFile), 0o600); err != nil {
		return fmt.Errorf("write the compose file: %w", err)
	}

	overlay, err := composeOverlay(spec, services)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "hexagon.yaml"), overlay, 0o600); err != nil {
		return fmt.Errorf("write the generated compose file: %w", err)
	}
	return nil
}

// composeFile is as much of the compose format as Hexagon writes.
//
// It is marshalled as JSON, and the file is still called .yaml, because JSON is
// a subset of YAML and compose parses it happily. That is not a trick to save a
// line: a YAML encoder would be a dependency in go.mod, added for a document
// this package fully controls and nobody hand-edits.
type composeFile struct {
	Services map[string]composeService `json:"services"`
}

type composeService struct {
	ContainerName string            `json:"container_name,omitempty"`
	Image         string            `json:"image,omitempty"`
	Command       []string          `json:"command,omitempty"`
	Environment   []string          `json:"environment,omitempty"`
	WorkingDir    string            `json:"working_dir,omitempty"`
	User          string            `json:"user,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	// Volumes take the same host:container[:ro] form dockerx.ContainerSpec.Binds
	// already has, so the list crosses over untouched.
	Volumes   []string `json:"volumes,omitempty"`
	Restart   string   `json:"restart,omitempty"`
	Ports     []string `json:"ports,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// composeOverlay renders the file Hexagon adds to the user's: its own service,
// and a labels-only stanza for each of the user's, which compose merges into
// their definitions.
//
// Generating the agent service rather than attaching to one the user wrote is
// the decision this whole shape rests on. Every invariant a session container
// carries — the workspace bind mount, the host uid, the labels the reconciler
// matches on, never as root — would otherwise move into a YAML file that
// Hexagon would then have to police.
func composeOverlay(spec dockerx.ContainerSpec, services []string) ([]byte, error) {
	ports := make([]string, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		// Host ip, no host port, container port: loopback with the host side
		// left to Docker, exactly as dockerx publishes one.
		ports = append(ports, "127.0.0.1::"+strconv.Itoa(p))
	}

	agent := composeService{
		// The name the plain path already uses, so the terminal, the bootstrap
		// and the VS Code proxy go on finding the container by it: Docker takes
		// a name wherever it takes an id.
		ContainerName: spec.Name,
		Image:         spec.Image,
		Command:       spec.Cmd,
		Environment:   spec.Env,
		WorkingDir:    spec.WorkingDir,
		User:          spec.User,
		Labels:        spec.Labels,
		Volumes:       spec.Binds,
		Ports:         ports,
		// The agent starts last: it is the one thing in the project that exists
		// to use the others.
		DependsOn: services,
	}
	if spec.AutoRestart {
		agent.Restart = "unless-stopped"
	}

	file := composeFile{Services: map[string]composeService{composex.AgentService: agent}}
	for _, name := range services {
		file.Services[name] = composeService{Labels: map[string]string{
			dockerx.LabelManaged:   "true",
			dockerx.LabelSessionID: spec.Labels[dockerx.LabelSessionID],
			dockerx.LabelRole:      serviceRole,
		}}
	}

	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render the compose file: %w", err)
	}
	return append(out, '\n'), nil
}
