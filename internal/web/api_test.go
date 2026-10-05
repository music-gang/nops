package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// makeToken makes a token for owner and returns its secret and what the store
// keeps of it.
func makeToken(t *testing.T, ts *testServer, owner string, expires time.Time) (string, store.Token) {
	t.Helper()
	secret := newToken()
	tok, err := ts.tokens.CreateToken(t.Context(), owner, "test", hashToken(secret), expires)
	if err != nil {
		t.Fatal(err)
	}
	return secret, tok
}

// apiToken is makeToken for the test that needs only the secret.
func apiToken(t *testing.T, ts *testServer, owner string, expires time.Time) string {
	t.Helper()
	secret, _ := makeToken(t, ts, owner, expires)
	return secret
}

// api sends a request to /api/ with the token (none if empty) and a JSON body
// (none if empty).
func (ts *testServer) api(method, target, token, body string) *httptest.ResponseRecorder {
	ts.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req)
	checkAgainstOpenAPI(ts.t, method, target, rec)
	return rec
}

// checkAgainstOpenAPI fails the test when the answer is not one the
// description of the operation holds.
func checkAgainstOpenAPI(t *testing.T, method, target string, rec *httptest.ResponseRecorder) {
	t.Helper()
	a, err := answers()
	if err != nil {
		t.Fatalf("openapi.json: %v", err)
	}
	if err := a.check(method, target, rec); err != nil {
		t.Errorf("not what openapi.json describes: %v", err)
	}
}

// apiErrorOf reads the {"error": ...} a response carries.
func apiErrorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error == "" {
		t.Fatalf("body %q is not an {\"error\": ...}: %v", rec.Body, err)
	}
	return e.Error
}

func TestAPIRequiresAValidToken(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	valid := apiToken(t, ts, "alice", time.Time{})
	expiring := apiToken(t, ts, "alice", testNow.Add(time.Hour))

	revoked, revokedToken := makeToken(t, ts, "alice", time.Time{})
	if err := ts.tokens.RevokeToken(t.Context(), revokedToken.ID); err != nil {
		t.Fatal(err)
	}

	if rec := ts.api("GET", "/api/jobs", valid, ""); rec.Code != http.StatusOK {
		t.Errorf("a valid token: status %d, want 200: %s", rec.Code, rec.Body)
	}
	for name, header := range map[string]string{
		"no header":      "",
		"another scheme": "Basic " + valid,
		"bearer alone":   "Bearer",
		"unknown token":  "Bearer nops_not-a-token",
		"revoked token":  "Bearer " + revoked,
	} {
		req := httptest.NewRequest("GET", "/api/jobs", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		ts.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
			continue
		}
		apiErrorOf(t, rec)
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate header", name)
		}
	}

	// The same token, once its expiry is past.
	if rec := ts.api("GET", "/api/jobs", expiring, ""); rec.Code != http.StatusOK {
		t.Fatalf("a token before its expiry: status %d, want 200", rec.Code)
	}
	*ts.clock = testNow.Add(2 * time.Hour)
	if rec := ts.api("GET", "/api/jobs", expiring, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a token after its expiry: status %d, want 401", rec.Code)
	}
}

