package composex

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// AgentService is the name Hexagon's own service takes in the project it
// assembles. A user's file cannot use it: compose merges services by name
// across files, so a second definition under this name would rewrite the one
// container every session invariant lives in.
const AgentService = "hexagon"

// document is as much of a normalized compose file as the refusals below need.
// Everything here is read from `docker compose config --format json`, which has
// already resolved the short and long form of every key, so there is one shape
// to look at rather than the several a file can be written in.
type document struct {
	Services map[string]service `json:"services"`
}

type service struct {
	Privileged  bool              `json:"privileged"`
	CapAdd      []json.RawMessage `json:"cap_add"`
	SecurityOpt []json.RawMessage `json:"security_opt"`
	Devices     []json.RawMessage `json:"devices"`
	NetworkMode string            `json:"network_mode"`
	PID         string            `json:"pid"`
	IPC         string            `json:"ipc"`
	UTS         string            `json:"uts"`
	User        string            `json:"user"`
	Volumes     []volume          `json:"volumes"`
	Build       json.RawMessage   `json:"build"`
	Ports       []port            `json:"ports"`
}

type volume struct {
	// Type is bind, volume or tmpfs. Only bind reaches the host filesystem.
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
}

type port struct {
	Target int `json:"target"`
	// Published is the host side. It arrives as a string in current compose and
	// as a number in older output, so it is read raw and only tested for being
	// there at all.
	Published json.RawMessage `json:"published"`
}

// check refuses a normalized compose document that asks for something a session
// must not be able to have, and returns the names of the services it describes.
//
// This is a denylist, and it is a denylist over the normalized document rather
// than over the source text on purpose: one over the text would be dodged by
// writing the same request another way, and this cannot be. What it upholds is
// the invariant in AGENTS.md — containers run as the host user, never as root,
// and the container boundary is the whole of the protection. A Dockerfile can
// already run anything its author likes inside a container; a compose file is
// the first thing a user of Hexagon could write that asks to leave one.
func check(normalized []byte) ([]string, error) {
	var doc document
	if err := json.Unmarshal(normalized, &doc); err != nil {
		return nil, fmt.Errorf("read the normalized compose file: %w", err)
	}
	if len(doc.Services) == 0 {
		return nil, fmt.Errorf("this compose file describes no services")
	}

	names := make([]string, 0, len(doc.Services))
	for name := range doc.Services {
		names = append(names, name)
	}
	// Sorted so a file with two problems is always refused for the same one,
	// and a test can say which.
	slices.Sort(names)

	for _, name := range names {
		if err := checkService(name, doc.Services[name]); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func checkService(name string, s service) error {
	refuse := func(key, why string) error {
		return fmt.Errorf("service %q: %s is not allowed here: %s", name, key, why)
	}

	if name == AgentService {
		return fmt.Errorf("service %q: that name is Hexagon's own service, which it adds to every project: rename it",
			AgentService)
	}

	const boundary = "the container boundary is the whole of the protection"
	switch {
	case s.Privileged:
		return refuse("privileged", boundary)
	case len(s.CapAdd) > 0:
		return refuse("cap_add", boundary)
	case len(s.SecurityOpt) > 0:
		return refuse("security_opt", boundary)
	case len(s.Devices) > 0:
		return refuse("devices", boundary)
	}

	for key, value := range map[string]string{
		"network_mode": s.NetworkMode,
		"pid":          s.PID,
		"ipc":          s.IPC,
		"uts":          s.UTS,
	} {
		if value == "host" {
			return refuse(key+": host", "it is the same boundary, left by another door")
		}
	}

	if isRoot(s.User) {
		return refuse("user "+s.User, "containers run as the host user, and never as root")
	}
	for _, v := range s.Volumes {
		if v.Type == "bind" {
			return refuse("the host path "+v.Source+" in volumes",
				"a bind mount of / , or of the Docker socket, is a root shell on this machine. Named volumes are fine")
		}
	}
	if len(s.Build) > 0 && string(s.Build) != "null" {
		return refuse("build", "there is no build context at session time: the Dockerfile is the one thing that gets built")
	}
	for _, p := range s.Ports {
		if published(p.Published) {
			return refuse(fmt.Sprintf("the fixed host port on %d in ports", p.Target),
				"two sessions of one project would fight over it, and the form without a host side works")
		}
	}
	return nil
}

// isRoot reports whether a service's user resolves to uid 0. Compose passes the
// value through untouched, so both spellings have to be recognised: "root" and
// "0", with or without a group after them.
func isRoot(user string) bool {
	uid, _, _ := strings.Cut(user, ":")
	return uid == "0" || uid == "root"
}

// published reports whether a port entry names a host port. Absent, null and
// the empty string all mean "let Docker choose", which is the form that works.
func published(raw json.RawMessage) bool {
	switch value := strings.TrimSpace(string(raw)); value {
	case "", "null", `""`, "0", `"0"`:
		return false
	default:
		return true
	}
}
