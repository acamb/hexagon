package httpapi

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/hostinfo"
)

// HostSampler reads the host's CPU, memory and filesystems for the Stats
// page. *hostinfo.Sampler satisfies it; a test substitutes a fake that reports
// the host half as unavailable, which the real implementation only does on a
// platform this package cannot build a test for.
type HostSampler interface {
	Read(paths []string) (hostinfo.Snapshot, error)
}

type statsResponse struct {
	Host   hostStatsResponse   `json:"host"`
	Docker dockerStatsResponse `json:"docker"`
}

type hostStatsResponse struct {
	Available   bool                     `json:"available"`
	CPU         *hostCPUResponse         `json:"cpu,omitempty"`
	Memory      *hostMemoryResponse      `json:"memory,omitempty"`
	Filesystems []hostFilesystemResponse `json:"filesystems,omitempty"`
}

type hostCPUResponse struct {
	Cores int `json:"cores"`
	// UsedPercent is absent on the first read after startup: hostinfo has only
	// one sample so far, and a percentage computed against boot would be wrong
	// in a way nobody would notice.
	UsedPercent *float64   `json:"usedPercent,omitempty"`
	Load        [3]float64 `json:"load"`
}

type hostMemoryResponse struct {
	Total     uint64 `json:"total"`
	Available uint64 `json:"available"`
}

type hostFilesystemResponse struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

type dockerStatsResponse struct {
	Host string `json:"host"`
	// SameMachine is false when Host names a daemon reachable over the network
	// rather than the one this process runs on: the host gauges and the Docker
	// figures then describe two different computers, and adding them up would
	// be meaningless.
	SameMachine bool                  `json:"sameMachine"`
	Usage       dockerUsageResponse   `json:"usage"`
	Images      []dockerImageResponse `json:"images"`
}

type dockerUsageResponse struct {
	Images     int64 `json:"images"`
	Containers int64 `json:"containers"`
	Volumes    int64 `json:"volumes"`
	BuildCache int64 `json:"buildCache"`
}

type dockerImageResponse struct {
	ID         string    `json:"id"`
	Tags       []string  `json:"tags"`
	Size       int64     `json:"size"`
	Created    time.Time `json:"created"`
	Containers int64     `json:"containers"`
	// InUse means a container, running or stopped, is based on this image.
	InUse bool `json:"inUse"`
	// Registered means Hexagon keeps this image: a row in the images table
	// points at it, or it is the default image Hexagon built for itself. It can
	// be true while InUse is false: the image is idle right now and the prune
	// leaves it alone anyway, because the next session from it should not have
	// to rebuild.
	Registered bool `json:"registered"`
	Dangling   bool `json:"dangling"`
}

// handleGetStats reports the host's resource figures and the daemon's own
// image and disk accounting. The host half is read first and independently of
// Docker: a daemon that is down should not hide the CPU and memory figures the
// operator opened the page for.
func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	host, err := s.hostSampler.Read([]string{s.cfg.DataDir, s.cfg.WorkspaceRoot})
	if err != nil {
		s.log.Error("read host stats", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read host stats")
		return
	}

	if err := s.docker.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker is unreachable")
		return
	}

	images, err := s.docker.ListImages(r.Context())
	if err != nil {
		s.log.Error("list docker images", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot list docker images")
		return
	}
	usage, err := s.docker.DiskUsage(r.Context())
	if err != nil {
		s.log.Error("docker disk usage", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read docker disk usage")
		return
	}
	registered, err := s.protectedImageRefs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot list registered images")
		return
	}

	writeJSON(w, http.StatusOK, statsResponse{
		Host: newHostStatsResponse(host),
		Docker: dockerStatsResponse{
			Host:        s.cfg.DockerHost,
			SameMachine: dockerHostIsLocal(s.cfg.DockerHost),
			Usage: dockerUsageResponse{
				Images:     usage.Images,
				Containers: usage.Containers,
				Volumes:    usage.Volumes,
				BuildCache: usage.BuildCache,
			},
			Images: newDockerImagesResponse(images, registered),
		},
	})
}

// protectedImageRefs is every image tag the prune must leave alone: the ones
// Hexagon rows point at, plus the default image Hexagon builds for itself.
//
// That last one has no row anywhere, so before it was named here the prune
// removed it as unused and the next browser login or source edit failed with
// the daemon's "No such image" — the image being idle is precisely its normal
// state. Only the tag in use now is protected: an older hexagon-default hash,
// left over from a change to the reference Dockerfile, is exactly what a prune
// is for.
//
// It logs its own failure so every caller reports the same message.
func (s *Server) protectedImageRefs(ctx context.Context) (map[string]bool, error) {
	refs, err := s.store.AllImageRefs(ctx)
	if err != nil {
		s.log.Error("list all image refs", "err", err)
		return nil, err
	}
	set := make(map[string]bool, len(refs)+1)
	for _, ref := range refs {
		set[ref] = true
	}
	if s.defaultImage != nil {
		set[s.defaultImage.Ref()] = true
	}
	return set, nil
}

