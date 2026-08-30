package auth

import "sync"

// Gate holds the two things that decide whether anybody can sign in: the GitHub
// OAuth client and the allowlist.
//
// They used to be built once at startup, and the process refused to run without
// them. It no longer does: a server with no OAuth application configured serves
// the first-time wizard instead, and the wizard's whole purpose is to supply
// one while the process is running. So both live behind this, and every request
// reads them through it rather than holding a copy.
//
// The pair is replaced together and never one at a time. They are configured by
// the same act and a request that saw a new OAuth client with the old allowlist
// would be checking a login against the wrong list.
type Gate struct {
	mu        sync.RWMutex
	oauth     *OAuth
	allowlist *Allowlist
}

// NewGate returns a gate holding what this process was configured with. Both
// arguments are nil when nothing was: that is the state the wizard exists to
// leave, not an error.
func NewGate(oauth *OAuth, allowlist *Allowlist) *Gate {
	return &Gate{oauth: oauth, allowlist: allowlist}
}

// Configured reports whether a login is possible at all.
func (g *Gate) Configured() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.oauth != nil && g.allowlist != nil
}

// OAuth returns the OAuth client, or nil when the server is not configured. A
// caller that can be reached before the wizard has run must check.
func (g *Gate) OAuth() *OAuth {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.oauth
}

// Allowlist returns the allowlist, or nil when the server is not configured.
func (g *Gate) Allowlist() *Allowlist {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.allowlist
}

// Set replaces both, which is how the first-time wizard hands over what it
// collected without the server being restarted.
func (g *Gate) Set(oauth *OAuth, allowlist *Allowlist) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.oauth, g.allowlist = oauth, allowlist
}
