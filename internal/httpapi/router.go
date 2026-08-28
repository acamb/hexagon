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
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
)

// RepoLister returns the repositories a user can start a session from,
// remembering them for a short while.
type RepoLister interface {
	List(ctx context.Context, userID, token string) ([]github.Repo, error)
	Invalidate(userID string)
}

// Deps are the collaborators the handlers need.
type Deps struct {
	Config *config.Config
	Store  *store.Store
	Auth   *auth.Service
	// OAuth drives the GitHub login. Nil when the development bypass is active.
	OAuth *auth.OAuth
	// Dev is the HEXAGON_DEV_USER bypass. Nil during normal operation.
	Dev      *auth.DevProvider
	GitHub   auth.UserFetcher
	Repos    RepoLister
	Docker   dockerx.API
	Sessions *session.Manager
	// BaseDockerfile is offered to the UI as the starting point for a new image.
	BaseDockerfile string
	Frontend       fs.FS
	Log            *slog.Logger
}

// Server carries the dependencies shared by the handlers.
type Server struct {
	cfg            *config.Config
	store          *store.Store
	auth           *auth.Service
	oauth          *auth.OAuth
	dev            *auth.DevProvider
	github         auth.UserFetcher
	repos          RepoLister
	docker         dockerx.API
	sessions       *session.Manager
	baseDockerfile string
	log            *slog.Logger
	frontend       fs.FS
	started        time.Time
}

// New builds the router.
func New(deps Deps) http.Handler {
	s := &Server{
		cfg:            deps.Config,
		store:          deps.Store,
		auth:           deps.Auth,
		oauth:          deps.OAuth,
		dev:            deps.Dev,
		github:         deps.GitHub,
		repos:          deps.Repos,
		docker:         deps.Docker,
		sessions:       deps.Sessions,
		baseDockerfile: deps.BaseDockerfile,
		log:            deps.Log,
		frontend:       deps.Frontend,
		started:        time.Now(),
	}

	mux := http.NewServeMux()

	// Public: the health check and the login handshake itself.
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("GET /api/auth/callback", s.handleAuthCallback)

	// Authenticated.
	protected := map[string]http.HandlerFunc{
		"GET /api/auth/me":         s.handleAuthMe,
		"POST /api/auth/logout":    s.handleAuthLogout,
		"GET /api/images":          s.handleListImages,
		"POST /api/images":         s.handleCreateImage,
		"GET /api/images/template": s.handleImageTemplate,
		"GET /api/images/{id}":     s.handleGetImage,
		"GET /api/images/{id}/log": s.handleImageLog,
		"DELETE /api/images/{id}":  s.handleDeleteImage,
		"GET /api/github/repos":    s.handleListRepos,

		"GET /api/sessions":               s.handleListSessions,
		"POST /api/sessions":              s.handleCreateSession,
		"GET /api/sessions/{id}":          s.handleGetSession,
		"PATCH /api/sessions/{id}":        s.handleUpdateSession,
		"POST /api/sessions/{id}/start":   s.handleStartSession,
		"POST /api/sessions/{id}/stop":    s.handleStopSession,
		"DELETE /api/sessions/{id}":       s.handleDeleteSession,
		"GET /api/sessions/{id}/terminal": s.handleTerminal,
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

	return requestLogger(s.log, s.guardStateChanges(mux))
}

// handleHealth reports whether the process can serve traffic. It touches the
// database so a broken data directory shows up here rather than on first use.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
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
