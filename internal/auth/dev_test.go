package auth

import "testing"

func TestIsLoopbackAddr(t *testing.T) {
	loopback := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "127.0.0.1"}
	for _, addr := range loopback {
		if !isLoopbackAddr(addr) {
			t.Errorf("isLoopbackAddr(%q) = false, want true", addr)
		}
	}

	// An empty host means every interface, which is exactly the case the dev
	// bypass must refuse.
	exposed := []string{":8080", "0.0.0.0:8080", "192.168.1.10:8080", "[::]:8080", "example.com:8080"}
	for _, addr := range exposed {
		if isLoopbackAddr(addr) {
			t.Errorf("isLoopbackAddr(%q) = true, want false", addr)
		}
	}
}

func TestNewDevProviderRefusesUnsafeConfigurations(t *testing.T) {
	if _, err := NewDevProvider("alice", "", "127.0.0.1:8080", nil, nil); err == nil {
		t.Error("dev provider accepted an empty GitHub token")
	}
	if _, err := NewDevProvider("alice", "token", "0.0.0.0:8080", nil, nil); err == nil {
		t.Error("dev provider accepted a non-loopback listen address")
	}
	if _, err := NewDevProvider("alice", "token", "127.0.0.1:8080", nil, nil); err != nil {
		t.Errorf("dev provider rejected a valid configuration: %v", err)
	}
}
