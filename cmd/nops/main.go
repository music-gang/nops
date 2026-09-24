// Command nops is a semi-automatic GitOps controller for HashiCorp Nomad:
// see docs/README.md for what it is and docs/architecture.md for how the
// pieces below fit together. This file only wires them.
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
	"sync"
	"syscall"
	"time"

	"github.com/music-gang/nops/internal/config"
	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/notify"
	"github.com/music-gang/nops/internal/store"
	"github.com/music-gang/nops/internal/web"
)

// shutdownTimeout is how long the dashboard's HTTP server gets to finish an
// in-flight request after a signal, before Shutdown gives up and returns.
// The three background loops need no timeout of their own: they already
// select on ctx.Done() (gitwatch.Watcher.Run, Engine.RunDetection,
// Engine.RunApply) and a hook wait (internal/hooks) respects the same ctx,
// so cancellation unwinds them promptly.
const shutdownTimeout = 10 * time.Second

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		// Load only writes flag-parse errors and -h usage to os.Stderr
		// itself; a validation error (a missing required option, and so
		// on) is only ever returned, so it is on us to print it.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err := run(ctx, cfg, log); err != nil {
		log.Error("nops", "error", err)
		os.Exit(1)
	}
}

// run builds every component and serves until ctx is done, then shuts down.
// It never logs cfg itself: it holds the Nomad and git tokens, the OIDC
// client secret and the webhook secret.
func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if cfg.NomadTLSSkipVerify {
		log.Warn("Nomad server certificate verification is disabled (-nomad-tls-skip-verify)")
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	nomadClient, err := nomadx.New(cfg.Nomad(), cfg.NomadNamespace)
	if err != nil {
		return fmt.Errorf("nomad client: %w", err)
	}

	hooksRunner := hooks.New(nomadClient, st, log, cfg.HookPollInterval)

	notifier := notify.New(notify.Options{
		Webhook:   notify.Endpoint{URL: cfg.NotifyWebhookURL, Token: cfg.NotifyWebhookToken},
		Discord:   notify.Endpoint{URL: cfg.NotifyDiscordURL},
		Slack:     notify.Endpoint{URL: cfg.NotifySlackURL},
		Ntfy:      notify.Endpoint{URL: cfg.NotifyNtfyURL, Token: cfg.NotifyNtfyToken},
		Gotify:    notify.Endpoint{URL: cfg.NotifyGotifyURL, Token: cfg.NotifyGotifyToken},
		PublicURL: cfg.PublicURL,
		Timeout:   cfg.NotifyTimeout,
	}, log)

	watcher := gitwatch.New(gitwatch.Options{
		URL: cfg.GitURL, Branch: cfg.GitBranch, Path: cfg.GitPath,
		Username: cfg.GitUsername, Token: cfg.GitToken, PollInterval: cfg.GitPollInterval,
	}, log)
	if err := watcher.Start(ctx); err != nil {
		return fmt.Errorf("git watcher: %w", err)
	}

	eng := engine.New(engine.Options{
		Store: st, Nomad: nomadClient, Snapshots: watcher, Notifier: notifier, Hooks: hooksRunner,
		Namespace: cfg.NomadNamespace, DriftInterval: cfg.DriftInterval,
		EngineInterval: cfg.EngineInterval, ApplyTimeout: cfg.ApplyTimeout, Log: log,
	})

	auth, err := newAuthenticator(cfg, log)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	handler, err := web.New(web.Options{
		Auth: auth, Store: st, Engine: eng, Git: watcher, Trigger: watcher.Trigger,
		CommitURL:     func(sha string) string { return gitwatch.CommitURL(cfg.GitURL, sha) },
		WebhookSecret: cfg.WebhookSecret, Log: log,
	})
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}

	// A child of the signal context, so a server failure below stops the
	// three loops too, not just the signal that started this function's ctx.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){watcher.Run, eng.RunDetection, eng.RunApply} {
		wg.Add(1)
		go func(loop func(context.Context)) {
			defer wg.Done()
			loop(ctx)
		}(loop)
	}

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: handler}
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-srvErr:
		if err != nil {
			runErr = fmt.Errorf("dashboard server: %w", err)
		}
		cancel() // stop the three loops too
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("dashboard server shutdown: %w", err)
	}
	wg.Wait()
	return runErr
}

// newAuthenticator builds the login backend config.check() approved:
// exactly one of cfg.AuthMode's two sets of options is set.
func newAuthenticator(cfg *config.Config, log *slog.Logger) (web.Authenticator, error) {
	switch cfg.AuthMode {
	case "oidc":
		return web.NewAuth(web.AuthOptions{
			Issuer: cfg.OIDCIssuerURL, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			RedirectURL:   cfg.PublicURL + "/auth/callback",
			AllowedUsers:  cfg.OIDCAllowedUsers,
			AllowedGroups: cfg.OIDCAllowedGroups,
			Log:           log,
		})
	case "basic":
		return web.NewBasicAuth(web.BasicAuthOptions{UsersFile: cfg.UsersFile, PublicURL: cfg.PublicURL, Log: log})
	default:
		// config.Load's check() already refuses any other value.
		return nil, fmt.Errorf("unknown -auth-mode %q", cfg.AuthMode)
	}
}
