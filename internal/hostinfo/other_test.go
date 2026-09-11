//go:build !linux

package hostinfo

import "testing"

func TestSamplerReadReportsUnavailable(t *testing.T) {
	s := NewSampler()
	snap, err := s.Read([]string{"/does/not/exist"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snap.Available {
		t.Error("Available = true, want false: this platform has no /proc or Statfs implementation")
	}
}
