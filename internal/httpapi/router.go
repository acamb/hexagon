// Package httpapi exposes Hexagon's HTTP surface: the JSON API under /api and
// the embedded single page application on everything else.
package httpapi

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/provider"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
)

// SourceEditor rewrites an image's source — a Dockerfile or a compose file —
// from an instruction in English, and checks whether a Claude Code credential
// works. It is nil when the server has no Claude Code binary to run, which is a
// state the UI is told about rather than a startup failure.
type SourceEditor interface {
	Edit(ctx context.Context, cred claudex.Credential, kind, content, instruction string) (claudex.Edit, error)
	Check(ctx context.Context, cred claudex.Credential) error
}

// DefaultImage reports the image Hexagon builds for itself, and starts building
// it when asked for one that is not there yet. It is what the browser login runs
// in when the user has no image of their own, and what the source editor runs in
// on a server with no claude binary. Nil when there is no Docker to build it
// with, which is a state the UI is told about rather than a startup failure.
type DefaultImage interface {
	State() claudex.State
}

// ComposeValidator refuses a compose file that asks for something a session
// must not be able to have. It is nil when the server has no `docker compose`,
// and the Images page then does not offer advanced images at all rather than
// offering ones that fail at the first session.
type ComposeValidator interface {
	Validate(ctx context.Context, content string) ([]string, error)
}

// Deps are the collaborators the handlers need.
type Deps struct {
	Config *config.Config
	Store  *store.Store
	Auth   *auth.Service
	// Gate holds the GitHub login and the allowlist that says who may use this
	// instance. It is read on every request, not only at the login, and it is
	// empty on a server the first-time wizard has not configured yet.
	Gate *auth.Gate
	// Setup guards that wizard. It is nil once there is nothing left to set up.
	Setup  *auth.Setup
	GitHub auth.UserFetcher
	// Providers are the sources of repositories this build knows about, and
	// Repos merges the listings of the accounts a user has connected to them.
	Providers provider.Registry
	Repos     *provider.Lister
	Docker    dockerx.API
	Sessions  *session.Manager
	// BaseDockerfile and BaseCompose are offered to the UI as the starting
	// points for a new image.
	BaseDockerfile string
	BaseCompose    string
	// Editor is optional: without it the Images page only edits by hand.
	Editor SourceEditor
	// EditorInContainer says the editor above is the container one, which is
	// slower and worth warning about: the pages that offer it explain why it is
	// not the binary.
	EditorInContainer bool
	// DefaultImage is optional in the same way the editor is.
	DefaultImage DefaultImage
	// Compose is optional too: without it advanced images are not offered.
	Compose  ComposeValidator
	Frontend fs.FS
	Log      *slog.Logger
}

// Server carries the dependencies shared by the handlers.
type Server struct {
	cfg               *config.Config
	store             *store.Store
	auth              *auth.Service
	gate              *auth.Gate
	setup             *auth.Setup
	github            auth.UserFetcher
	providers         provider.Registry
	repos             *provider.Lister
	docker            dockerx.API
	sessions          *session.Manager
	baseDockerfile    string
	baseCompose       string
	editor            SourceEditor
	editorInContainer bool
	defaultImage      DefaultImage
	compose           ComposeValidator
	log               *slog.Logger
	frontend          fs.FS
	started           time.Time
	// limiter bounds what one address can ask of the routes that answer without
	// a session.
	limiter *ipLimiter
}

