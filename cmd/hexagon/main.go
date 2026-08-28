// Command hexagon runs the Hexagon server: a web UI for managing Claude Code
// sessions in Docker containers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andrea/hexagon"
	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/httpapi"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
)

const shutdownTimeout = 10 * time.Second

func main() {
	configPath := flag.String("config", "", "path to a JSON configuration file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "hexagon:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	// The configuration comes first because it decides how verbose the logger
	// is. Nothing before this point can fail in a way worth logging: a Load
	// error goes to stderr on its own.
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Debug)
	if cfg.ConfigFile != "" {
		log.Info("configuration loaded", "file", cfg.ConfigFile)
	}

	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()

	if n, err := st.DeleteExpiredUserSessions(context.Background()); err != nil {
		log.Warn("prune expired sessions", "err", err)
	} else if n > 0 {
		log.Info("pruned expired sessions", "count", n)
	}

	// Nothing is going to finish work that was in flight when we stopped.
	if n, err := st.FailInterruptedImageBuilds(context.Background()); err != nil {
		log.Warn("clear interrupted builds", "err", err)
	} else if n > 0 {
		log.Info("marked interrupted image builds as failed", "count", n)
	}
	if n, err := st.FailInterruptedSessions(context.Background()); err != nil {
		log.Warn("clear interrupted sessions", "err", err)
	} else if n > 0 {
		log.Info("marked interrupted sessions as failed", "count", n)
	}

	docker, err := dockerx.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	defer docker.Close()

	// A daemon that is down right now may come up later, so this is a warning
	// rather than a startup failure.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := docker.Ping(pingCtx); err != nil {
		log.Warn("docker is unreachable: images and sessions will not work", "err", err)
	}
	pingCancel()

	deps, err := buildDeps(cfg, st, docker, log)
	if err != nil {
		return err
	}

	// Line the database up with what the daemon actually has, before serving
	// anyone a stale view of it.
	reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := deps.Sessions.Reconcile(reconcileCtx); err != nil {
		log.Warn("reconcile sessions", "err", err)
	}
	reconcileCancel()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.New(deps),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: the terminal endpoint streams for as long as the
		// browser stays attached.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "data_dir", cfg.DataDir, "workspaces", cfg.WorkspaceRoot)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// buildDeps wires authentication. Exactly one of the two modes is configured:
// the GitHub OAuth login, or the development bypass. Neither can be skipped, so
// the server never starts with an unauthenticated API.
func buildDeps(cfg *config.Config, st *store.Store, docker dockerx.API, log *slog.Logger) (httpapi.Deps, error) {
	frontend, err := hexagon.FrontendFS()
	if err != nil {
		return httpapi.Deps{}, fmt.Errorf("load frontend: %w", err)
	}

	cipher, err := auth.NewCipher(cfg.SecretKey)
	if err != nil {
		return httpapi.Deps{}, err
	}
	gh := github.New()
	if cfg.GitHubAPIURL != "" {
		log.Warn("using a non-default GitHub API", "url", cfg.GitHubAPIURL)
		gh = github.NewWithBaseURL(cfg.GitHubAPIURL)
	}
	logins := auth.NewService(st, cipher, cfg.PublicURL)
	repos := github.NewRepoCache(gh, github.DefaultRepoTTL)

	// Containers run as the user running the server, so files written into the
	// bind mounted clone stay owned by them rather than by root.
	sessions := session.NewManager(st, docker, session.GitCloner{}, logins, session.Config{
		WorkspaceRoot:     cfg.WorkspaceRoot,
		ClaudeCredentials: cfg.ClaudeCredentials,
		AnthropicAPIKey:   cfg.AnthropicAPIKey,
		GitUserName:       cfg.GitUserName,
		GitUserEmail:      cfg.GitUserEmail,
		ContainerUser:     fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
	}, log)

	deps := httpapi.Deps{
		Config:         cfg,
		Store:          st,
		Auth:           logins,
		GitHub:         gh,
		Repos:          repos,
		Docker:         docker,
		Sessions:       sessions,
		BaseDockerfile: hexagon.BaseDockerfile,
		Frontend:       frontend,
		Log:            log,
	}

	if cfg.DevUser != "" {
		dev, err := auth.NewDevProvider(cfg.DevUser, cfg.DevGitHubToken, cfg.Addr, gh, logins)
		if err != nil {
			return httpapi.Deps{}, err
		}
		log.Warn("the development bypass is configured: authentication is skipped", "user", cfg.DevUser)
		deps.Dev = dev
		return deps, nil
	}

	oauth, err := auth.NewOAuth(auth.OAuthConfig{
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		PublicURL:    cfg.PublicURL,
		AllowedUsers: cfg.AllowedUsers,
	})
	if err != nil {
		return httpapi.Deps{}, err
	}
	log.Info("github login enabled", "callback", oauth.RedirectURI(), "allowed_users", cfg.AllowedUsers)
	deps.OAuth = oauth
	return deps, nil
}

// newLogger returns a text logger, at debug level when debug logging is on.
func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
