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
	"github.com/andrea/hexagon/internal/composex"
	"github.com/andrea/hexagon/internal/config"
	"github.com/andrea/hexagon/internal/dockerx"
	"github.com/andrea/hexagon/internal/github"
	"github.com/andrea/hexagon/internal/httpapi"
	"github.com/andrea/hexagon/internal/provider"
	"github.com/andrea/hexagon/internal/session"
	"github.com/andrea/hexagon/internal/store"
)

const shutdownTimeout = 10 * time.Second

// version is stamped at build time from the VERSION file at the root of the
// repository, with -X main.version. An unstamped build says "dev", which is how
// a binary from `go build ./cmd/hexagon` is told apart from a released one.
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to a JSON configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("hexagon", version)
		return
	}

	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "hexagon:", err)
		os.Exit(1)
	}
}

// rootStartupError refuses to start as root unless the operator opts in. The
// container user is derived from this process (ContainerUser, below), so uid 0
// here means root:root in every session container — the opposite of the
// "containers run as you, never as root" guarantee. It is a function so the
// decision can be tested without the test being root.
func rootStartupError(uid int, allowRoot bool) error {
	if uid == 0 && !allowRoot {
		return errors.New("refusing to run as root: session containers would run as root too; " +
			"run as an unprivileged user, or set HEXAGON_ALLOW_ROOT to override")
	}
	return nil
}

