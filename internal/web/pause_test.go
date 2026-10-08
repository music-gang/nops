package web

import (
	"errors"
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

// pausedHold is what the engine says of a job alice paused an hour ago.
func pausedHold() *engine.Hold {
	return &engine.Hold{
		Kind: engine.HoldPaused, Reason: "paused by alice: db incident",
		By: "alice", Since: testNow.Add(-time.Hour), Note: "db incident",
	}
}

func pausedWeb() engine.Observation {
	return engine.Observation{
		JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "web.nomad.hcl",
		ObservedAt: testNow, Hold: pausedHold(),
	}
}

func TestPause(t *testing.T) {
	en := &fakeEngine{}
	ts := newTestServer(t, &fakeStore{}, en, "")
	cookie := ts.session("alice")

	rec := ts.do("POST", "/jobs/default/web/pause", formBody(url.Values{"reason": {"db incident"}}), cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, Location %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if len(en.pauseCalls) != 1 || en.pauseCalls[0] != (pauseCall{"default", "web", "alice", "db incident"}) {
		t.Errorf("Pause calls = %+v, want one for default/web by alice with the reason", en.pauseCalls)
	}
}

func TestResume(t *testing.T) {
	en := &fakeEngine{}
	ts := newTestServer(t, &fakeStore{}, en, "")
	cookie := ts.session("bob")

	rec := ts.do("POST", "/jobs/default/web/resume", nil, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, Location %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if len(en.resumeCalls) != 1 || en.resumeCalls[0] != (resumeCall{"default", "web", "bob"}) {
		t.Errorf("Resume calls = %+v, want one for default/web by bob", en.resumeCalls)
	}
}

// Like Retry, the page a write returns to is a name from a fixed list.
func TestPauseAndResumeReturnToANamedPage(t *testing.T) {
	obs := engine.Observation{JobID: "web", Namespace: "default"}
	for _, action := range []string{"pause", "resume"} {
		for back, want := range map[string]string{
			"": "/", "jobs": "/jobs", "job": "/jobs/default/web", "https://evil.test": "/", "//evil.test": "/",
		} {
			ts := newTestServer(t, &fakeStore{}, &fakeEngine{observations: []engine.Observation{obs}}, "")
			cookie := ts.session("alice")
			rec := ts.do("POST", "/jobs/default/web/"+action, formBody(url.Values{"back": {back}}), cookie)
			if got := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || got != want {
				t.Errorf("%s back=%q: status %d, Location %q, want 303 to %q", action, back, rec.Code, got, want)
			}
		}
	}
}

func TestPauseAndResumeErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		action string
		en     *fakeEngine
		code   int
		body   string
	}{
		{"pause: not in the repository", "pause", &fakeEngine{pauseErr: engine.ErrNotPausable}, http.StatusNotFound, "nothing to pause"},
		{"pause: already paused", "pause", &fakeEngine{pauseErr: store.ErrAlreadyPaused}, http.StatusConflict, "already paused"},
		{"pause: reason too long", "pause", &fakeEngine{pauseErr: engine.ErrPauseNoteTooLong}, http.StatusBadRequest, "Reason too long"},
		{"pause: anything else", "pause", &fakeEngine{pauseErr: errors.New("sqlite: disk on fire")}, http.StatusInternalServerError, "it has been logged"},
		{"resume: unknown job", "resume", &fakeEngine{resumeErr: store.ErrNotFound}, http.StatusNotFound, "This job does not exist."},
		{"resume: not paused", "resume", &fakeEngine{resumeErr: store.ErrNotPaused}, http.StatusConflict, "not paused"},
		{"resume: anything else", "resume", &fakeEngine{resumeErr: errors.New("sqlite: disk on fire")}, http.StatusInternalServerError, "it has been logged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestServer(t, &fakeStore{}, c.en, "")
			rec := ts.do("POST", "/jobs/default/web/"+c.action, nil, ts.session("alice"))
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.body) {
				t.Fatalf("status %d, want %d with %q; body: %s", rec.Code, c.code, c.body, rec.Body)
			}
			// Never the error text: it can quote SQLite.
			if strings.Contains(rec.Body.String(), "disk on fire") {
				t.Error("the response leaks the underlying error")
			}
		})
	}
}

