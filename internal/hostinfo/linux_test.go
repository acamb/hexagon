//go:build linux

package hostinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const statSampleOne = `cpu  100 0 50 850 0 0 0 0 0 0
cpu0 100 0 50 850 0 0 0 0 0 0
`

// statSampleTwo advances every counter from statSampleOne by a known amount:
// 40 more busy jiffies (20 user, 20 system) and 60 more idle ones (50 idle, 10
// iowait), for a total delta of 100 and a usage of 40%.
const statSampleTwo = `cpu  120 0 70 900 10 0 0 0 0 0
cpu0 120 0 70 900 10 0 0 0 0 0
`

const memInfoSample = `MemTotal:       32827004 kB
MemFree:          412345 kB
MemAvailable:   20805332 kB
Buffers:          123456 kB
`

const loadAvgSample = "0.40 0.60 0.80 1/523 12345\n"

func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestReadMemoryReportsMemAvailableNotMemFree(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "meminfo", memInfoSample)

	mem, err := readMemory(path)
	if err != nil {
		t.Fatalf("readMemory: %v", err)
	}
	if want := uint64(32827004 * 1024); mem.Total != want {
		t.Errorf("Total = %d, want %d", mem.Total, want)
	}
	if want := uint64(20805332 * 1024); mem.Available != want {
		t.Errorf("Available = %d, want MemAvailable (%d), not MemFree", mem.Available, want)
	}
}

func TestReadMemoryMalformedFileNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "meminfo", "not meminfo at all\n")

	_, err := readMemory(path)
	if err == nil {
		t.Fatal("readMemory accepted a file with no MemTotal or MemAvailable")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name %s", err, path)
	}
}

func TestSamplerReadTwoSamplesProduceThePercentTheDeltaImplies(t *testing.T) {
	dir := t.TempDir()
	statPath := writeFixture(t, dir, "stat", statSampleOne)
	memPath := writeFixture(t, dir, "meminfo", memInfoSample)
	loadPath := writeFixture(t, dir, "loadavg", loadAvgSample)

	s := newSamplerAt(statPath, memPath, loadPath)

	first, err := s.Read(nil)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if first.CPU.UsedPercent != nil {
		t.Errorf("first read UsedPercent = %v, want nil: a single sample is the average since boot", *first.CPU.UsedPercent)
	}

	if err := os.WriteFile(statPath, []byte(statSampleTwo), 0o600); err != nil {
		t.Fatalf("write second stat sample: %v", err)
	}
	second, err := s.Read(nil)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if second.CPU.UsedPercent == nil {
		t.Fatal("second read UsedPercent = nil, want a value now there are two samples")
	}
	if got, want := *second.CPU.UsedPercent, 40.0; got != want {
		t.Errorf("UsedPercent = %v, want %v", got, want)
	}
	if second.CPU.Load != [3]float64{0.40, 0.60, 0.80} {
		t.Errorf("Load = %v, want the sample's three values", second.CPU.Load)
	}
}

func TestReadCPUTicksMalformedFileNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "stat", "cpu  not a number\n")

	_, err := readCPUTicks(path)
	if err == nil {
		t.Fatal("readCPUTicks accepted a malformed cpu line")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name %s", err, path)
	}
}

func TestReadFilesystemsDropsASecondPathOnTheSameDevice(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", sub, err)
	}

	filesystems, err := readFilesystems([]string{dir, sub})
	if err != nil {
		t.Fatalf("readFilesystems: %v", err)
	}
	if len(filesystems) != 1 {
		t.Fatalf("filesystems = %v, want one entry: both paths are on the same device", filesystems)
	}
	if filesystems[0].Path != dir {
		t.Errorf("Path = %q, want the first path (%q)", filesystems[0].Path, dir)
	}
	if filesystems[0].Total == 0 {
		t.Error("Total = 0, want the real size of the filesystem under t.TempDir()")
	}
}
