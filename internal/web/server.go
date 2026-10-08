// Package web serves the dashboard, the JSON API (api.go, authenticated by the
// tokens of access.go) and the git webhook. Authentication is
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

	"github.com/music-gang/nops/internal/acl"
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

// recentCompletedLimit is how many deployments the Overview's "Recently
// completed" section shows.
const recentCompletedLimit = 5

// Store is what the dashboard reads from SQLite. *store.Store implements it.
type Store interface {
	GetDeployment(ctx context.Context, id string) (*store.Deployment, error)
	ListActive(ctx context.Context) ([]*store.Deployment, error)
	ListHistory(ctx context.Context, limit int) ([]*store.Deployment, error)
	ListRecentCompleted(ctx context.Context, limit int) ([]*store.Deployment, error)
	ListByJob(ctx context.Context, namespace, jobID string, limit int) ([]*store.Deployment, error)
	LatestPerJob(ctx context.Context) ([]*store.Deployment, error)
	Events(ctx context.Context, deploymentID string) ([]store.Event, error)
	ListHookRuns(ctx context.Context, deploymentID string) ([]*store.HookRun, error)
	DeploymentHooks(ctx context.Context, deploymentID string) ([]store.DeploymentHook, error)
	RetryOf(ctx context.Context, id string) (*store.Deployment, error)
}

// Engine is what the dashboard calls to decide a pending deployment, to
// retry a failed or rejected one, to pause and resume a job, to deploy a job held by its
// sync window, to promote the canaries of a
// deployment that waits for it and to read the drift of "none"-policy jobs.
// *engine.Engine implements it.
type Engine interface {
	Approve(ctx context.Context, id, specHash, actor string) error
	Reject(ctx context.Context, id, actor string) error
	Observations() []engine.Observation
	Orphans() []engine.Orphan
	Retry(ctx context.Context, id, actor string) (string, error)
	Retryable(d, latest *store.Deployment) bool
	DeployNow(ctx context.Context, namespace, jobID, specHash, actor string) (string, error)
	Promote(ctx context.Context, id, actor string) error
	Pause(ctx context.Context, namespace, jobID, actor, reason string) error
	Resume(ctx context.Context, namespace, jobID, actor string) error
	Status() engine.Status
}

// AccessStore is what the access control needs from SQLite: the ACL policies
// and the tokens, which the Administration page and /api/acl/ change and every
// request looks up. *store.Store implements it.
type AccessStore interface {
	PutACLPolicy(ctx context.Context, p store.ACLPolicy, by store.Audit) error
	ACLPolicy(ctx context.Context, name string) (store.ACLPolicy, error)
	ACLPolicies(ctx context.Context) ([]store.ACLPolicy, error)
	DeleteACLPolicy(ctx context.Context, name string, by store.Audit) error
	CreateACLToken(ctx context.Context, t store.ACLToken, secretHash string, by store.Audit) (store.ACLToken, error)
	ACLTokens(ctx context.Context) ([]store.ACLToken, error)
	ACLToken(ctx context.Context, accessorID string) (store.ACLToken, error)
	ACLTokenBySecret(ctx context.Context, hash string, acl bool) (store.ACLToken, error)
	RevokeACLToken(ctx context.Context, accessorID string, by store.Audit) error
	ACLChanges(ctx context.Context, limit int) ([]store.ACLChange, error)
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
	Access AccessStore

	// ACL turns the access control on (-acl, docs/acl.md). Off, every token
	// and every login can do everything.
	ACL bool
	// BootstrapToken is the secret of the bootstrap token, a management token
	// that lives in configuration. Empty unless ACL is on.
	BootstrapToken string

	// NomadUIURL is where a person opens Nomad in a browser (-nomad-ui-url), the
	// base the dashboard's "Open in Nomad" links are built on. Optional: empty
	// shows no link.
	NomadUIURL string

	// Nomad is what the Nomad panel of the Job and Deployment pages reads.
	// Optional: without it the panel is not shown.
	Nomad Nomad

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

	// Version is the build shown in the dashboard's footer and in the
	// /healthz body. Optional: empty shows none.
	Version string

	// Metrics serves GET /metrics (internal/metrics), which checks its own
	// token and never needs a session. Optional: nil serves no /metrics.
	Metrics http.Handler

	// BasePath is the sub path the dashboard is served under (e.g. "/nops"),
	// or "" for the domain root. It is stripped from every incoming request
	// (except /healthz and /metrics, reachable at the bare path too: an
	// orchestrator probes, and Prometheus scrapes, the task's own port,
	// bypassing whatever prefix a reverse proxy mounts it under) and
	// prepended to every generated URL: redirects, cookie paths, template
	// links and static asset addresses. There is no
	// separate flag for it: config.Config reads it from -public-url's own
	// path. See docs/running-nops.md#under-a-sub-path.
	BasePath string

	Log *slog.Logger
}