// A token stays valid after its owner leaves the allowlist or the users file
// (docs/api.md#tokens): the API reads the token, never the login.
func TestAPITokenOutlivesItsOwnersLogin(t *testing.T) {
	resumeAsAlice := func(t *testing.T, ts *testServer, token string) {
		t.Helper()
		if rec := ts.api("POST", "/api/jobs/default/web/resume", token, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("status %d, want 204: %s", rec.Code, rec.Body)
		}
		if calls := ts.engine.resumeCalls; len(calls) != 1 || calls[0].actor != "alice" {
			t.Errorf("resume calls = %+v, want one as alice", calls)
		}
	}

	t.Run("allowlist", func(t *testing.T) {
		ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
		token := apiToken(t, ts, "alice", time.Time{})
		ts.auth.opts.AllowedUsers = []string{"bob"} // alice is no longer listed
		resumeAsAlice(t, ts, token)
	})

	t.Run("users file", func(t *testing.T) {
		basic, err := NewBasicAuth(BasicAuthOptions{
			UsersFile: writeUsersFile(t, "bob:"+bcryptHash(t, "s3cret")+"\n"), // no alice
			PublicURL: "http://nops.test",
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		ts := newTestServerLogin(t, &fakeStore{}, &fakeEngine{}, "", nil, "", basic)
		resumeAsAlice(t, ts, apiToken(t, ts, "alice", time.Time{}))
	})
}

// A login session opens the dashboard and a token opens the API: neither
// opens the other, so neither widens what the other allows.
func TestAPIAndDashboardDoNotShareTheirCredentials(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	token := apiToken(t, ts, "alice", time.Time{})

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	req.AddCookie(mintSession(t, ts.auth, "alice"))
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a session on /api/jobs: status %d, want 401", rec.Code)
	}

	for _, target := range []string{"/jobs", "/history", "/tokens"} {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		ts.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), loginPath) {
			t.Errorf("a token on %s: status %d, Location %q, want a redirect to the login", target, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestAPIUnknownPathsAreJSON404BehindTheToken(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	token := apiToken(t, ts, "alice", time.Time{})
	if rec := ts.api("GET", "/api/nothing", token, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path with a token: status %d, want 404", rec.Code)
	} else {
		apiErrorOf(t, rec)
	}
	if rec := ts.api("GET", "/api/nothing", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown path without a token: status %d, want 401: it must not say which paths exist", rec.Code)
	}
}

// Every action calls the engine as the token's owner, the way the dashboard
// calls it as the logged-in user.
func TestAPIActionsActAsTheTokenOwner(t *testing.T) {
	en := &fakeEngine{retryNext: "d2", deployNowNext: "d3"}
	ts := newTestServer(t, &fakeStore{}, en, "")
	token := apiToken(t, ts, "carol", time.Time{})

	for _, c := range []struct {
		name, target, body string
		status             int
		answer             string
	}{
		{"approve", "/api/deployments/d1/approve", `{"spec_hash":"h1"}`, http.StatusNoContent, ""},
		{"reject", "/api/deployments/d1/reject", "", http.StatusNoContent, ""},
		{"promote", "/api/deployments/d1/promote", "", http.StatusNoContent, ""},
		{"retry", "/api/deployments/d1/retry", "", http.StatusOK, `{"deployment_id":"d2"}`},
		{"pause", "/api/jobs/default/web/pause", `{"reason":"db incident"}`, http.StatusNoContent, ""},
		{"resume", "/api/jobs/default/web/resume", "", http.StatusNoContent, ""},
		{"deploy now", "/api/jobs/default/web/deploy-now", `{"spec_hash":"h2"}`, http.StatusOK, `{"deployment_id":"d3"}`},
	} {
		rec := ts.api("POST", c.target, token, c.body)
		if rec.Code != c.status {
			t.Errorf("%s: status %d, want %d: %s", c.name, rec.Code, c.status, rec.Body)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != c.answer {
			t.Errorf("%s: body %q, want %q", c.name, got, c.answer)
		}
	}

	if got, want := fmt.Sprint(en.approveCalls), fmt.Sprint([]approveCall{{"d1", "h1", "carol"}}); got != want {
		t.Errorf("approve calls = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(en.rejectCalls), fmt.Sprint([]rejectCall{{"d1", "carol"}}); got != want {
		t.Errorf("reject calls = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(en.promoteCalls), fmt.Sprint([]promoteCall{{"d1", "carol"}}); got != want {
		t.Errorf("promote calls = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(en.retryCalls), fmt.Sprint([]retryCall{{"d1", "carol"}}); got != want {
		t.Errorf("retry calls = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(en.pauseCalls), fmt.Sprint([]pauseCall{{"default", "web", "carol", "db incident"}}); got != want {
		t.Errorf("pause calls = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(en.resumeCalls), fmt.Sprint([]resumeCall{{"default", "web", "carol"}}); got != want {
		t.Errorf("resume calls = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(en.deployNowCalls), fmt.Sprint([]deployNowCall{{"default", "web", "h2", "carol"}}); got != want {
		t.Errorf("deploy now calls = %s, want %s", got, want)
	}
}

func TestAPIFetch(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	token := apiToken(t, ts, "alice", time.Time{})
	if rec := ts.api("POST", "/api/fetch", token, ""); rec.Code != http.StatusAccepted || ts.trig != 1 {
		t.Errorf("fetch: status %d, polls %d, want 202 and one poll", rec.Code, ts.trig)
	}
	if rec := ts.api("POST", "/api/fetch", "", ""); rec.Code != http.StatusUnauthorized || ts.trig != 1 {
		t.Errorf("fetch without a token: status %d, polls %d, want 401 and no poll", rec.Code, ts.trig)
	}
}

// An approval carries the spec hash the caller saw, and the API refuses one
// without it before it asks the engine anything.
func TestAPIApproveNeedsTheSpecHash(t *testing.T) {
	for _, target := range []string{"/api/deployments/d1/approve", "/api/jobs/default/web/deploy-now"} {
		for name, body := range map[string]string{"no body": "", "empty hash": `{"spec_hash":""}`, "no hash": `{}`} {
			en := &fakeEngine{}
			ts := newTestServer(t, &fakeStore{}, en, "")
			rec := ts.api("POST", target, apiToken(t, ts, "alice", time.Time{}), body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s, %s: status %d, want 400", target, name, rec.Code)
			}
			if len(en.approveCalls)+len(en.deployNowCalls) != 0 {
				t.Errorf("%s, %s: the engine was called", target, name)
			}
		}
	}
}

func TestAPIBadBodies(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	token := apiToken(t, ts, "alice", time.Time{})
	for name, body := range map[string]string{
		"not JSON":      `reason=x`,
		"unknown field": `{"reasn":"typo"}`,
		"too large":     `{"reason":"` + strings.Repeat("a", maxAPIBody) + `"}`,
	} {
		rec := ts.api("POST", "/api/jobs/default/web/pause", token, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
			continue
		}
		apiErrorOf(t, rec)
	}
}

// The API answers an engine refusal with the status the dashboard gives it:
// an approval for an old spec, or for a paused job, is a 409, not a success.
func TestAPIAnswersWhatTheEngineRefuses(t *testing.T) {
	wrapped := func(err error) error { return fmt.Errorf("approve d1: %w", err) }
	for _, c := range []struct {
		name, target, body string
		set                func(*fakeEngine, error)
		err                error
		status             int
	}{
		{"approve, spec changed", "/api/deployments/d1/approve", `{"spec_hash":"old"}`, func(e *fakeEngine, err error) { e.approveErr = err }, wrapped(engine.ErrStaleApproval), 409},
		{"approve, job paused", "/api/deployments/d1/approve", `{"spec_hash":"h"}`, func(e *fakeEngine, err error) { e.approveErr = err }, wrapped(engine.ErrPaused), 409},
		{"approve, not in git", "/api/deployments/d1/approve", `{"spec_hash":"h"}`, func(e *fakeEngine, err error) { e.approveErr = err }, wrapped(engine.ErrNotInRepo), 409},
		{"approve, unknown", "/api/deployments/d1/approve", `{"spec_hash":"h"}`, func(e *fakeEngine, err error) { e.approveErr = err }, wrapped(store.ErrNotFound), 404},
		{"approve, store down", "/api/deployments/d1/approve", `{"spec_hash":"h"}`, func(e *fakeEngine, err error) { e.approveErr = err }, errors.New("disk on fire"), 500},
		{"reject, unknown", "/api/deployments/d1/reject", "", func(e *fakeEngine, err error) { e.rejectErr = err }, store.ErrNotFound, 404},
		{"promote, nothing waits", "/api/deployments/d1/promote", "", func(e *fakeEngine, err error) { e.promoteErr = err }, engine.ErrNotWaitingForPromotion, 409},
		{"promote, unknown", "/api/deployments/d1/promote", "", func(e *fakeEngine, err error) { e.promoteErr = err }, store.ErrNotFound, 404},
		{"retry, not retryable", "/api/deployments/d1/retry", "", func(e *fakeEngine, err error) { e.retryErr = err }, engine.ErrNotRetryable, 409},
		{"retry, already retried", "/api/deployments/d1/retry", "", func(e *fakeEngine, err error) { e.retryErr = err }, store.ErrAlreadyRetried, 409},
		{"retry, unknown", "/api/deployments/d1/retry", "", func(e *fakeEngine, err error) { e.retryErr = err }, store.ErrNotFound, 404},
		{"pause, not in git", "/api/jobs/default/web/pause", "", func(e *fakeEngine, err error) { e.pauseErr = err }, engine.ErrNotPausable, 404},
		{"pause, already paused", "/api/jobs/default/web/pause", "", func(e *fakeEngine, err error) { e.pauseErr = err }, store.ErrAlreadyPaused, 409},
		{"pause, reason too long", "/api/jobs/default/web/pause", "", func(e *fakeEngine, err error) { e.pauseErr = err }, engine.ErrPauseNoteTooLong, 400},
		{"resume, not paused", "/api/jobs/default/web/resume", "", func(e *fakeEngine, err error) { e.resumeErr = err }, store.ErrNotPaused, 409},
		{"resume, unknown", "/api/jobs/default/web/resume", "", func(e *fakeEngine, err error) { e.resumeErr = err }, store.ErrNotFound, 404},
		{"deploy now, not deployable", "/api/jobs/default/web/deploy-now", `{"spec_hash":"h"}`, func(e *fakeEngine, err error) { e.deployNowErr = err }, engine.ErrNotDeployable, 409},
		{"deploy now, unknown", "/api/jobs/default/web/deploy-now", `{"spec_hash":"h"}`, func(e *fakeEngine, err error) { e.deployNowErr = err }, store.ErrNotFound, 404},
	} {
		t.Run(c.name, func(t *testing.T) {
			en := &fakeEngine{}
			c.set(en, c.err)
			ts := newTestServer(t, &fakeStore{}, en, "")
			rec := ts.api("POST", c.target, apiToken(t, ts, "alice", time.Time{}), c.body)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			msg := apiErrorOf(t, rec)
			if c.status == 500 && strings.Contains(msg, "disk on fire") {
				t.Errorf("a 500 quotes the error: %q", msg)
			}
		})
	}
}

func TestAPIJobs(t *testing.T) {
	pending := dep("d1", "web", store.StatePendingApproval, time.Hour)
	en := &fakeEngine{
		observations: []engine.Observation{
			{JobID: "web", Namespace: "default", Policy: meta.PolicyApproval, FilePath: "web.nomad.hcl", Drift: true, SpecHash: "spec-hash-1", ObservedAt: testNow},
			{JobID: "db", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "db.nomad.hcl", Hold: pausedHold(), ObservedAt: testNow},
		},
		orphans: []engine.Orphan{{JobID: "old", Namespace: "default", Policy: store.PolicyAuto, NomadStatus: "running", ObservedAt: testNow}},
	}
	ts := newTestServer(t, &fakeStore{latest: []*store.Deployment{pending}}, en, "")
	rec := ts.api("GET", "/api/jobs", apiToken(t, ts, "alice", time.Time{}), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Jobs []struct {
			Namespace, Job, Policy, Sync string
			Drift                        bool
			SpecHash                     string `json:"spec_hash"`
			Hold                         *struct{ Kind, Reason, By string }
			LastDeployment               *struct{ ID, State, SpecHash string } `json:"last_deployment"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Jobs) != 3 {
		t.Fatalf("jobs = %+v, want three: db, old and web", got.Jobs)
	}
	db, old, web := got.Jobs[0], got.Jobs[1], got.Jobs[2]
	if db.Job != "db" || db.Sync != "paused" || db.Hold == nil || db.Hold.Kind != "paused" || db.Hold.By != "alice" {
		t.Errorf("db = %+v, want paused by alice", db)
	}
	if old.Job != "old" || old.Sync != "orphan" || old.Policy != "auto" {
		t.Errorf("old = %+v, want an orphan", old)
	}
	if web.Job != "web" || web.Sync != "pending" || !web.Drift || web.SpecHash != "spec-hash-1" ||
		web.LastDeployment == nil || web.LastDeployment.ID != "d1" || web.LastDeployment.State != "pending_approval" {
		t.Errorf("web = %+v, want pending approval with drift and its last deployment", web)
	}
	if strings.Contains(rec.Body.String(), specMarker) {
		t.Error("the answer holds a job spec")
	}
}

func TestAPIJob(t *testing.T) {
	diff := diffJSON(t, sampleDiff())
	deps := []*store.Deployment{dep("d2", "web", store.StateFailed, time.Hour), dep("d1", "web", store.StateCompleted, 2*time.Hour)}
	en := &fakeEngine{observations: []engine.Observation{{
		JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "web.nomad.hcl",
		Drift: true, PlanDiff: diff, SpecHash: "h", ObservedAt: testNow,
		Issues: []meta.Issue{{Severity: meta.SeverityWarn, Key: "nops_timeout", Message: "too short"}},
	}}}
	ts := newTestServer(t, &fakeStore{byJob: deps}, en, "")
	token := apiToken(t, ts, "alice", time.Time{})

	rec := ts.api("GET", "/api/jobs/default/web", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Job, File   string
		Diff        struct{ Type string }
		Issues      []struct{ Key, Message string }
		Deployments []struct{ ID string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Job != "web" || got.File != "web.nomad.hcl" || got.Diff.Type != "Edited" ||
		len(got.Issues) != 1 || got.Issues[0].Key != "nops_timeout" || len(got.Deployments) != 2 || got.Deployments[0].ID != "d2" {
		t.Errorf("job = %+v, want the file, the drift diff, the issue and both deployments", got)
	}
	if strings.Contains(rec.Body.String(), specMarker) {
		t.Error("the answer holds a job spec")
	}

	empty := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	if rec := empty.api("GET", "/api/jobs/default/nope", apiToken(t, empty, "alice", time.Time{}), ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: status %d, want 404", rec.Code)
	}
}

func TestAPIDeployments(t *testing.T) {
	st := &fakeStore{
		active:  []*store.Deployment{dep("d3", "web", store.StatePendingApproval, time.Minute)},
		history: []*store.Deployment{dep("d2", "db", store.StateCompleted, time.Hour), dep("d1", "web", store.StateFailed, 2*time.Hour)},
	}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	rec := ts.api("GET", "/api/deployments", apiToken(t, ts, "alice", time.Time{}), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct{ Deployments []struct{ ID, State string } }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range got.Deployments {
		ids = append(ids, d.ID+":"+d.State)
	}
	if want := "d3:pending_approval d2:completed d1:failed"; strings.Join(ids, " ") != want {
		t.Errorf("deployments = %v, want %s: the active and the finished, newest first", ids, want)
	}
}

func TestAPIDeployment(t *testing.T) {
	d := sampleDeployment()
	d.PlanDiff = diffJSON(t, sampleDiff())
	st := &fakeStore{
		deployment: d,
		events:     []store.Event{{Time: testNow, From: store.StateDetected, To: store.StatePendingApproval, Actor: "nops"}},
		hookRuns:   map[string]*store.HookRun{"d1/pre": {Phase: "pre", HookJobID: "web-migrate", State: store.HookSucceeded, StartedAt: testNow}},
	}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	token := apiToken(t, ts, "alice", time.Time{})

	rec := ts.api("GET", "/api/deployments/d1", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		ID       string
		SpecHash string `json:"spec_hash"`
		Diff     struct{ Type string }
		Events   []struct{ From, To, Actor string }
		HookRuns []struct{ Phase, Job, State string } `json:"hook_runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "d1" || got.SpecHash != "spec-hash-1" || got.Diff.Type != "Edited" ||
		len(got.Events) != 1 || got.Events[0].To != "pending_approval" || len(got.HookRuns) != 1 || got.HookRuns[0].Job != "web-migrate" {
		t.Errorf("deployment = %+v, want its spec hash, diff, events and hook runs", got)
	}
	if strings.Contains(rec.Body.String(), specMarker) {
		t.Error("the answer holds the job spec, which is not redacted")
	}

	empty := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	if rec := empty.api("GET", "/api/deployments/nope", apiToken(t, empty, "alice", time.Time{}), ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown deployment: status %d, want 404", rec.Code)
	}
}
