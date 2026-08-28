// Package httpapi exposes Hexagon's HTTP surface: the JSON API under /api and
// the embedded single page application on everything else.
package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/store"
)

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
	Frontend fs.FS
	Log      *slog.Logger
}

// Server carries the dependencies shared by the handlers.
type Server struct {
	cfg      *config.Config
	store    *store.Store
	auth     *auth.Service
	oauth    *auth.OAuth
	dev      *auth.DevProvider
	github   auth.UserFetcher
	log      *slog.Logger
	frontend fs.FS
	started  time.Time
}

// New builds the router.
func New(deps Deps) http.Handler {
	s := &Server{
		cfg:      deps.Config,
		store:    deps.Store,
		auth:     deps.Auth,
		oauth:    deps.OAuth,
		dev:      deps.Dev,
		github:   deps.GitHub,
		log:      deps.Log,
		frontend: deps.Frontend,
		started:  time.Now(),
	}

	mux := http.NewServeMux()

	// Public: the health check and the login handshake itself.
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("GET /api/auth/callback", s.handleAuthCallback)

	// Authenticated.
	mux.Handle("GET /api/auth/me", s.requireAuth(http.HandlerFunc(s.handleAuthMe)))
	mux.Handle("POST /api/auth/logout", s.requireAuth(http.HandlerFunc(s.handleAuthLogout)))

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

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}
