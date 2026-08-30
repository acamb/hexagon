package auth

import (
	"testing"
)

func TestGateIsEmptyUntilItIsConfigured(t *testing.T) {
	gate := NewGate(nil, nil)
	if gate.Configured() {
		t.Error("Configured = true on a gate holding nothing, want false")
	}
	if gate.OAuth() != nil || gate.Allowlist() != nil {
		t.Error("an unconfigured gate handed out a login, want nil so callers refuse")
	}

	oauth, err := NewOAuth(OAuthConfig{ClientID: "id", ClientSecret: "secret", PublicURL: "http://127.0.0.1:8080"})
	if err != nil {
		t.Fatalf("NewOAuth: %v", err)
	}
	allowlist := newAllowlist(t, openStore(t), "alice")

	gate.Set(oauth, allowlist)
	if !gate.Configured() {
		t.Error("Configured = false after Set, want true")
	}
	if gate.OAuth() != oauth || gate.Allowlist() != allowlist {
		t.Error("the gate handed back something other than what it was given")
	}
}

func TestAllowlistReportsTheEntriesItWasConfiguredWith(t *testing.T) {
	entries := newAllowlist(t, openStore(t), " 1234 ", "Bob", "").Entries()
	if len(entries) != 2 || entries[0] != "1234" || entries[1] != "Bob" {
		t.Errorf("Entries = %v, want the two usable entries as they were written", entries)
	}
}
