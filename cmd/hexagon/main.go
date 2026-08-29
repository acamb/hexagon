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
	"path/filepath"
	"syscall"
	"time"

	"github.com/andrea/hexagon"
	"github.com/andrea/hexagon/internal/auth"
	"github.com/andrea/hexagon/internal/bitbucket"
	"github.com/andrea/hexagon/internal/claudex"
	"github.com/andrea/hexagon/internal/codeserver"
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/httpapi"
	"github.com/andrea/hexagon/internal/provider"
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
	// Every start, not just the first: the whole value of the setting is that
	// nobody discovers by accident that the session cookie is on the wire.
	if cfg.InsecureHTTP {
		log.Warn("insecureHttp is set: the session cookie may travel in plaintext",
			"addr", cfg.Addr, "public_url", cfg.PublicURL)
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

	// After buildDeps, which is where the allowlist is built.
	pruneRevokedSessions(context.Background(), st, deps.Allowlist, log)

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
		// An idle connection is one between requests, so this is safe for
		// everything: a hijacked WebSocket is no longer an idle HTTP connection
		// and the server stops managing it.
		IdleTimeout: 120 * time.Second,
		// No read or write timeout, deliberately. Both set a deadline on the
		// underlying connection, which the terminal and the VS Code proxy hijack
		// and keep for as long as the browser stays attached. Request bodies get
		// their deadline per route instead, in httpapi.decodeJSON.
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

// buildDeps wires authentication. The GitHub OAuth login is the only mode there
// is, and its constructor refuses an empty allowlist, so the server never starts
// with an unauthenticated API.
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

	// Every source of repositories this build knows about. GitHub is also the
	// login, which is why its client is built above; the rest are accounts a
	// signed-in user connects.
	providers := provider.NewRegistry(gh, bitbucket.NewWithBaseURL(cfg.BitbucketAPIURL))
	repos := provider.NewLister(providers, logins, provider.DefaultRepoTTL)
	credentials := auth.NewGitCredentialSource(logins, providers)

	vscode := codeserver.New(codeserver.Config{Dir: cfg.VSCodeDir, Version: cfg.VSCodeVersion})

	// Containers run as the user running the server, so files written into the
	// bind mounted clone stay owned by them rather than by root.
	sessions := session.NewManager(st, docker, session.GitCloner{}, credentials, vscode, session.Config{
		WorkspaceRoot:     cfg.WorkspaceRoot,
		ClaudeCredentials: cfg.ClaudeCredentials,
		AnthropicAPIKey:   cfg.AnthropicAPIKey,
		ClaudeLoginDir:    filepath.Join(cfg.DataDir, "claude-login"),
		GitUserName:       cfg.GitUserName,
		GitUserEmail:      cfg.GitUserEmail,
		ContainerUser:     fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
	}, log)

	deps := httpapi.Deps{
		Config:         cfg,
		Store:          st,
		Auth:           logins,
		GitHub:         gh,
		Providers:      providers,
		Repos:          repos,
		Docker:         docker,
		Sessions:       sessions,
		BaseDockerfile: hexagon.BaseDockerfile,
		Frontend:       frontend,
		Log:            log,
	}

	// Optional: without a claude binary the Images page can still be edited by
	// hand, so a missing one is a note in the log rather than a refusal to run.
	// The interface is left nil in that case, which is what the UI asks about.
	if runner, err := claudex.New(cfg.ClaudeBinary); err != nil {
		log.Info("no claude binary: Dockerfiles can only be edited by hand", "err", err)
	} else {
		deps.Editor = runner.WithModel(cfg.ClaudeModel)
	}

	allowlist, err := auth.NewAllowlist(cfg.AllowedUsers, st, log)
	if err != nil {
		return httpapi.Deps{}, err
	}
	oauth, err := auth.NewOAuth(auth.OAuthConfig{
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		PublicURL:    cfg.PublicURL,
	})
	if err != nil {
		return httpapi.Deps{}, err
	}
	log.Info("github login enabled", "callback", oauth.RedirectURI(), "allowed_users", cfg.AllowedUsers)
	deps.OAuth = oauth
	deps.Allowlist = allowlist
	return deps, nil
}

// pruneRevokedSessions signs out anyone the allowlist no longer admits. The
// per-request check already refuses them, but only when they come back: this is
// what makes a removal take effect on a browser that never does.
func pruneRevokedSessions(ctx context.Context, st *store.Store, allowlist *auth.Allowlist, log *slog.Logger) {
	users, err := st.ListUsers(ctx)
	if err != nil {
		log.Warn("look for sessions to revoke", "err", err)
		return
	}
	for _, user := range users {
		switch allowed, err := allowlist.Allowed(ctx, user.GitHubLogin, user.GitHubID); {
		case err != nil:
			log.Warn("check the allowlist", "login", user.GitHubLogin, "err", err)
			continue
		case allowed:
			continue
		}
		n, err := st.DeleteUserSessionsForUser(ctx, user.ID)
		switch {
		case err != nil:
			log.Warn("revoke sessions", "login", user.GitHubLogin, "err", err)
		case n > 0:
			log.Info("revoked the sessions of a user no longer in the allowlist",
				"login", user.GitHubLogin, "count", n)
		}
	}
}

// newLogger returns a text logger, at debug level when debug logging is on.
func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
