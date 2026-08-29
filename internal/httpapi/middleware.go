package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/store"
)

// requireAuth rejects requests without a valid session and puts the user into
// the request context for the wrapped handler.
//
// The WebSocket terminal endpoint goes through this too: browsers send cookies
// on the upgrade handshake, so there is no reason for it to authenticate
// differently from the rest of the API.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.auth.Authenticate(r.Context(), r)
		switch {
		case errors.Is(err, auth.ErrNoSession):
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		case err != nil:
			s.log.Error("authenticate request", "path", r.URL.Path, "err", err)
			writeError(w, http.StatusInternalServerError, "authentication failed")
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
	})
}

// user returns the authenticated caller. Only call it from handlers behind
// requireAuth, where the middleware guarantees it is present.
func (s *Server) user(r *http.Request) *store.User {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		panic("httpapi: handler without requireAuth asked for the user")
	}
	return user
}

// contentSecurityPolicy is what a browser is allowed to load for a page this
// server renders itself. Three of the directives are looser than they look, and
// each is a place where tightening it would break something:
//
//   - style-src 'unsafe-inline', because xterm.js injects its stylesheet at
//     runtime. A nonce would work and would have to be threaded into a hashed
//     asset this server does not template. Not worth it for style.
//   - img-src https:, because avatars come from whichever provider the account
//     is on — GitHub's CDN, Bitbucket's, a different host again. Enumerating
//     provider CDNs is a list that goes stale the first time one of them moves.
//   - frame-ancestors 'none', which is not loose at all: nothing here is meant
//     to be embedded, and this is what makes the session controls
//     unclickjackable.
//
// connect-src 'self' covers the terminal WebSocket: 'self' matches the ws and
// wss forms of our own origin, so the socket needs no directive of its own.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"connect-src 'self'; " +
	"font-src 'self' data:; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'; " +
	"form-action 'none'"

// securityHeaders sets what the browser is told about every response, and wraps
// the router from outside the way requestLogger does: a header that is only on
// the routes someone remembered is not a policy.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	// HSTS is emitted only over https. Served over plaintext it is ignored, and
	// served from a development instance on localhost it is a trap that
	// outlives the instance: the browser remembers the host for a year.
	hsts := strings.HasPrefix(s.cfg.PublicURL, "https://")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		if hsts {
			header.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		// The VS Code proxy is exempt. code-server needs inline scripts,
		// workers and blob: URLs; the policy above would break it, and one
		// loose enough for it would be worth nothing on our own pages. The
		// answer to that is serving the editor from its own origin, which is a
		// larger change than this one.
		if !isVSCodeProxyPath(r.URL.Path) {
			header.Set("Content-Security-Policy", contentSecurityPolicy)
		}
		next.ServeHTTP(w, r)
	})
}

// guardStateChanges is defence in depth against cross-site requests. The
// session cookie is SameSite=Lax, which already blocks cross-site form posts;
// on top of that a mutating API call must look like it came from our own
// frontend: a JSON content type (which HTML forms cannot produce) and, when the
// browser sends one, a matching Origin.
func (s *Server) guardStateChanges(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		// The VS Code proxy carries a whole other application's traffic, which is
		// not JSON and cannot be made to be. It stays behind requireAuth and the
		// same-origin check like every other endpoint; only the content type rule
		// is lifted.
		if ct := r.Header.Get("Content-Type"); !isVSCodeProxyPath(r.URL.Path) && !strings.HasPrefix(ct, "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, "expected Content-Type: application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isVSCodeProxyPath reports whether p is the VS Code proxy for some session,
// with or without the trailing path code-server itself is asked for.
func isVSCodeProxyPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/api/sessions/")
	if !ok {
		return false
	}
	_, after, ok := strings.Cut(rest, "/")
	return ok && (after == "vscode" || strings.HasPrefix(after, "vscode/"))
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// sameOrigin reports whether the request originates from the configured public
// URL. A missing Origin header is accepted: non-browser clients such as curl do
// not send one, and browsers always do on the requests we care about.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return s.originMatches(origin)
}

// originMatches compares one origin against the configured public URL. Scheme
// and host, because that is what an origin is: a path or a trailing slash in
// either value is not part of the comparison.
func (s *Server) originMatches(origin string) bool {
	got, err := url.Parse(origin)
	if err != nil {
		return false
	}
	want, err := url.Parse(s.cfg.PublicURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(got.Scheme, want.Scheme) && strings.EqualFold(got.Host, want.Host)
}

// originSignal is what a request says about where it was made from.
type originSignal int

const (
	// originUnknown: the request carries neither header. A non-browser client
	// does that, and so does a browser too old to send Sec-Fetch-Site. Whether
	// that is good enough is the caller's decision, not this function's.
	originUnknown originSignal = iota
	// originSame: the request came from our own pages.
	originSame
	// originCross: it came from somewhere else, and the browser said so itself.
	originCross
)

// sameOriginSignal reads the two headers a browser uses to describe where a
// request came from into one answer.
//
// Origin wins when it is there, because it names the origin outright where
// Sec-Fetch-Site only classifies it. Sec-Fetch-Site covers what Origin leaves
// out: browsers omit Origin on a top-level navigation, which is exactly the
// shape of the VS Code button. "none" is such a navigation — a typed URL, a
// bookmark, a link opened in a new tab — and stays unknown rather than same:
// nobody else's page put the request there, but it is not the proof of our own
// origin that a state change is held to.
func (s *Server) sameOriginSignal(r *http.Request) originSignal {
	if origin := r.Header.Get("Origin"); origin != "" {
		if s.originMatches(origin) {
			return originSame
		}
		return originCross
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return originSame
	case "cross-site", "same-site":
		return originCross
	}
	return originUnknown
}

// isWebSocketUpgrade reports whether r is a WebSocket handshake. Connection is a
// list and picks up other tokens on the way through a proxy, so this looks for
// the token rather than comparing the whole header.
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}
