package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// -- fakes -----------------------------------------------------------------

type fakeStore struct {
	deployment *store.Deployment
	getErr     error
	active     []*store.Deployment
	activeErr  error
	history    []*store.Deployment
	historyErr error
	byJob      []*store.Deployment // what ListByJob returns, whatever the job
	byJobErr   error
	latest     []*store.Deployment
	latestErr  error
	events     []store.Event
	eventsErr  error
	hookRuns   map[string]*store.HookRun // key: deploymentID+"/"+phase
	hookErr    error
	hooks      []store.DeploymentHook // what DeploymentHooks returns, whatever the deployment
	hooksErr   error
}

func (f *fakeStore) DeploymentHooks(ctx context.Context, deploymentID string) ([]store.DeploymentHook, error) {
	return f.hooks, f.hooksErr
}

// sampleHooks are the hooks sampleDeployment froze: a pre-hook and a post-hook.
func sampleHooks() []store.DeploymentHook {
	return []store.DeploymentHook{
		{Phase: "pre", HookID: "web-migrate", Revision: "web-migrate-0a1b2c3d"},
		{Phase: "post", HookID: "web-smoke", Revision: "web-smoke-9f8e7d6c"},
	}
}

func (f *fakeStore) GetDeployment(ctx context.Context, id string) (*store.Deployment, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.deployment == nil {
		return nil, store.ErrNotFound
	}
	return f.deployment, nil
}

func (f *fakeStore) ListActive(ctx context.Context) ([]*store.Deployment, error) {
	return f.active, f.activeErr
}

func (f *fakeStore) ListHistory(ctx context.Context, limit int) ([]*store.Deployment, error) {
	return f.history, f.historyErr
}

func (f *fakeStore) ListByJob(ctx context.Context, namespace, jobID string, limit int) ([]*store.Deployment, error) {
	return f.byJob, f.byJobErr
}

func (f *fakeStore) LatestPerJob(ctx context.Context) ([]*store.Deployment, error) {
	return f.latest, f.latestErr
}

func (f *fakeStore) Events(ctx context.Context, deploymentID string) ([]store.Event, error) {
	return f.events, f.eventsErr
}

func (f *fakeStore) GetHookRun(ctx context.Context, deploymentID, phase string) (*store.HookRun, error) {
	if f.hookErr != nil {
		return nil, f.hookErr
	}
	run, ok := f.hookRuns[deploymentID+"/"+phase]
	if !ok {
		return nil, store.ErrNotFound
	}
	return run, nil
}

type approveCall struct{ id, specHash, actor string }
type rejectCall struct{ id, actor string }
type retryCall struct{ namespace, job, actor string }

type fakeEngine struct {
	approveErr   error
	rejectErr    error
	retryErr     error
	approveCalls []approveCall
	rejectCalls  []rejectCall
	retryCalls   []retryCall
	observations []engine.Observation
	orphans      []engine.Orphan
	status       engine.Status
}

func (f *fakeEngine) Approve(ctx context.Context, id, specHash, actor string) error {
	f.approveCalls = append(f.approveCalls, approveCall{id, specHash, actor})
	return f.approveErr
}

func (f *fakeEngine) Reject(ctx context.Context, id, actor string) error {
	f.rejectCalls = append(f.rejectCalls, rejectCall{id, actor})
	return f.rejectErr
}

func (f *fakeEngine) Retry(ctx context.Context, namespace, jobID, actor string) error {
	f.retryCalls = append(f.retryCalls, retryCall{namespace, jobID, actor})
	return f.retryErr
}

func (f *fakeEngine) Observations() []engine.Observation { return f.observations }
func (f *fakeEngine) Orphans() []engine.Orphan           { return f.orphans }
func (f *fakeEngine) Status() engine.Status              { return f.status }

type fakeGit struct {
	snap   gitwatch.Snapshot
	status gitwatch.Status
}

func (f *fakeGit) Snapshot() gitwatch.Snapshot { return f.snap }
func (f *fakeGit) Status() gitwatch.Status     { return f.status }

// -- test setup --------------------------------------------------------------