func run(configPath string) error {
	// The configuration comes first because it decides how verbose the logger
	// is. Nothing before this point can fail in a way worth logging: a Load
	// error goes to stderr on its own.
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log, level := newLogger(cfg.Debug)
	if cfg.ConfigFile != "" {
		log.Info("configuration loaded", "file", cfg.ConfigFile)
	}
	// Every start, not just the first: the whole value of the setting is that
	// nobody discovers by accident that the session cookie is on the wire.
	if cfg.InsecureHTTP {
		log.Warn("insecureHttp is set: the session cookie may travel in plaintext",
			"addr", cfg.Addr, "public_url", cfg.PublicURL)
	}
	// A server running as root would run every session container as root:root,
	// which is the boundary the container is supposed to be — against the
	// untrusted code a session runs — turned off. Fail closed.
	allowRoot := os.Getenv("HEXAGON_ALLOW_ROOT") != ""
	if err := rootStartupError(os.Getuid(), allowRoot); err != nil {
		return err
	}
	if allowRoot && os.Getuid() == 0 {
		log.Warn("running as root because HEXAGON_ALLOW_ROOT is set: session containers will run as root")
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
	failInterruptedTransfers(context.Background(), st, cfg.TransfersDir, log)

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

	deps, err := buildDeps(cfg, st, docker, log, level)
	if err != nil {
		return err
	}

	// The wizard is open until somebody signs in, so this is asked at every
	// start and not only on a server that has never been configured.
	if err := openSetup(context.Background(), st, &deps, cfg, log); err != nil {
		return err
	}

	// After buildDeps, which is where the allowlist is built.
	pruneRevokedSessions(context.Background(), st, deps.Gate.Allowlist(), log)

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

	go runTransferJanitor(ctx, st, cfg.TransfersDir, log)

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
// is, and a server that has not been given one starts anyway, serving the
// first-time wizard and nothing else: the gate is empty, and requireAuth
// refuses every request that is not part of setting it up. That is not a
// weaker rule than before, when a missing allowlist was a startup error — it is
// the same rule with somewhere to go from.
func buildDeps(cfg *config.Config, st *store.Store, docker dockerx.API, log *slog.Logger, level *slog.LevelVar) (httpapi.Deps, error) {
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

	// Advanced images need `docker compose`, which is a plugin the machine may
	// simply not have. It is asked once, here, so a server without it does not
	// offer advanced mode at all rather than offering one that fails at the
	// first session. Both interfaces stay nil in that case, which is what the
	// Images page and the session manager ask about.
	//
	// The typed nil matters: assigning an unavailable *composex.Runner to the
	// interfaces would make them non-nil and both checks useless.
	var (
		composeSessions session.Compose
		composeImages   httpapi.ComposeValidator
	)
	composeCtx, composeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if runner := composex.New(cfg.DockerCLI, cfg.DockerHost); runner.Available(composeCtx) {
		composeSessions, composeImages = runner, runner
	} else {
		log.Info("no docker compose: images with a compose file cannot be created or started")
	}
	composeCancel()

	// Containers run as the user running the server, so files written into the
	// bind mounted clone stay owned by them rather than by root.
	sessions := session.NewManager(st, docker, session.GitCloner{}, credentials, vscode, composeSessions, session.Config{
		WorkspaceRoot:     cfg.WorkspaceRoot,
		ClaudeCredentials: cfg.ClaudeCredentials,
		AnthropicAPIKey:   cfg.AnthropicAPIKey,
		ClaudeLoginDir:    filepath.Join(cfg.DataDir, "claude-login"),
		ClaudeAccountsDir: cfg.ClaudeAccountsDir,
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
		BaseCompose:    hexagon.BaseCompose,
		Compose:        composeImages,
		Frontend:       frontend,
		Log:            log,
		LogLevel:       level,
	}

	// The image Hexagon builds for itself, and the two things that need it: the
	// browser login on a machine where the user has built no image, and the
	// source editor on a machine with no claude binary. Nothing is built here —
	// the first page that asks for the image starts that.
	defaultImage := claudex.NewDefaultImage(docker, hexagon.BaseDockerfile, log)
	deps.DefaultImage = defaultImage

	// The binary is preferred wherever there is one: it answers in seconds,
	// where the container path pays for a container on every call and for an
	// image build on the first. A packaged install has no binary — the service
	// user has no npm and no home to install one into — and that used to leave
	// the feature silently absent, which is the reason for the fallback and for
	// telling the UI which of the two it got.
	if runner, err := claudex.New(cfg.ClaudeBinary); err == nil {
		deps.Editor = runner.WithModel(cfg.ClaudeModel).WithLogger(log)
	} else {
		log.Info("no claude binary: image sources will be edited through a container", "err", err)
		deps.Editor = claudex.NewContainer(docker, defaultImage,
			fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), cfg.ClaudeCredentials, cfg.ClaudeModel, log)
		deps.EditorInContainer = true
	}

	gate := auth.NewGate(nil, nil)
	deps.Gate = gate
	if cfg.GitHubClientID == "" || cfg.GitHubClientSecret == "" || len(cfg.AllowedUsers) == 0 {
		log.Warn("no github login configured: this server can serve the first-time wizard and nothing else",
			"config_file", cfg.ConfigPath)
		return deps, nil
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
	gate.Set(oauth, allowlist)
	return deps, nil
}

// openSetup opens the first-time wizard while nobody has ever signed in, and
// prints the password that guards it.
//
// The log is the only channel a server nobody can sign in to already shares
// with the operator, and this is the one moment the password exists in full:
// it is generated here, printed here, and kept only as a digest afterwards. A
// restart prints a new one and retires this one.
func openSetup(ctx context.Context, st *store.Store, deps *httpapi.Deps, cfg *config.Config, log *slog.Logger) error {
	signedIn, err := st.AnyUser(ctx)
	if err != nil {
		return fmt.Errorf("check whether anybody has signed in: %w", err)
	}
	if signedIn {
		return nil
	}

	setup, password, err := auth.NewSetup()
	if err != nil {
		return err
	}
	deps.Setup = setup
	log.Warn("first-time setup is open until somebody signs in",
		"url", cfg.PublicURL+"/setup", "password", password)
	return nil
}

// pruneRevokedSessions signs out anyone the allowlist no longer admits. The
// per-request check already refuses them, but only when they come back: this is
// what makes a removal take effect on a browser that never does.
func pruneRevokedSessions(ctx context.Context, st *store.Store, allowlist *auth.Allowlist, log *slog.Logger) {
	if allowlist == nil {
		// Nothing to check against yet. Every request is refused anyway until
		// the first-time wizard has run.
		return
	}
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

// newLogger returns a text logger and the level it reads, which the settings
// page moves while the server runs.
//
// The level is a variable rather than a constant because of when debug logging
// is wanted: never in advance, always in the middle of something going wrong.
// One that took effect at the next restart would be turned on after the
// evidence had gone.
func newLogger(debug bool) (*slog.Logger, *slog.LevelVar) {
	level := new(slog.LevelVar)
	level.Set(logLevel(debug))
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})), level
}

// logLevel is the one place the debug setting becomes a level.
func logLevel(debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
