//go:build !linux

package hostinfo

// Sampler is the non-Linux stand-in. /proc and syscall.Statfs are Linux
// specifics, and Hexagon ships for Linux, so this exists only so `make test`
// on a developer's Mac keeps working: the host half of the Stats page reports
// itself unavailable here rather than failing to build at all.
type Sampler struct{}

// NewSampler returns a Sampler that always reports Snapshot.Available as
// false.
func NewSampler() *Sampler { return &Sampler{} }

// Read touches nothing: there is no /proc to read on this platform.
func (s *Sampler) Read(paths []string) (Snapshot, error) {
	return Snapshot{}, nil
}
