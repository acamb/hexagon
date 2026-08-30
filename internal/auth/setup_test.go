package auth

import (
	"strings"
	"testing"
)

func TestSetupPasswordIsAcceptedHoweverItIsTypedBack(t *testing.T) {
	setup, password, err := NewSetup()
	if err != nil {
		t.Fatalf("NewSetup: %v", err)
	}
	if !strings.Contains(password, "-") {
		t.Errorf("password = %q, want it grouped so it can be read off a log", password)
	}

	for _, typed := range []string{
		password,
		strings.ReplaceAll(password, "-", ""),
		strings.ToLower(password),
		" " + password + "\n",
		strings.ReplaceAll(password, "-", " "),
	} {
		if !setup.Verify(typed) {
			t.Errorf("Verify(%q) = false, want true", typed)
		}
	}
}

func TestSetupRefusesEveryOtherPassword(t *testing.T) {
	setup, password, err := NewSetup()
	if err != nil {
		t.Fatalf("NewSetup: %v", err)
	}
	other, _, err := NewSetup()
	if err != nil {
		t.Fatalf("NewSetup: %v", err)
	}

	for _, wrong := range []string{"", "hunter2", password + "A", password[1:]} {
		if setup.Verify(wrong) {
			t.Errorf("Verify(%q) = true, want false", wrong)
		}
	}
	// Two processes never share a password: the one in this run's log is the
	// only one that works.
	if other.Verify(password) {
		t.Error("a password from another process was accepted, want each run to have its own")
	}
}