func TestPauseAndResumeRefuseCrossOrigin(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		en := &fakeEngine{}
		ts := newTestServer(t, &fakeStore{}, en, "")

		req := httptest.NewRequest("POST", "/jobs/default/web/"+action, nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(ts.session("alice"))
		rec := httptest.NewRecorder()
		ts.h.ServeHTTP(rec, req)

		if calls := len(en.pauseCalls) + len(en.resumeCalls); rec.Code != http.StatusForbidden || calls != 0 {
			t.Errorf("%s cross-site: status %d with %d engine calls, want 403 and none", action, rec.Code, calls)
		}
	}
}

// Only a logged-in person can hold a job: the action is theirs, and the pause
// records who.
func TestPauseAndResumeNeedALogin(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		en := &fakeEngine{}
		ts := newTestServer(t, &fakeStore{}, en, "")

		rec := ts.do("POST", "/jobs/default/web/"+action, nil)
		if calls := len(en.pauseCalls) + len(en.resumeCalls); calls != 0 {
			t.Errorf("%s without a session reached the engine (status %d)", action, rec.Code)
		}
	}
}

func TestJobSyncPaused(t *testing.T) {
	errIssue := []meta.Issue{{Severity: meta.SeverityError, Key: "k"}}
	held := pausedHold()
	st := func(s store.State) *store.Deployment { return &store.Deployment{State: s} }
	for _, c := range []struct {
		name   string
		obs    engine.Observation
		latest *store.Deployment
		want   syncKey
	}{
		{"paused with no drift still shows", engine.Observation{Hold: held}, nil, syncPaused},
		{"paused with drift", engine.Observation{Hold: held, Drift: true}, nil, syncPaused},
		{"paused beats blocked", engine.Observation{Hold: held, BlockedBy: "x"}, st(store.StateFailed), syncPaused},
		{"paused beats pending", engine.Observation{Hold: held}, st(store.StatePendingApproval), syncPaused},
		{"invalid beats paused", engine.Observation{Hold: held, Issues: errIssue}, nil, syncInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := jobSync(c.obs, c.latest); got != c.want {
				t.Errorf("jobSync = %q, want %q", got, c.want)
			}
		})
	}
}

func TestJobsPageShowsAPausedJob(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{
		pausedWeb(),
		{JobID: "db", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "db.nomad.hcl"},
	}}
	ts := newTestServer(t, &fakeStore{}, en, "")

	mustContain(t, ts.get("/jobs"), ">Paused<", "paused by alice: db incident", `href="?state=paused"`)

	page := ts.get("/jobs?state=paused")
	mustContain(t, page, `href="/jobs/default/web"`)
	mustNotContain(t, page, `href="/jobs/default/db"`)
}

func TestOverviewListsAPausedJobEvenInSync(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{
		pausedWeb(), // no drift: a forgotten pause must still show
		{JobID: "db", Namespace: "default", Policy: meta.PolicyAuto},
	}}
	page := newTestServer(t, &fakeStore{}, en, "").get("/")

	mustContain(t, page, ">Paused<", "paused by alice: db incident", `href="/jobs/default/web"`,
		`action="/jobs/default/web/resume"`, `name="back" value="overview"`)
	mustNotContain(t, page, "Nothing needs your attention", "default/db")
	if n := strings.Count(page, "Resume</button>"); n != 1 {
		t.Errorf("%d Resume buttons, want 1", n)
	}
}

// Paused goes after what is broken and before what nobody asked for.
func TestOverviewOrdersPausedBetweenFailuresAndOrphans(t *testing.T) {
	failed := dep("f1", "api", store.StateFailed, time.Hour)
	failed.Error = "boom"
	st := &fakeStore{latest: []*store.Deployment{failed}}
	en := &fakeEngine{
		observations: []engine.Observation{pausedWeb()},
		orphans:      []engine.Orphan{{JobID: "old", Namespace: "default", Policy: store.PolicyAuto}},
	}
	page := newTestServer(t, st, en, "").get("/")

	order := []string{">Failed<", ">Paused<", ">Not in git<"}
	last := -1
	for _, want := range order {
		i := strings.Index(page, want)
		if i < 0 || i < last {
			t.Fatalf("%q is at %d, after %d: the list is not ordered %v", want, i, last, order)
		}
		last = i
	}
}