func newHostStatsResponse(snap hostinfo.Snapshot) hostStatsResponse {
	if !snap.Available {
		return hostStatsResponse{Available: false}
	}
	filesystems := make([]hostFilesystemResponse, 0, len(snap.Filesystems))
	for _, fs := range snap.Filesystems {
		filesystems = append(filesystems, hostFilesystemResponse{Path: fs.Path, Total: fs.Total, Free: fs.Free})
	}
	return hostStatsResponse{
		Available: true,
		CPU: &hostCPUResponse{
			Cores:       snap.CPU.Cores,
			UsedPercent: snap.CPU.UsedPercent,
			Load:        snap.CPU.Load,
		},
		Memory:      &hostMemoryResponse{Total: snap.Memory.Total, Available: snap.Memory.Available},
		Filesystems: filesystems,
	}
}

func newDockerImagesResponse(images []dockerx.ImageSummary, registered map[string]bool) []dockerImageResponse {
	out := make([]dockerImageResponse, 0, len(images))
	for _, img := range images {
		out = append(out, dockerImageResponse{
			ID:         img.ID,
			Tags:       img.Tags,
			Size:       img.Size,
			Created:    img.Created,
			Containers: img.Containers,
			InUse:      img.Containers > 0,
			Registered: imageIsRegistered(img, registered),
			Dangling:   len(img.Tags) == 0,
		})
	}
	return out
}

// imageIsRegistered reports whether any tag of img is a Hexagon image's
// image_ref. A dangling image, which has no tags, is never registered: an
// image row always names a tag, never a bare id.
func imageIsRegistered(img dockerx.ImageSummary, registered map[string]bool) bool {
	for _, tag := range img.Tags {
		if registered[tag] {
			return true
		}
	}
	return false
}

// dockerHostIsLocal reports whether host names the daemon this process itself
// talks to over loopback or a local socket, as opposed to one reachable only
// over the network. An empty host is the SDK's own default, which is always
// local.
func dockerHostIsLocal(host string) bool {
	if host == "" {
		return true
	}
	scheme, rest, ok := strings.Cut(host, "://")
	if !ok {
		return false
	}
	switch scheme {
	case "unix", "npipe":
		return true
	case "tcp":
		h, _, err := net.SplitHostPort(rest)
		if err != nil {
			h = rest
		}
		if h == "localhost" {
			return true
		}
		ip := net.ParseIP(h)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

type pruneResponse struct {
	Removed   int    `json:"removed"`
	Reclaimed uint64 `json:"reclaimed"`
}

// handlePruneImages removes every image no container is based on and
// protectedImageRefs does not name. It never uses ImagesPrune: that would
// delete the image behind every stopped session, which stays registered on
// purpose so the next start does not have to rebuild.
func (s *Server) handlePruneImages(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker is unreachable")
		return
	}

	images, err := s.docker.ListImages(r.Context())
	if err != nil {
		s.log.Error("list docker images", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot prune images")
		return
	}
	registered, err := s.protectedImageRefs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot prune images")
		return
	}

	var result pruneResponse
	for _, img := range images {
		if img.Containers > 0 || imageIsRegistered(img, registered) {
			continue
		}
		if err := s.docker.RemoveImage(r.Context(), img.ID); err != nil {
			s.log.Warn("prune image", "id", img.ID, "err", err)
			continue
		}
		result.Removed++
		result.Reclaimed += uint64(img.Size)
	}
	s.log.Info("images pruned", "removed", result.Removed, "reclaimed", result.Reclaimed)
	writeJSON(w, http.StatusOK, result)
}

// handlePruneContainers removes every stopped container the daemon holds
// except Hexagon's own, through the single filtered call dockerx.PruneContainers
// makes: there is no list-then-delete here for a Hexagon container to slip
// through.
func (s *Server) handlePruneContainers(w http.ResponseWriter, r *http.Request) {
	if err := s.docker.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker is unreachable")
		return
	}

	pruned, err := s.docker.PruneContainers(r.Context())
	if err != nil {
		s.log.Error("prune containers", "err", err)
		writeError(w, http.StatusInternalServerError, "cannot prune containers")
		return
	}
	s.log.Info("containers pruned", "removed", pruned.Removed, "reclaimed", pruned.Reclaimed)
	writeJSON(w, http.StatusOK, pruneResponse{Removed: pruned.Removed, Reclaimed: pruned.Reclaimed})
}
