package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
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
	recent     []*store.Deployment // what ListRecentCompleted returns
	recentErr  error
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
	retryOf    *store.Deployment // what RetryOf returns, ErrNotFound if nil
	retryOfErr error
}

func (f *fakeStore) RetryOf(ctx context.Context, id string) (*store.Deployment, error) {
	if f.retryOfErr != nil {
		return nil, f.retryOfErr
	}
	if f.retryOf == nil {
		return nil, store.ErrNotFound
	}
	return f.retryOf, nil
}

func (f *fakeStore) DeploymentHooks(ctx context.Context, deploymentID string) ([]store.DeploymentHook, error) {
	return f.hooks, f.hooksErr
}

// sampleHooks are the hooks sampleDeployment froze: a pre-hook and a post-hook.
func sampleHooks() []store.DeploymentHook {
	return []store.DeploymentHook{
		{Phase: "pre", HookID: "web-migrate", Revision: "web-migrate-0a1b2c3d",
			JobSpec: `{"ID":"web-migrate","Meta":{"nops_role":"hook","nops_timeout":"10m"}}`},
		{Phase: "post", HookID: "web-smoke", Revision: "web-smoke-9f8e7d6c",
			JobSpec: `{"ID":"web-smoke","Meta":{"nops_role":"hook"}}`},
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

func (f *fakeStore) ListRecentCompleted(ctx context.Context, limit int) ([]*store.Deployment, error) {
	return f.recent, f.recentErr
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

// ListHookRuns returns the runs set for the deployment (keys "<id>/<phase>",
// or "<id>/<phase>/<position>" to add more), pre before post and by position.
func (f *fakeStore) ListHookRuns(ctx context.Context, deploymentID string) ([]*store.HookRun, error) {
	if f.hookErr != nil {
		return nil, f.hookErr
	}
	var out []*store.HookRun
	for k, run := range f.hookRuns {
		if strings.HasPrefix(k, deploymentID+"/") {
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Phase != out[j].Phase {
			return out[i].Phase == "pre"
		}
		return out[i].Position < out[j].Position
	})
	return out, nil
}

type approveCall struct{ id, specHash, actor string }
type rejectCall struct{ id, actor string }
type retryCall struct{ id, actor string }
type promoteCall struct{ id, actor string }
type pauseCall struct{ namespace, job, actor, reason string }
type resumeCall struct{ namespace, job, actor string }

type fakeEngine struct {
	approveErr     error
	rejectErr      error
	retryErr       error
	retryNext      string          // what Retry answers: the deployment that retries; the one asked about if empty
	retryable      map[string]bool // by deployment ID
	promoteErr     error
	pauseErr       error
	resumeErr      error
	deployNowErr   error
	deployNowNext  string // what DeployNow answers: the deployment it created, none if empty
	pauseCalls     []pauseCall
	resumeCalls    []resumeCall
	deployNowCalls []deployNowCall
	promoteCalls   []promoteCall
	approveCalls   []approveCall
	rejectCalls    []rejectCall
	retryCalls     []retryCall
	observations   []engine.Observation
	orphans        []engine.Orphan
	status         engine.Status
	accessor       string // the accessor ID the last Approve ran with
}

func (f *fakeEngine) Approve(ctx context.Context, id, specHash, actor string) error {
	f.approveCalls = append(f.approveCalls, approveCall{id, specHash, actor})
	f.accessor = store.AccessorFrom(ctx)
	return f.approveErr
}

func (f *fakeEngine) Reject(ctx context.Context, id, actor string) error {
	f.rejectCalls = append(f.rejectCalls, rejectCall{id, actor})
	return f.rejectErr
}

func (f *fakeEngine) Retry(ctx context.Context, id, actor string) (string, error) {
	f.retryCalls = append(f.retryCalls, retryCall{id, actor})
	if f.retryErr != nil {
		return "", f.retryErr
	}
	if f.retryNext != "" {
		return f.retryNext, nil
	}
	return id, nil
}

func (f *fakeEngine) Retryable(d, latest *store.Deployment) bool {
	return f.retryable[d.ID] && latest != nil && latest.ID == d.ID
}

type deployNowCall struct{ namespace, jobID, specHash, actor string }

func (f *fakeEngine) DeployNow(ctx context.Context, namespace, jobID, specHash, actor string) (string, error) {
	f.deployNowCalls = append(f.deployNowCalls, deployNowCall{namespace, jobID, specHash, actor})
	return f.deployNowNext, f.deployNowErr
}

func (f *fakeEngine) Promote(ctx context.Context, id, actor string) error {
	f.promoteCalls = append(f.promoteCalls, promoteCall{id, actor})
	return f.promoteErr
}

func (f *fakeEngine) Pause(ctx context.Context, namespace, jobID, actor, reason string) error {
	f.pauseCalls = append(f.pauseCalls, pauseCall{namespace, jobID, actor, reason})
	return f.pauseErr
}

func (f *fakeEngine) Resume(ctx context.Context, namespace, jobID, actor string) error {
	f.resumeCalls = append(f.resumeCalls, resumeCall{namespace, jobID, actor})
	return f.resumeErr
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
	clock  *time.Time // what the server's clock reads: a test moves it
	tokens *store.Store
	aclOn  bool // the server runs with the ACL on
}

// noAccess is the AccessStore of a test that never calls the API: it knows no
// ACL policy and no token.
type noAccess struct{}

func (noAccess) PutACLPolicy(context.Context, store.ACLPolicy, store.Audit) error {
	return errors.New("noAccess: not expected")
}
func (noAccess) ACLPolicy(context.Context, string) (store.ACLPolicy, error) {
	return store.ACLPolicy{}, store.ErrNotFound
}
func (noAccess) ACLPolicies(context.Context) ([]store.ACLPolicy, error) { return nil, nil }
func (noAccess) DeleteACLPolicy(context.Context, string, store.Audit) error {
	return store.ErrNotFound
}
func (noAccess) CreateACLToken(context.Context, store.ACLToken, string, store.Audit) (store.ACLToken, error) {
	return store.ACLToken{}, errors.New("noAccess: not expected")
}
func (noAccess) ACLTokens(context.Context) ([]store.ACLToken, error) { return nil, nil }
func (noAccess) ACLToken(context.Context, string) (store.ACLToken, error) {
	return store.ACLToken{}, store.ErrNotFound
}
func (noAccess) ACLTokenBySecret(context.Context, string, bool) (store.ACLToken, error) {
	return store.ACLToken{}, store.ErrNotFound
}
func (noAccess) RevokeACLToken(context.Context, string, store.Audit) error  { return store.ErrNotFound }
func (noAccess) ACLChanges(context.Context, int) ([]store.ACLChange, error) { return nil, nil }

func newTestServer(t *testing.T, st Store, en *fakeEngine, secret string) *testServer {
	t.Helper()
	return newTestServerWithNomad(t, st, en, secret, nil)
}

// newTestServerWithNomad is newTestServer with a Nomad for the panel (nil: none).
func newTestServerWithNomad(t *testing.T, st Store, en *fakeEngine, secret string, nomad Nomad) *testServer {
	t.Helper()
	return newTestServerOptions(t, st, en, secret, nomad, "")
}

// newTestServerOptions is newTestServerWithNomad with the address the Nomad UI
// is opened at (empty: no links into it).
func newTestServerOptions(t *testing.T, st Store, en *fakeEngine, secret string, nomad Nomad, nomadUI string) *testServer {
	t.Helper()
	return newTestServerLogin(t, st, en, secret, nomad, nomadUI, nil)
}

// newTestServerLogin is newTestServerOptions with the login the server runs
// (nil: ts.auth, OpenID Connect allowing alice).
func newTestServerLogin(t *testing.T, st Store, en *fakeEngine, secret string, nomad Nomad, nomadUI string, login Authenticator, mods ...func(*Options)) *testServer {
	t.Helper()
	now := testNow
	ts := &testServer{t: t, auth: newTestAuth(t), logs: &syncBuffer{}, engine: en, git: &fakeGit{}, clock: &now}
	log := slog.New(slog.NewTextHandler(ts.logs, nil))
	// Real SQLite on the server's clock: what the tokens tests read back is what
	// the store keeps.
	tokens, err := store.Open(filepath.Join(t.TempDir(), "nops.db"), store.WithClock(func() time.Time { return *ts.clock }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tokens.Close() })
	ts.tokens = tokens
	if login == nil {
		login = ts.auth
	}
	opts := Options{
		Auth: login, Store: st, Engine: en, Git: ts.git, Access: tokens, Nomad: nomad, NomadUIURL: nomadUI, WebhookSecret: secret,
		Trigger:   func() { ts.trig++ },
		CommitURL: func(sha string) string { return "https://git.test/commit/" + sha },
		Now:       func() time.Time { return *ts.clock },
		Version:   "v0.0.0-test",
		Log:       log,
	}
	for _, mod := range mods {
		mod(&opts)
	}
	ts.aclOn = opts.ACL
	h, err := New(opts)
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
		JobSpec:   `{"ID":"web","Name":"` + specMarker + `","Meta":{"nops_pre_hook":"web-migrate","nops_post_hook":"web-smoke"}}`,
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

	getPaths := []string{"/", "/jobs", "/jobs/default/web", "/history", "/drift", "/deployments/d1", "/deployments/d1/status", "/admin", "/admin/policies/readers"}
	for _, p := range getPaths {
		rec := ts.do("GET", p, nil)
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s without session: status %d, want %d (redirect to login)", p, rec.Code, http.StatusFound)
		}
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/auth/login") {
			t.Errorf("GET %s without session: Location %q, want it to start with /auth/login", p, loc)
		}
	}

	postPaths := []string{"/deployments/d1/approve", "/deployments/d1/reject", "/deployments/d1/retry", "/fetch", "/admin/policies", "/admin/tokens", "/admin/tokens/x/revoke", "/admin/policies/x/delete"}
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
	if got := rec.Body.String(); got != "ok v0.0.0-test\n" {
		t.Errorf("healthz body = %q, want the version", got)
	}

	rec = ts.do("GET", "/static/app.css", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("static: status %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("static Cache-Control = %q", got)
	}
}

// Every page's footer names the build; a dashboard built without a version
// shows no footer and a bare "ok" on /healthz.
func TestVersionFooter(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	for _, p := range []string{"/", "/jobs", "/history"} {
		if body := ts.get(p); !strings.Contains(body, `<footer class="app-footer muted">nops v0.0.0-test</footer>`) {
			t.Errorf("GET %s: no version footer", p)
		}
	}

	h, err := New(Options{Auth: ts.auth, Store: &fakeStore{}, Engine: &fakeEngine{}, Git: &fakeGit{}, Access: noAccess{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts.h = h
	if body := ts.get("/"); strings.Contains(body, "app-footer") {
		t.Error("footer shown without a version")
	}
	if got := ts.do("GET", "/healthz", nil).Body.String(); got != "ok\n" {
		t.Errorf("healthz body without a version = %q, want \"ok\\n\"", got)
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

// Invariant 3: an approval is valid for the spec_hash the person saw. The
// handler passes on the one in the form, never the one the store holds now: with
// the stored one a spec that changed after the page loaded would be approved.
func TestApprovePassesOnTheHashOfTheFormNotTheStoredOne(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()} // spec-hash-1 in the store
	en := &fakeEngine{}
	ts := newTestServer(t, st, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	ts.do("POST", "/deployments/d1/approve", formBody(url.Values{"spec_hash": {"what-the-page-showed"}}), cookie)

	if len(en.approveCalls) != 1 || en.approveCalls[0] != (approveCall{"d1", "what-the-page-showed", "alice"}) {
		t.Errorf("Approve calls = %+v, want the form's hash, not the stored spec-hash-1", en.approveCalls)
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

func TestApproveOfAJobThatIsNotInGitConflict(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{approveErr: fmt.Errorf("approve d1: %w", engine.ErrNotInRepo)}
	ts := newTestServer(t, st, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("POST", "/deployments/d1/approve", formBody(url.Values{"spec_hash": {"spec-hash-1"}}), cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
	for _, want := range []string{"not in the repository", "nothing to approve", "reject"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("409 page does not say %q", want)
		}
	}
	if strings.Contains(rec.Body.String(), "spec changed") {
		t.Error("409 page blames a changed spec: the job is what is missing")
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