// New builds the router.
func New(deps Deps) http.Handler {
	s := &Server{
		cfg:               deps.Config,
		store:             deps.Store,
		auth:              deps.Auth,
		gate:              deps.Gate,
		setup:             deps.Setup,
		github:            deps.GitHub,
		providers:         deps.Providers,
		repos:             deps.Repos,
		docker:            deps.Docker,
		sessions:          deps.Sessions,
		baseDockerfile:    deps.BaseDockerfile,
		baseCompose:       deps.BaseCompose,
		editor:            deps.Editor,
		editorInContainer: deps.EditorInContainer,
		defaultImage:      deps.DefaultImage,
		compose:           deps.Compose,
		log:               deps.Log,
		frontend:          deps.Frontend,
		started:           time.Now(),
		limiter:           newIPLimiter(deps.Config.PublicRatePerMinute),
	}

	mux := http.NewServeMux()

	// Public: the health check and the login handshake itself. These are the
	// only routes an unauthenticated caller reaches, so they are the only ones
	// that need a limit of their own; everything else is bounded by having to
	// hold a session first.
	mux.HandleFunc("GET /api/health", s.limitPublic(s.handleHealth))
	mux.HandleFunc("GET /api/auth/login", s.limitPublic(s.handleAuthLogin))
	mux.HandleFunc("GET /api/auth/callback", s.limitPublic(s.handleAuthCallback))

	// Public too, and the exception to the rule above: the first-time wizard
	// runs before there is anybody to authenticate. It closes itself the moment
	// somebody signs in, and until then it is guarded by a password that exists
	// only in this process's memory and in its log. See
	// plans/M2/01-first-time-wizard.md.
	mux.HandleFunc("GET /api/setup", s.limitPublic(s.handleSetupStatus))
	mux.HandleFunc("POST /api/setup", s.limitPublic(s.handleSetup))

	// Authenticated.
	protected := map[string]http.HandlerFunc{
		"GET /api/auth/me":         s.handleAuthMe,
		"POST /api/auth/logout":    s.handleAuthLogout,
		"GET /api/settings":        s.handleGetSettings,
		"PUT /api/settings":        s.handleUpdateSettings,
		"GET /api/images":          s.handleListImages,
		"POST /api/images":         s.handleCreateImage,
		"GET /api/images/template": s.handleImageTemplate,
		"POST /api/images/source":  s.handleEditSource,
		"GET /api/images/{id}":     s.handleGetImage,
		"GET /api/images/{id}/log": s.handleImageLog,
		"DELETE /api/images/{id}":  s.handleDeleteImage,

		"GET /api/repos":                  s.handleListRepos,
		"GET /api/accounts":               s.handleListAccounts,
		"PUT /api/accounts/{provider}":    s.handleConnectAccount,
		"DELETE /api/accounts/{provider}": s.handleDisconnectAccount,

		"GET /api/claude":                s.handleClaudeStatus,
		"PUT /api/claude/credential":     s.handleSetClaudeCredential,
		"DELETE /api/claude/credential":  s.handleForgetClaudeCredential,
		"GET /api/claude/login/terminal": s.handleClaudeLoginTerminal,
		"DELETE /api/claude/login":       s.handleStopClaudeLogin,

		"GET /api/sessions":               s.handleListSessions,
		"POST /api/sessions":              s.handleCreateSession,
		"GET /api/sessions/{id}":          s.handleGetSession,
		"PATCH /api/sessions/{id}":        s.handleUpdateSession,
		"PUT /api/sessions/{id}/ports":    s.handleUpdateSessionPorts,
		"POST /api/sessions/{id}/start":   s.handleStartSession,
		"POST /api/sessions/{id}/stop":    s.handleStopSession,
		"DELETE /api/sessions/{id}":       s.handleDeleteSession,
		"GET /api/sessions/{id}/terminal": s.handleTerminal,

		// No method: every verb has to reach the proxy, including the WebSocket
		// upgrade code-server's own terminal uses.
		"/api/sessions/{id}/vscode":           s.handleVSCode,
		"/api/sessions/{id}/vscode/{path...}": s.handleVSCode,
	}
	for pattern, handler := range protected {
		mux.Handle(pattern, s.requireAuth(handler))
	}

	// Unknown API routes must answer as API routes; without this they would
	// fall through to the SPA and return index.html with a 200.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
	mux.Handle("/", s.spaHandler())

	return requestLogger(s.log, s.securityHeaders(s.guardStateChanges(mux)))
}

// handleHealth reports whether the process can serve traffic.
//
// Without a session the answer is that and nothing more. Whether the Docker
// daemon is reachable, how long this process has been up and whether its
// database is answering are facts about the machine, and this route is open to
// anyone who can reach the port. With a session it touches the database, so a
// broken data directory shows up here rather than on first use.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if _, err := s.auth.Authenticate(r.Context(), r); err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	body := map[string]any{
		"status": "ok",
		"uptime": time.Since(s.started).Round(time.Second).String(),
		"docker": "ok",
	}
	if err := s.docker.Ping(r.Context()); err != nil {
		// Not fatal: the server is still useful, and the UI can explain why
		// images and sessions are unavailable.
		body["docker"] = "unreachable"
	}
	if err := s.store.Ping(); err != nil {
		body["status"] = "degraded"
		body["error"] = "database unreachable"
		s.log.Error("health check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// requestLogger logs one line per request with the status and duration.
func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// Unwrap exposes the real ResponseWriter to http.ResponseController, so
// flushing and deadlines still reach it through this wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Hijack keeps WebSocket upgrades working. Without it this wrapper hides the
// underlying http.Hijacker and the terminal handshake fails with 501.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("httpapi: the response writer does not support hijacking")
	}
	// From here the connection is ours; nothing more will write a status.
	r.wroteHeader = true
	return hijacker.Hijack()
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}