func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	a, err := NewAuth(AuthOptions{
		Issuer: "https://idp.test", ClientID: "nops", ClientSecret: "secret",
		RedirectURL:  "http://nops.test/auth/callback",
		AllowedUsers: []string{"alice"},
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// mintSession signs a session cookie directly, skipping the OIDC round trip
// tested end to end in auth_test.go: what these tests need is a page behind
// a valid session, not the login flow itself.
func mintSession(t *testing.T, a *Auth, actor string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	sess := sessionData{User: actor, Expires: a.now().Add(time.Hour).Unix()}
	if !a.setCookie(rec, sessionCookie, "/", sess, time.Hour) {
		t.Fatal("mint session: setCookie failed")
	}
	c := cookie(rec, sessionCookie)
	if c == nil {
		t.Fatal("mint session: no cookie set")
	}
	return c
}

// testNow is the clock every page test reads: relative times are counted from
// it, so "created 1h ago" does not depend on when the tests run.
var testNow = time.Date(2026, 1, 2, 4, 4, 5, 0, time.UTC)

type testServer struct {
	t      *testing.T
	h      http.Handler
	auth   *Auth
	logs   *syncBuffer
	trig   int
	engine *fakeEngine
	git    *fakeGit
}

func newTestServer(t *testing.T, st Store, en *fakeEngine, secret string) *testServer {
	t.Helper()
	ts := &testServer{t: t, auth: newTestAuth(t), logs: &syncBuffer{}, engine: en, git: &fakeGit{}}
	log := slog.New(slog.NewTextHandler(ts.logs, nil))
	h, err := New(Options{
		Auth: ts.auth, Store: st, Engine: en, Git: ts.git, WebhookSecret: secret,
		Trigger:   func() { ts.trig++ },
		CommitURL: func(sha string) string { return "https://git.test/commit/" + sha },
		Now:       func() time.Time { return testNow },
		Log:       log,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts.h = h
	return ts
}

func (ts *testServer) do(method, target string, body *strings.Reader, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	ts.t.Helper()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, target, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req)
	return rec
}

// get is a GET of target behind a session as alice, failing the test unless
// the answer is 200; it returns the body.
func (ts *testServer) get(target string) string {
	ts.t.Helper()
	rec := ts.do("GET", target, nil, mintSession(ts.t, ts.auth, "alice"))
	if rec.Code != http.StatusOK {
		ts.t.Fatalf("GET %s: status %d, body %s", target, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// specMarker is inside every sample deployment's JobSpec: it must never reach
// a page.
const specMarker = "TOP-SECRET-JOB-SPEC-MARKER"

func sampleDeployment() *store.Deployment {
	return &store.Deployment{
		ID: "d1", JobID: "web", Namespace: "default",
		CommitSHA: "abc123def456", CommitSubject: "feat(web): scale up", CommitAuthor: "Iacopo",
		SpecHash: "spec-hash-1", Policy: store.PolicyApproval, CASIndex: 42,
		State: store.StatePendingApproval, PlanDiff: "",
		JobSpec:   `{"ID":"web","Name":"` + specMarker + `","Meta":{"nops_pre_hook":"web-migrate","nops_pre_hook_timeout":"10m","nops_post_hook":"web-smoke"}}`,
		CreatedAt: testNow.Add(-time.Hour),
		UpdatedAt: testNow.Add(-time.Hour),
	}
}

// dep is a sample deployment of a job, in a state, created age ago.
func dep(id, job string, st store.State, age time.Duration) *store.Deployment {
	d := sampleDeployment()
	d.ID, d.JobID, d.State = id, job, st
	d.CreatedAt, d.UpdatedAt = testNow.Add(-age), testNow.Add(-age)
	return d
}

// mustContain fails for every want the page lacks.
func mustContain(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

// mustNotContain fails for every unwanted string the page has.
func mustNotContain(t *testing.T, page string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(page, u) {
			t.Errorf("page has %q, which it must not", u)
		}
	}
}

// -- authentication: every dashboard page goes through Auth.Require --------

func TestPagesRequireLogin(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{}
	ts := newTestServer(t, st, en, "")

	getPaths := []string{"/", "/jobs", "/jobs/default/web", "/history", "/drift", "/deployments/d1", "/deployments/d1/status"}
	for _, p := range getPaths {
		rec := ts.do("GET", p, nil)
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s without session: status %d, want %d (redirect to login)", p, rec.Code, http.StatusFound)
		}
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/auth/login") {
			t.Errorf("GET %s without session: Location %q, want it to start with /auth/login", p, loc)
		}
	}

	postPaths := []string{"/deployments/d1/approve", "/deployments/d1/reject", "/jobs/default/web/retry", "/fetch"}
	for _, p := range postPaths {
		rec := ts.do("POST", p, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without session: status %d, want %d", p, rec.Code, http.StatusUnauthorized)
		}
	}
}

// healthz and the static assets need no session.
func TestHealthzAndStaticAreOpen(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")

	rec := ts.do("GET", "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: status %d", rec.Code)
	}

	rec = ts.do("GET", "/static/app.css", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("static: status %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("static Cache-Control = %q", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	rec := ts.do("GET", "/healthz", nil)
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("healthz Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
}

// -- approve / reject ---------------------------------------------------------

func formBody(v url.Values) *strings.Reader { return strings.NewReader(v.Encode()) }

func TestApprove(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{}
	ts := newTestServer(t, st, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("POST", "/deployments/d1/approve", formBody(url.Values{"spec_hash": {"spec-hash-1"}}), cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Location"); got != "/deployments/d1" {
		t.Errorf("Location = %q", got)
	}
	if len(en.approveCalls) != 1 || en.approveCalls[0] != (approveCall{"d1", "spec-hash-1", "alice"}) {
		t.Errorf("Approve calls = %+v", en.approveCalls)
	}
}

func TestApproveStaleSpecHashConflict(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{approveErr: engine.ErrStaleApproval}
	ts := newTestServer(t, st, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("POST", "/deployments/d1/approve", formBody(url.Values{"spec_hash": {"stale"}}), cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "spec changed") {
		t.Error("409 page must explain the spec changed")
	}
}

func TestApproveNotFound(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{approveErr: store.ErrNotFound}
	ts := newTestServer(t, st, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("POST", "/deployments/d1/approve", formBody(url.Values{"spec_hash": {"spec-hash-1"}}), cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestReject(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{}
	ts := newTestServer(t, st, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("POST", "/deployments/d1/reject", nil, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if len(en.rejectCalls) != 1 || en.rejectCalls[0] != (rejectCall{"d1", "alice"}) {
		t.Errorf("Reject calls = %+v", en.rejectCalls)
	}
}

// approve/reject go through Auth.Require like every other write: a
// cross-origin POST is refused before the engine is ever called (the
// mechanism itself is exercised in depth by auth_test.go).
func TestApproveRefusesCrossOrigin(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{}
	ts := newTestServer(t, st, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	req := httptest.NewRequest("POST", "/deployments/d1/approve", formBody(url.Values{"spec_hash": {"spec-hash-1"}}))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if len(en.approveCalls) != 0 {
		t.Error("Approve must not be called for a refused cross-origin request")
	}
}