func TestJobPageOfAPausedJob(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{pausedWeb()}}
	page := newTestServer(t, &fakeStore{}, en, "").get("/jobs/default/web")

	mustContain(t, page,
		"Paused by alice", "db incident", "until it is resumed",
		`action="/jobs/default/web/resume"`, `name="back" value="job"`, "Resume</button>")
	// It is paused already: no form to pause it again.
	mustNotContain(t, page, `action="/jobs/default/web/pause"`, "Reason (optional)")
}

func TestJobPageOfAJobThatCanBePaused(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{{
		JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "web.nomad.hcl", ObservedAt: testNow,
	}}}
	page := newTestServer(t, &fakeStore{}, en, "").get("/jobs/default/web")

	mustContain(t, page, `action="/jobs/default/web/pause"`, `name="reason"`, `maxlength="500"`, "Reason (optional)", `name="back" value="job"`)
	mustNotContain(t, page, "Paused by", "Resume")
}

// A job that is not in the repository has nothing to hold.
func TestJobPageNotInTheRepositoryHasNoPauseForm(t *testing.T) {
	st := &fakeStore{byJob: []*store.Deployment{dep("d0", "web", store.StateCompleted, time.Hour)}}
	page := newTestServer(t, st, &fakeEngine{}, "").get("/jobs/default/web")
	mustNotContain(t, page, `/pause"`, "Reason (optional)")
}

func TestDeploymentPageOfAPausedJobCannotBeApproved(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{observations: []engine.Observation{pausedWeb()}}
	page := newTestServer(t, st, en, "").get("/deployments/d1")

	mustContain(t, page,
		"The job is paused", "paused by alice: db incident", "cannot be approved until it is resumed",
		`action="/jobs/default/web/resume"`,
		// Rejecting starts nothing, so it stays.
		`action="/deployments/d1/reject"`)
	mustNotContain(t, page, `action="/deployments/d1/approve"`)
}

func TestDeploymentPageOfAJobThatIsNotPausedCanBeApproved(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{observations: []engine.Observation{{JobID: "web", Namespace: "default"}}}
	page := newTestServer(t, st, en, "").get("/deployments/d1")

	mustContain(t, page, `action="/deployments/d1/approve"`)
	mustNotContain(t, page, "The job is paused")
}

func TestApproveOfAPausedJobConflict(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{approveErr: engine.ErrPaused, observations: []engine.Observation{pausedWeb()}}
	ts := newTestServer(t, st, en, "")

	rec := ts.do("POST", "/deployments/d1/approve", formBody(url.Values{"spec_hash": {"spec-hash-1"}}), ts.session("alice"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
	mustContain(t, rec.Body.String(), "This job is paused", "resume it to approve", "reject")
	mustNotContain(t, rec.Body.String(), "spec changed", "not in the repository")
}

// -- a closed sync window ----------------------------------------------------

func windowHold() *engine.Hold {
	return &engine.Hold{Kind: engine.HoldWindow, Reason: "outside its sync window, next opens Fri 2026-09-25 09:00 UTC"}
}

func TestJobSyncHeld(t *testing.T) {
	st := func(s store.State) *store.Deployment { return &store.Deployment{State: s} }
	for _, c := range []struct {
		name   string
		obs    engine.Observation
		latest *store.Deployment
		want   syncKey
	}{
		{"drifting outside the window", engine.Observation{Hold: windowHold(), Drift: true}, nil, syncHeld},
		{"after a completed deployment", engine.Observation{Hold: windowHold(), Drift: true}, st(store.StateCompleted), syncHeld},
		{"nothing to deploy is in sync, whatever the window", engine.Observation{Hold: windowHold()}, nil, syncInSync},
		{"a deployment in flight is deploying", engine.Observation{Hold: windowHold(), Drift: true}, st(store.StateApplying), syncDeploying},
		{"blocked beats held: there is something to do", engine.Observation{Hold: windowHold(), Drift: true, BlockedBy: "x"}, st(store.StateFailed), syncBlocked},
		{"a pause beats a window", engine.Observation{Hold: pausedHold(), Drift: true}, nil, syncPaused},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := jobSync(c.obs, c.latest); got != c.want {
				t.Errorf("jobSync = %q, want %q", got, c.want)
			}
		})
	}
}

