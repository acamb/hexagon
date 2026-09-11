//go:build linux

package hostinfo

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Sampler reads /proc and the filesystems it is pointed at. It keeps the
// previous CPU sample so Read can report a percentage rather than the average
// since boot, which is all a single read of /proc/stat yields.
type Sampler struct {
	statPath string
	memPath  string
	loadPath string

	mu   sync.Mutex
	last *cpuTicks
}

// NewSampler reads the real /proc.
func NewSampler() *Sampler {
	return &Sampler{statPath: "/proc/stat", memPath: "/proc/meminfo", loadPath: "/proc/loadavg"}
}

// newSamplerAt points a Sampler at fixture files instead of /proc, so tests
// exercise the parsing without depending on the machine they run on.
func newSamplerAt(statPath, memPath, loadPath string) *Sampler {
	return &Sampler{statPath: statPath, memPath: memPath, loadPath: loadPath}
}

// cpuTicks is one /proc/stat sample, in jiffies since boot. Read reports a
// percentage from the delta between two of these, never from one alone.
type cpuTicks struct {
	idle  uint64
	total uint64
}

// Read reports the host's current CPU, memory and load, plus the free space on
// each of paths. A path on the same device as one already reported is left
// out, so a data directory and workspace root that share a disk are not drawn
// as two bars for one number.
func (s *Sampler) Read(paths []string) (Snapshot, error) {
	ticks, err := readCPUTicks(s.statPath)
	if err != nil {
		return Snapshot{}, err
	}

	s.mu.Lock()
	prev := s.last
	s.last = &ticks
	s.mu.Unlock()

	cpu := CPU{Cores: runtime.NumCPU()}
	if prev != nil {
		if deltaTotal := ticks.total - prev.total; deltaTotal > 0 {
			percent := (1 - float64(ticks.idle-prev.idle)/float64(deltaTotal)) * 100
			cpu.UsedPercent = &percent
		}
	}

	cpu.Load, err = readLoadAvg(s.loadPath)
	if err != nil {
		return Snapshot{}, err
	}

	mem, err := readMemory(s.memPath)
	if err != nil {
		return Snapshot{}, err
	}

	filesystems, err := readFilesystems(paths)
	if err != nil {
		return Snapshot{}, err
	}

	return Snapshot{Available: true, CPU: cpu, Memory: mem, Filesystems: filesystems}, nil
}

// readCPUTicks parses the aggregate "cpu " line of /proc/stat: user, nice,
// system, idle, iowait, irq, softirq, steal, and on newer kernels guest and
// guest_nice. Idle is idle plus iowait — the kernel counts a CPU waiting on
// disk as not idle, but nothing was using it either.
func readCPUTicks(path string) (cpuTicks, error) {
	f, err := os.Open(path)
	if err != nil {
		return cpuTicks{}, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var ticks cpuTicks
		for i, field := range fields[1:] {
			v, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuTicks{}, fmt.Errorf("read %s: field %d: %w", path, i+1, err)
			}
			ticks.total += v
			if i == 3 || i == 4 { // idle, iowait
				ticks.idle += v
			}
		}
		return ticks, nil
	}
	if err := scanner.Err(); err != nil {
		return cpuTicks{}, fmt.Errorf("read %s: %w", path, err)
	}
	return cpuTicks{}, fmt.Errorf("read %s: no cpu line", path)
}

// readMemory parses MemTotal and MemAvailable out of /proc/meminfo.
// MemAvailable, not MemFree: see the doc comment on Memory.
func readMemory(path string) (Memory, error) {
	f, err := os.Open(path)
	if err != nil {
		return Memory{}, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	var mem Memory
	var sawTotal, sawAvailable bool
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		var target *uint64
		switch fields[0] {
		case "MemTotal:":
			target, sawTotal = &mem.Total, true
		case "MemAvailable:":
			target, sawAvailable = &mem.Available, true
		default:
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return Memory{}, fmt.Errorf("read %s: %s: %w", path, fields[0], err)
		}
		*target = kb * 1024
	}
	if err := scanner.Err(); err != nil {
		return Memory{}, fmt.Errorf("read %s: %w", path, err)
	}
	if !sawTotal || !sawAvailable {
		return Memory{}, fmt.Errorf("read %s: missing MemTotal or MemAvailable", path)
	}
	return mem, nil
}

// readLoadAvg parses the three load averages /proc/loadavg starts with.
func readLoadAvg(path string) ([3]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return [3]float64{}, fmt.Errorf("read %s: %w", path, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return [3]float64{}, fmt.Errorf("read %s: too few fields", path)
	}
	var load [3]float64
	for i := range load {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return [3]float64{}, fmt.Errorf("read %s: field %d: %w", path, i, err)
		}
		load[i] = v
	}
	return load, nil
}

// readFilesystems statfs's each path and drops one that lands on a device
// already reported by an earlier path in the list.
func readFilesystems(paths []string) ([]Filesystem, error) {
	seen := map[syscall.Fsid]bool{}
	var out []Filesystem
	for _, path := range paths {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err != nil {
			return nil, fmt.Errorf("statfs %s: %w", path, err)
		}
		if seen[stat.Fsid] {
			continue
		}
		seen[stat.Fsid] = true

		blockSize := uint64(stat.Bsize)
		out = append(out, Filesystem{
			Path:  path,
			Total: stat.Blocks * blockSize,
			// Bavail, not Bfree: Bfree includes the blocks the kernel reserves
			// for root, which is not space the server could actually fill.
			Free: stat.Bavail * blockSize,
		})
	}
	return out, nil
}