// server holds the dependencies every handler needs.
type server struct {
	auth      Authenticator
	store     Store
	engine    Engine
	git       Git
	access    AccessStore
	aclOn     bool
	bootstrap string
	nomad     Nomad  // nil: no Nomad panel
	nomadUI   string // "": no links into the Nomad UI
	panels    panelCache
	trigger   func()
	commitURL func(sha string) string
	now       func() time.Time
	secret    []byte
	version   string
	metrics   http.Handler // nil: no /metrics
	basePath  string
	log       *slog.Logger
	tmpl      *template.Template
	static    fs.FS
	started   time.Time
}

// New builds the dashboard and git webhook handler. It parses every template
// once, so a broken one fails loud at startup rather than on the first
// request.
func New(o Options) (http.Handler, error) {
	if o.Auth == nil || o.Store == nil || o.Engine == nil || o.Git == nil || o.Access == nil || o.Log == nil {
		return nil, errors.New("web: Auth, Store, Engine, Git, Access and Log are required")
	}
	if o.ACL && o.BootstrapToken == "" {
		return nil, errors.New("web: BootstrapToken is required when ACL is on")
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
		access:    o.Access,
		aclOn:     o.ACL,
		bootstrap: o.BootstrapToken,
		nomad:     o.Nomad,
		nomadUI:   strings.TrimRight(o.NomadUIURL, "/"),
		trigger:   o.Trigger,
		commitURL: o.CommitURL,
		now:       o.Now,
		secret:    []byte(o.WebhookSecret),
		version:   o.Version,
		metrics:   o.Metrics,
		basePath:  o.BasePath,
		log:       o.Log,
		tmpl:      tmpl,
		static:    staticDir,
		started:   time.Now(),
	}
	return s.handler(), nil
}

// handler wraps the routes with the security headers and, if the dashboard
// is served under a base path, strips it from every incoming request before
// they reach the routes below, which are registered at their bare paths:
// docs/running-nops.md#under-a-sub-path has the reasoning. /healthz and
// /metrics stay reachable at the bare path too, without the prefix, since an
// orchestrator's health check and a Prometheus scrape hit the task's own port
// directly.
func (s *server) handler() http.Handler {
	h := securityHeaders(s.routes())
	if s.basePath == "" {
		return h
	}
	stripped := http.StripPrefix(s.basePath, h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || (s.metrics != nil && r.URL.Path == "/metrics") {
			h.ServeHTTP(w, r)
			return
		}
		stripped.ServeHTTP(w, r)
	})
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
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}
	if len(s.secret) > 0 {
		mux.HandleFunc("POST /webhook/git", s.webhook)
	}

	static := http.FileServerFS(s.static)
	mux.Handle("GET /static/", noStoreExempt(http.StripPrefix("/static/", static)))

	mux.Handle("GET /{$}", s.page(anyToken(), s.index))
	mux.Handle("GET /jobs", s.page(anyToken(), s.jobs))
	mux.Handle("GET /jobs/{namespace}/{job}", s.page(inNamespace(acl.Read), s.job))
	mux.Handle("GET /history", s.page(anyToken(), s.history))
	mux.Handle("GET /drift", s.page(anyToken(), s.drift))
	mux.Handle("GET /deployments/{id}", s.page(onDeployment(acl.Read), s.deployment))
	mux.Handle("GET /deployments/{id}/status", s.page(onDeployment(acl.Read), s.deploymentStatus))
	mux.Handle("POST /deployments/{id}/approve", s.page(onDeployment(acl.Approve), s.approve))
	mux.Handle("POST /deployments/{id}/reject", s.page(onDeployment(acl.Approve), s.reject))
	mux.Handle("POST /deployments/{id}/promote", s.page(onDeployment(acl.Promote), s.promote))
	mux.Handle("POST /deployments/{id}/retry", s.page(onDeployment(acl.Retry), s.retry))
	mux.Handle("POST /jobs/{namespace}/{job}/pause", s.page(inNamespace(acl.Pause), s.pause))
	mux.Handle("POST /jobs/{namespace}/{job}/resume", s.page(inNamespace(acl.Pause), s.resume))
	mux.Handle("POST /jobs/{namespace}/{job}/deploy-now", s.page(inNamespace(acl.DeployNow), s.deployNow))
	if s.trigger != nil {
		mux.Handle("POST /fetch", s.page(globally(acl.Fetch), s.fetchNow))
	}
	mux.Handle("GET /admin", s.page(anyToken(), s.adminPage))
	mux.Handle("GET /admin/policies/{name}", s.page(management(), s.policyPage))
	mux.Handle("POST /admin/policies", s.page(management(), s.savePolicy))
	mux.Handle("POST /admin/policies/{name}/delete", s.page(management(), s.deletePolicy))
	mux.Handle("POST /admin/tokens", s.page(management(), s.createTokenForm))
	mux.Handle("POST /admin/tokens/{accessor}/revoke", s.page(management(), s.revokeTokenForm))

	s.apiRoutes(mux)

	return mux
}

// healthz never requires a session: it is what an orchestrator or a load
// balancer probes. The body names the build ("ok v0.1.0"), so a probe or a
// person can tell which release answers; a probe only reads the status.
func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, strings.TrimSpace("ok "+s.version))
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
