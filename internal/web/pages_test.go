package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/meta"
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
	events     []store.Event
	eventsErr  error
	hookRuns   map[string]*store.HookRun // key: deploymentID+"/"+phase
	hookErr    error
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

type fakeEngine struct {
	approveErr   error
	rejectErr    error
	approveCalls []approveCall
	rejectCalls  []rejectCall
	observations []engine.Observation
}

func (f *fakeEngine) Approve(ctx context.Context, id, specHash, actor string) error {
	f.approveCalls = append(f.approveCalls, approveCall{id, specHash, actor})
	return f.approveErr
}

func (f *fakeEngine) Reject(ctx context.Context, id, actor string) error {
	f.rejectCalls = append(f.rejectCalls, rejectCall{id, actor})
	return f.rejectErr
}

func (f *fakeEngine) Observations() []engine.Observation { return f.observations }

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

type testServer struct {
	t      *testing.T
	h      http.Handler
	auth   *Auth
	logs   *syncBuffer
	trig   int
	engine *fakeEngine
}

func newTestServer(t *testing.T, st Store, en *fakeEngine, secret string) *testServer {
	t.Helper()
	ts := &testServer{t: t, auth: newTestAuth(t), logs: &syncBuffer{}, engine: en}
	log := slog.New(slog.NewTextHandler(ts.logs, nil))
	h, err := New(Options{
		Auth: ts.auth, Store: st, Engine: en, WebhookSecret: secret,
		Trigger: func() { ts.trig++ },
		Log:     log,
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

func sampleDeployment() *store.Deployment {
	return &store.Deployment{
		ID: "d1", JobID: "web", Namespace: "default",
		CommitSHA: "abc123def456", SpecHash: "spec-hash-1",
		State: store.StatePendingApproval, PlanDiff: "",
		JobSpec:   "TOP-SECRET-JOB-SPEC-MARKER",
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// -- authentication: every dashboard page goes through Auth.Require --------

func TestPagesRequireLogin(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{}
	ts := newTestServer(t, st, en, "")

	getPaths := []string{"/", "/history", "/drift", "/deployments/d1", "/deployments/d1/status"}
	for _, p := range getPaths {
		rec := ts.do("GET", p, nil)
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s without session: status %d, want %d (redirect to login)", p, rec.Code, http.StatusFound)
		}
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/auth/login") {
			t.Errorf("GET %s without session: Location %q, want it to start with /auth/login", p, loc)
		}
	}

	postPaths := []string{"/deployments/d1/approve", "/deployments/d1/reject"}
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

// -- security headers -------------------------------------------------------

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

// -- pages render, and never leak job_spec ----------------------------------

func TestIndexPage(t *testing.T) {
	pending := sampleDeployment()
	active := sampleDeployment()
	active.ID, active.State = "d2", store.StateApplying
	st := &fakeStore{active: []*store.Deployment{pending, active}}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("GET", "/", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"alice", "default/web", "Pending approval", "Applying"} {
		if !strings.Contains(body, want) {
			t.Errorf("index body missing %q", want)
		}
	}
	if strings.Contains(body, "TOP-SECRET-JOB-SPEC-MARKER") {
		t.Error("index page leaks JobSpec")
	}
}

func TestHistoryPage(t *testing.T) {
	d := sampleDeployment()
	d.State, d.DecidedBy, d.DecidedAt = store.StateCompleted, "alice", time.Now()
	st := &fakeStore{history: []*store.Deployment{d}}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("GET", "/history", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Completed") {
		t.Error("history page missing the deployment's state")
	}
}

func TestDriftPage(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{
		{JobID: "batch", Namespace: "default", Policy: meta.PolicyNone, Drift: false,
			Issues: []meta.Issue{{Severity: meta.SeverityWarn, Key: "nops_policy", Message: "bad value"}}},
	}}
	ts := newTestServer(t, &fakeStore{}, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("GET", "/drift", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"default/batch", "nops_policy", "bad value"} {
		if !strings.Contains(body, want) {
			t.Errorf("drift page missing %q", want)
		}
	}
}

func TestDriftPageInvalidDiffIsAServerError(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{
		{JobID: "web", Namespace: "default", Drift: true, PlanDiff: "not json"},
	}}
	ts := newTestServer(t, &fakeStore{}, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("GET", "/drift", nil, cookie)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if !strings.Contains(ts.logs.String(), "level=ERROR") {
		t.Error("a server error must be logged at ERROR (fail loud)")
	}
}

func TestDeploymentPage(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment(), events: []store.Event{
		{From: store.StateDetected, To: store.StatePendingApproval, Actor: "nops", Message: "drift detected", Time: time.Now()},
	}}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("GET", "/deployments/d1", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"default/web", "spec-hash-1", "drift detected", "Approve", "Reject"} {
		if !strings.Contains(body, want) {
			t.Errorf("deployment page missing %q", want)
		}
	}
	if strings.Contains(body, "TOP-SECRET-JOB-SPEC-MARKER") {
		t.Error("deployment page leaks JobSpec")
	}
}

func TestDeploymentPageNotFound(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "") // no deployment set: GetDeployment returns ErrNotFound
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("GET", "/deployments/missing", nil, cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestDeploymentPageStoreError(t *testing.T) {
	st := &fakeStore{getErr: errors.New("database is locked")}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("GET", "/deployments/d1", nil, cookie)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "database is locked") {
		t.Error("a Nomad/SQLite error must never reach the response body")
	}
	if !strings.Contains(ts.logs.String(), "database is locked") {
		t.Error("the error must still be logged (fail loud)")
	}
}

func TestDeploymentStatusFragmentStopsPollingWhenTerminal(t *testing.T) {
	d := sampleDeployment()
	d.State = store.StateCompleted
	st := &fakeStore{deployment: d}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("GET", "/deployments/d1/status", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hx-trigger") {
		t.Error("a terminal deployment's status fragment must not keep polling")
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
