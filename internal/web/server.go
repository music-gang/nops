// Package web serves the dashboard and the git webhook. Authentication is
// session.go (shared session/cookie/actor mechanics), auth.go (OpenID
// Connect) and auth_basic.go (local users); this file wires the pages, the
// diff renderer and the webhook behind whichever Authenticator it is given.
// The pages are documented in docs/dashboard.md.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

//go:embed static
var staticFiles embed.FS

//go:embed templates
var templateFiles embed.FS

// historyLimit is how many past deployments the history page shows. There is
// no pagination: a self-hosted, personal cluster does not accumulate enough
// deployments to need one, and adding it now would be overengineering.
const historyLimit = 200

// Store is what the dashboard reads from SQLite. *store.Store implements it.
type Store interface {
	GetDeployment(ctx context.Context, id string) (*store.Deployment, error)
	ListActive(ctx context.Context) ([]*store.Deployment, error)
	ListHistory(ctx context.Context, limit int) ([]*store.Deployment, error)
	ListByJob(ctx context.Context, namespace, jobID string, limit int) ([]*store.Deployment, error)
	LatestPerJob(ctx context.Context) ([]*store.Deployment, error)
	Events(ctx context.Context, deploymentID string) ([]store.Event, error)
	GetHookRun(ctx context.Context, deploymentID, phase string) (*store.HookRun, error)
}

// Engine is what the dashboard calls to decide a pending deployment, to
// retry a blocked job and to read the drift of "none"-policy jobs. *engine.Engine implements it.
type Engine interface {
	Approve(ctx context.Context, id, specHash, actor string) error
	Reject(ctx context.Context, id, actor string) error
	Observations() []engine.Observation
	Orphans() []engine.Orphan
	Retry(ctx context.Context, namespace, jobID, actor string) error
	Status() engine.Status
}

// Git is what the dashboard shows about the repository nops reads: the
// commit it is on and how the last poll went. *gitwatch.Watcher implements it.
type Git interface {
	Snapshot() gitwatch.Snapshot
	Status() gitwatch.Status
}

// Authenticator is what the dashboard needs from a login backend: NewAuth
// (OpenID Connect) and NewBasicAuth (local users) both implement it, and
// -auth-mode picks exactly one (docs/dashboard.md#authentication).
type Authenticator interface {
	// Register adds the backend's /auth/* routes to mux.
	Register(mux *http.ServeMux)
	// Require lets a request through only with a valid session, and puts
	// the actor in its context (UserFrom).
	Require(next http.Handler) http.Handler
}

var (
	_ Authenticator = (*Auth)(nil)
	_ Authenticator = (*BasicAuth)(nil)
)

// Options configures New.
type Options struct {
	Auth   Authenticator
	Store  Store
	Engine Engine
	Git    Git

	// CommitURL returns the web address of a commit, or "" if there is none
	// (the SHA is then shown without a link). Optional.
	CommitURL func(sha string) string
	// Now is the clock the relative times ("3m ago") are counted from.
	// Optional: time.Now.
	Now func() time.Time

	// Trigger runs the git watcher's non-blocking poll after a webhook
	// request passes its signature check, and for the dashboard's "fetch
	// now" button. Required when WebhookSecret is set; without it the button
	// is not served.
	Trigger func()
	// WebhookSecret is compared against the per-forge signature of an
	// incoming webhook request. Empty disables /webhook/git (a 404).
	WebhookSecret string

	Log *slog.Logger
}

// server holds the dependencies every handler needs.
type server struct {
	auth      Authenticator
	store     Store
	engine    Engine
	git       Git
	trigger   func()
	commitURL func(sha string) string
	now       func() time.Time
	secret    []byte
	log       *slog.Logger
	tmpl      *template.Template
	static    fs.FS
	started   time.Time
}

// New builds the dashboard and git webhook handler. It parses every template
// once, so a broken one fails loud at startup rather than on the first
// request.
func New(o Options) (http.Handler, error) {
	if o.Auth == nil || o.Store == nil || o.Engine == nil || o.Git == nil || o.Log == nil {
		return nil, errors.New("web: Auth, Store, Engine, Git and Log are required")
	}
	if o.WebhookSecret != "" && o.Trigger == nil {
		return nil, errors.New("web: Trigger is required when WebhookSecret is set")
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	staticDir, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("web: static assets: %w", err)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &server{
		auth:      o.Auth,
		store:     o.Store,
		engine:    o.Engine,
		git:       o.Git,
		trigger:   o.Trigger,
		commitURL: o.CommitURL,
		now:       o.Now,
		secret:    []byte(o.WebhookSecret),
		log:       o.Log,
		tmpl:      tmpl,
		static:    staticDir,
		started:   time.Now(),
	}
	return securityHeaders(s.routes()), nil
}

// parseTemplates parses every template once, shared by New (fail loud at
// startup) and the render smoke test (server_test.go).
func parseTemplates() (*template.Template, error) {
	tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return tmpl, nil
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	s.auth.Register(mux) // GET/POST /auth/*, whichever backend this is

	mux.HandleFunc("GET /healthz", s.healthz)
	if len(s.secret) > 0 {
		mux.HandleFunc("POST /webhook/git", s.webhook)
	}

	static := http.FileServerFS(s.static)
	mux.Handle("GET /static/", noStoreExempt(http.StripPrefix("/static/", static)))

	mux.Handle("GET /{$}", s.auth.Require(http.HandlerFunc(s.index)))
	mux.Handle("GET /jobs", s.auth.Require(http.HandlerFunc(s.jobs)))
	mux.Handle("GET /jobs/{namespace}/{job}", s.auth.Require(http.HandlerFunc(s.job)))
	mux.Handle("GET /history", s.auth.Require(http.HandlerFunc(s.history)))
	mux.Handle("GET /drift", s.auth.Require(http.HandlerFunc(s.drift)))
	mux.Handle("GET /deployments/{id}", s.auth.Require(http.HandlerFunc(s.deployment)))
	mux.Handle("GET /deployments/{id}/status", s.auth.Require(http.HandlerFunc(s.deploymentStatus)))
	mux.Handle("POST /deployments/{id}/approve", s.auth.Require(http.HandlerFunc(s.approve)))
	mux.Handle("POST /deployments/{id}/reject", s.auth.Require(http.HandlerFunc(s.reject)))
	mux.Handle("POST /jobs/{namespace}/{job}/retry", s.auth.Require(http.HandlerFunc(s.retry)))
	if s.trigger != nil {
		mux.Handle("POST /fetch", s.auth.Require(http.HandlerFunc(s.fetchNow)))
	}

	return mux
}

// healthz never requires a session: it is what an orchestrator or a load
// balancer probes.
func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// noStoreExempt serves static assets with a cacheable header instead of the
// no-store the rest of the dashboard gets: they hold no secret and are
// embedded in the binary, so a day-long cache is safe and a new release
// simply serves new bytes at the same path.
func noStoreExempt(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		next.ServeHTTP(w, r)
	})
}

// securityHeaders adds the headers every response gets, dashboard and
// webhook alike. The dashboard has no inline script or style, so the CSP
// allows only same-origin sources; htmx.config.includeIndicatorStyles is
// turned off in the page head so it never needs 'unsafe-inline'.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; " +
		"form-action 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
