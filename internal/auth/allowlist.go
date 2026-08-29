package auth

import (
	"errors"
	"strings"
)

// Allowlist answers one question: may this person use this machine. It is its
// own type rather than a detail of OAuth because that question is not asked
// only at the login — every request asks it, and a login that was admitted last
// week is not evidence about this one.
type Allowlist struct {
	logins map[string]bool
}

// NewAllowlist builds the list from the configured entries.
//
// It fails closed. Without an allowlist any GitHub account on the planet could
// sign in and get a shell on this machine, so an empty list is a startup error
// rather than a permissive default.
func NewAllowlist(entries []string) (*Allowlist, error) {
	logins := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry = strings.TrimSpace(entry); entry != "" {
			logins[strings.ToLower(entry)] = true
		}
	}
	if len(logins) == 0 {
		return nil, errors.New("HEXAGON_ALLOWED_USERS is required: list the GitHub logins allowed to sign in")
	}
	return &Allowlist{logins: logins}, nil
}

// Allowed reports whether a GitHub login may use Hexagon. GitHub logins are
// case insensitive, so the comparison is too.
func (a *Allowlist) Allowed(login string) bool {
	return a.logins[strings.ToLower(login)]
}
