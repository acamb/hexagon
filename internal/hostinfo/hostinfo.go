// Package hostinfo reads the resource figures of the machine a Hexagon server
// runs on: CPU, memory and the filesystems under its data and workspace
// directories.
//
// It does no formatting and keeps no policy — it returns numbers, and lets the
// caller decide what to do with them. The real implementation is Linux only,
// behind a build tag; everything else gets a fallback that reports the host
// half as unavailable rather than failing, so the rest of the server keeps
// working on a platform this package cannot read.
package hostinfo

// CPU is one reading of the host's processor usage.
//
// UsedPercent is nil until a second sample lets it be computed as a delta:
// a single read of /proc/stat yields the average since boot, which is not
// what a page called Stats means by "CPU usage now". Load is reported
// alongside it because it costs one file read, needs no history and answers
// the same question while UsedPercent is not there yet.
type CPU struct {
	Cores       int
	UsedPercent *float64
	Load        [3]float64
}

// Memory is one reading of the host's RAM.
//
// Available is MemAvailable, not MemFree: on a machine that has been up for a
// while MemFree is close to zero and means nothing, because the page cache is
// holding the rest and will give it back on demand. MemAvailable is the
// kernel's own estimate of what a new process could actually get.
type Memory struct {
	Total     uint64
	Available uint64
}

// Filesystem is the space on one mounted filesystem.
type Filesystem struct {
	Path  string
	Total uint64
	Free  uint64
}

// Snapshot is everything one call to Sampler.Read reports.
//
// Available is false on a platform this package has no implementation for; the
// rest of the struct is then zero and should not be read.
type Snapshot struct {
	Available   bool
	CPU         CPU
	Memory      Memory
	Filesystems []Filesystem
}