func TestJobsPageShowsAHeldJob(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{
		{JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "web.nomad.hcl", Drift: true, Hold: windowHold()},
		{JobID: "db", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "db.nomad.hcl", Hold: windowHold()}, // in sync
	}}
	ts := newTestServer(t, &fakeStore{}, en, "")

	page := ts.get("/jobs")
	mustContain(t, page, ">Held<", "outside its sync window, next opens Fri 2026-09-25 09:00 UTC", `href="?state=held"`, "In sync")
	if n := strings.Count(page, ">Held<"); n != 1 {
		t.Errorf("%d Held pills, want 1: the job with nothing to deploy is in sync", n)
	}
	mustContain(t, ts.get("/jobs?state=held"), `href="/jobs/default/web"`)
	mustNotContain(t, ts.get("/jobs?state=held"), `href="/jobs/default/db"`)
}

// A window is nobody's decision and needs nobody: not in Needs attention.
func TestOverviewDoesNotListAHeldJob(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{
		{JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, Drift: true, Hold: windowHold()},
	}}
	page := newTestServer(t, &fakeStore{}, en, "").get("/")
	mustContain(t, page, "Nothing needs your attention")
	mustNotContain(t, page, "default/web", ">Held<")
}

func TestJobPageOfAHeldJob(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{{
		JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "web.nomad.hcl", ObservedAt: testNow,
		Drift: true, PlanDiff: diffJSON(t, sampleDiff()), Hold: windowHold(),
	}}}
	page := newTestServer(t, &fakeStore{}, en, "").get("/jobs/default/web")

	mustContain(t, page, "Held:", "outside its sync window, next opens Fri 2026-09-25 09:00 UTC", "only inside its sync window",
		// It can still be paused, and there is no Resume for a window.
		`action="/jobs/default/web/pause"`)
	mustNotContain(t, page, "Resume", "Paused by")

	// Nothing to deploy: no notice.
	en.observations[0].Drift, en.observations[0].PlanDiff = false, ""
	mustNotContain(t, newTestServer(t, &fakeStore{}, en, "").get("/jobs/default/web"), "Held:")
}

// Approve stays open: a window gates what Nops starts on its own.
func TestDeploymentPageOfAHeldJobCanBeApproved(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	en := &fakeEngine{observations: []engine.Observation{{JobID: "web", Namespace: "default", Hold: windowHold()}}}
	page := newTestServer(t, st, en, "").get("/deployments/d1")

	mustContain(t, page, `action="/deployments/d1/approve"`)
	mustNotContain(t, page, "The job is paused")
}

// The window, and the time zone it is read in, are on the page of the job
// whether or not it drifts: nobody should have to know Nops reads it in UTC.
func TestJobPageShowsTheSyncWindowAndItsZone(t *testing.T) {
	closes := time.Date(2026, time.September, 30, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		window  *engine.WindowStatus
		wants   []string
		unwants []string
	}{
		{"open", &engine.WindowStatus{Spec: "0 9 * * *", Duration: 11 * time.Hour, Zone: "Europe/Rome", Open: true, Until: closes},
			[]string{"Window", "open until Wed 2026-09-30 20:00 UTC", "Schedule", "0 9 * * * for 11h, read in Europe/Rome"}, nil},
		{"closed", &engine.WindowStatus{Spec: "0 9 * * *", Duration: 11 * time.Hour, Zone: "UTC", Until: closes},
			[]string{"closed until Wed 2026-09-30 20:00 UTC", "read in UTC"}, []string{"open until"}},
		{"open for ever", &engine.WindowStatus{Spec: "* * * * *", Duration: time.Hour, Zone: "UTC", Open: true},
			[]string{"open, it never closes"}, nil},
		{"none", nil, nil, []string{"Schedule", "read in"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			en := &fakeEngine{observations: []engine.Observation{{
				JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "web.nomad.hcl", ObservedAt: testNow, Window: tc.window,
			}}}
			page := newTestServer(t, &fakeStore{}, en, "").get("/jobs/default/web")
			mustContain(t, page, tc.wants...)
			mustNotContain(t, page, tc.unwants...)
		})
	}
}
