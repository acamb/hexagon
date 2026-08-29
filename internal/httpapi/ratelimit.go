package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// limiterIdleTTL is how long a client's bucket is kept after its last
	// request. Long enough that a normal user keeps one bucket for a session of
	// work, short enough that a flood of forged addresses does not become a
	// permanent map.
	limiterIdleTTL = 10 * time.Minute
	// limiterSweepEvery bounds how often the map is walked, so a burst of new
	// addresses costs one walk rather than one per request.
	limiterSweepEvery = time.Minute
)

// ipLimiter is a token bucket per client address, for the handful of routes
// that answer without a session. Those are the only ones an unauthenticated
// caller can reach, and the login handshake talks to GitHub on their behalf.
type ipLimiter struct {
	every rate.Limit
	burst int

	mu        sync.Mutex
	clients   map[string]*client
	lastSweep time.Time
}

type client struct {
	limiter *rate.Limiter
	seen    time.Time
}

// newIPLimiter allows perMinute requests a minute from one address, and lets a
// whole minute's worth arrive at once: a browser opening the login page makes
// several requests in a breath, and refusing that would be a bug, not a limit.
func newIPLimiter(perMinute int) *ipLimiter {
	return &ipLimiter{
		every:     rate.Limit(float64(perMinute) / 60),
		burst:     perMinute,
		clients:   map[string]*client{},
		lastSweep: time.Now(),
	}
}

// allow reports whether this address may make one more request now.
func (l *ipLimiter) allow(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if now.Sub(l.lastSweep) > limiterSweepEvery {
		for key, c := range l.clients {
			if now.Sub(c.seen) > limiterIdleTTL {
				delete(l.clients, key)
			}
		}
		l.lastSweep = now
	}

	c, ok := l.clients[addr]
	if !ok {
		c = &client{limiter: rate.NewLimiter(l.every, l.burst)}
		l.clients[addr] = c
	}
	c.seen = now
	return c.limiter.Allow()
}

// limitPublic rate-limits one of the routes that is reachable without a
// session, per client address.
func (s *Server) limitPublic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		addr := clientIP(r)
		if !s.limiter.allow(addr) {
			s.log.Warn("rate limited", "addr", addr, "path", r.URL.Path)
			writeError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next(w, r)
	}
}

// clientIP is who to hold responsible for a request.
//
// X-Forwarded-For is believed only when the immediate peer is loopback, which
// is where the reverse proxy this server is meant to run behind lives (see
// deploy/proxy). Anywhere else the header is something the caller typed, and
// keying the limiter on it would let whoever it is meant to stop pick a fresh
// bucket per request.
//
// The last entry is the one the nearest proxy appended, and so the only one it
// vouched for; everything to its left was already in the header when it
// arrived.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return host
	}
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
			return last
		}
	}
	return host
}
