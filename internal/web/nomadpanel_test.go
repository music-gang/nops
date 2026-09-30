package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// fakeNomad answers the panel's three reads and counts them.
type fakeNomad struct {
	job      *api.Job
	jobErr   error
	allocs   []nomadx.Alloc
	allocErr error
	dep      *api.Deployment
	depErr   error

	jobCalls, allocCalls, depCalls int
}

func (f *fakeNomad) Job(_ context.Context, _, _ string) (*api.Job, error) {
	f.jobCalls++
	return f.job, f.jobErr
}

func (f *fakeNomad) Allocations(_ context.Context, _, _ string) ([]nomadx.Alloc, error) {
	f.allocCalls++
	return f.allocs, f.allocErr
}

func (f *fakeNomad) LatestDeployment(_ context.Context, _, _ string) (*api.Deployment, error) {
	f.depCalls++
	return f.dep, f.depErr
}

func (f *fakeNomad) calls() int { return f.jobCalls + f.allocCalls + f.depCalls }

func ptr[T any](v T) *T { return &v }

// canaryNomad is a service at version 4 with two groups, one of them waiting for
// a promotion, and the allocations of its live version and of the one before.
func canaryNomad() *fakeNomad {
	return &fakeNomad{
		job: &api.Job{
			Status: ptr("running"), Type: ptr("service"), Version: ptr(uint64(4)),
			TaskGroups: []*api.TaskGroup{
				{Name: ptr("web"), Count: ptr(3)},
				{Name: ptr("worker"), Count: ptr(1)},
			},
		},
		allocs: []nomadx.Alloc{
			{ID: "a1", TaskGroup: "web", JobVersion: 4, ClientStatus: "running", Healthy: ptr(true), Canary: true},
			{ID: "a2", TaskGroup: "web", JobVersion: 4, ClientStatus: "pending"},
			{ID: "a3", TaskGroup: "web", JobVersion: 4, ClientStatus: "failed"},
			{ID: "a4", TaskGroup: "web", JobVersion: 3, ClientStatus: "running"}, // the version being replaced: not counted
			{ID: "a5", TaskGroup: "worker", JobVersion: 4, ClientStatus: "lost"},
			{ID: "a6", TaskGroup: "gone", JobVersion: 4, ClientStatus: "running"}, // a group the job no longer has
		},
		dep: &api.Deployment{
			ID: "0123456789abcdef", JobModifyIndex: 9, Status: "running", StatusDescription: "Deployment is running but requires manual promotion",
			TaskGroups: map[string]*api.DeploymentState{
				"web": {DesiredTotal: 3, DesiredCanaries: 1, PlacedCanaries: []string{"a1"}, PlacedAllocs: 1, HealthyAllocs: 1,
					RequireProgressBy: testNow.Add(2 * time.Minute)},
			},
		},
	}
}

func applyingDeployment() *store.Deployment {
	d := sampleDeployment()
	d.State, d.AppliedIndex, d.PromotionWaitSince = store.StateApplying, 9, testNow.Add(-time.Minute)
	return d
}

func TestJobPageShowsWhatNomadReports(t *testing.T) {
	ts := newTestServerWithNomad(t, &fakeStore{byJob: []*store.Deployment{applyingDeployment()}}, &fakeEngine{}, "", canaryNomad())
	page := ts.get("/jobs/default/web")

	mustContain(t, page, `id="live-nomad"`, "Nomad", "running", "service, version 4",
		// web: 3 desired; of the live version 1 running, 1 pending, 1 failed, 1 healthy, 1 canary
		`<td class="mono">web</td><td>3</td><td>1</td><td>1</td><td><span class="text-failed">1</span></td><td>0</td><td>1</td><td>1</td>`,
		// worker: 1 desired, its one allocation lost
		`<td class="mono">worker</td><td>1</td><td>0</td><td>0</td><td>0</td><td><span class="text-failed">1</span></td><td>0</td><td>0</td>`,
		"01234567", "Deployment is running but requires manual promotion", "1 / 1", "in 2m")
	mustNotContain(t, page, "gone", `action="/deployments/d1/promote"`) // the Job page carries no button
	if strings.Contains(page, "waits on") {
		t.Error("a job's page says which deployment a nops deployment waits on")
	}
}

func TestDeploymentPageShowsTheNomadDeploymentItWaitsOn(t *testing.T) {
	st := &fakeStore{deployment: applyingDeployment()}
	ts := newTestServerWithNomad(t, st, &fakeEngine{}, "", canaryNomad())

	for _, path := range []string{"/deployments/d1", "/deployments/d1/status"} {
		page := ts.get(path)
		mustContain(t, page, "Nomad deployment", "This is the one this deployment waits on.", "requires manual promotion",
			"Waiting for canary promotion in Nomad.", `action="/deployments/d1/promote"`, ">Promote</button>")
		mustNotContain(t, page, "Not the one tracking")
	}

	// A Nomad deployment of another index is not the one this apply waits on.
	other := canaryNomad()
	other.dep.JobModifyIndex = 12
	ts = newTestServerWithNomad(t, st, &fakeEngine{}, "", other)
	page := ts.get("/deployments/d1")
	mustContain(t, page, "Not the one tracking this deployment")
	mustNotContain(t, page, "This is the one this deployment waits on.")
}

// The panel is for a deployment that is applying, and for one whose Nomad the
// dashboard can read.
func TestNomadPanelOnlyWhereItBelongs(t *testing.T) {
	notApplying := applyingDeployment()
	notApplying.State = store.StateCompleted
	nom := canaryNomad()
	ts := newTestServerWithNomad(t, &fakeStore{deployment: notApplying}, &fakeEngine{}, "", nom)
	mustNotContain(t, ts.get("/deployments/d1"), "Nomad deployment", `<h2 class="box__title">Nomad</h2>`)
	if nom.calls() != 0 {
		t.Errorf("Nomad was asked %d times for a deployment that is not applying", nom.calls())
	}

	ts = newTestServer(t, &fakeStore{deployment: applyingDeployment(), byJob: []*store.Deployment{applyingDeployment()}}, &fakeEngine{}, "")
	mustNotContain(t, ts.get("/deployments/d1"), `<h2 class="box__title">Nomad</h2>`)
	mustNotContain(t, ts.get("/jobs/default/web"), `<h2 class="box__title">Nomad</h2>`, "live-nomad")
}

// What Nomad cannot tell does not break the page: the panel says so, and the
// failure is logged once.
func TestNomadPanelSaysWhenNomadDoesNotAnswer(t *testing.T) {
	for name, tt := range map[string]struct {
		set  func(f *fakeNomad)
		want string
	}{
		"job":         {func(f *fakeNomad) { f.jobErr = errors.New("boom") }, "Nomad did not answer (job): boom"},
		"allocations": {func(f *fakeNomad) { f.allocErr = errors.New("boom") }, "Nomad did not answer (allocations): boom"},
		"deployment":  {func(f *fakeNomad) { f.depErr = errors.New("boom") }, "Nomad did not answer (deployment): boom"},
	} {
		t.Run(name, func(t *testing.T) {
			nom := canaryNomad()
			tt.set(nom)
			ts := newTestServerWithNomad(t, &fakeStore{byJob: []*store.Deployment{applyingDeployment()}}, &fakeEngine{}, "", nom)
			page := ts.get("/jobs/default/web")
			mustContain(t, page, tt.want, "Deployments") // the rest of the page is there
			ts.get("/jobs/default/web")
			if n := strings.Count(ts.logs.String(), "read Nomad for the panel"); n != 1 {
				t.Errorf("%d ERROR lines for two polls inside the cache, want 1:\n%s", n, ts.logs)
			}
			if !strings.Contains(ts.logs.String(), "level=ERROR") {
				t.Errorf("the failure is not logged at ERROR:\n%s", ts.logs)
			}
		})
	}
}

func TestNomadPanelForAJobNomadDoesNotHave(t *testing.T) {
	nom := &fakeNomad{jobErr: nomadx.ErrJobNotFound}
	ts := newTestServerWithNomad(t, &fakeStore{byJob: []*store.Deployment{applyingDeployment()}}, &fakeEngine{}, "", nom)
	mustContain(t, ts.get("/jobs/default/web"), "This job does not exist in Nomad.")
	if strings.Contains(ts.logs.String(), "level=ERROR") {
		t.Errorf("a job Nomad does not have is not an error:\n%s", ts.logs)
	}

	nom = &fakeNomad{job: &api.Job{Status: ptr("running"), Type: ptr("batch"), Version: ptr(uint64(0))}}
	ts = newTestServerWithNomad(t, &fakeStore{byJob: []*store.Deployment{applyingDeployment()}}, &fakeEngine{}, "", nom)
	mustContain(t, ts.get("/jobs/default/web"), "Nomad has no deployment for this job.")
}

// The pages poll every few seconds, in as many tabs as are open: the panel of a
// job is read from Nomad once per TTL.
func TestNomadPanelIsCachedForAShortTime(t *testing.T) {
	nom := canaryNomad()
	ts := newTestServerWithNomad(t, &fakeStore{byJob: []*store.Deployment{applyingDeployment()}, deployment: applyingDeployment()}, &fakeEngine{}, "", nom)

	ts.get("/jobs/default/web")
	ts.get("/jobs/default/web")
	ts.get("/deployments/d1") // the same job, from another page
	if nom.jobCalls != 1 || nom.allocCalls != 1 || nom.depCalls != 1 {
		t.Fatalf("Nomad calls = job %d, allocations %d, deployment %d after three renders inside the TTL, want 1 each",
			nom.jobCalls, nom.allocCalls, nom.depCalls)
	}

	*ts.clock = ts.clock.Add(panelTTL - time.Second)
	ts.get("/jobs/default/web")
	if nom.jobCalls != 1 {
		t.Errorf("job calls = %d just before the TTL, want the cached panel", nom.jobCalls)
	}
	*ts.clock = ts.clock.Add(2 * time.Second)
	page := ts.get("/jobs/default/web")
	if nom.jobCalls != 2 || nom.allocCalls != 2 || nom.depCalls != 2 {
		t.Errorf("Nomad calls = job %d, allocations %d, deployment %d after the TTL, want 2 each", nom.jobCalls, nom.allocCalls, nom.depCalls)
	}
	// The progress deadline is relative to now, so it moves although the panel is cached.
	mustContain(t, page, "in 1m")

	// Another job is another entry.
	before := nom.jobCalls
	ts.get("/jobs/default/api")
	if nom.jobCalls != before+1 {
		t.Errorf("job calls = %d, want %d for another job", nom.jobCalls, before+1)
	}
}

func TestPromote(t *testing.T) {
	en := &fakeEngine{}
	ts := newTestServer(t, &fakeStore{}, en, "")
	rec := ts.do("POST", "/deployments/d1/promote", nil, mintSession(t, ts.auth, "alice"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/deployments/d1" {
		t.Fatalf("status %d, Location %q, want 303 to /deployments/d1", rec.Code, rec.Header().Get("Location"))
	}
	if len(en.promoteCalls) != 1 || en.promoteCalls[0] != (promoteCall{"d1", "alice"}) {
		t.Errorf("Promote calls = %+v, want one for d1 by alice", en.promoteCalls)
	}
}

func TestPromoteNeedsALogin(t *testing.T) {
	en := &fakeEngine{}
	ts := newTestServer(t, &fakeStore{}, en, "")
	rec := ts.do("POST", "/deployments/d1/promote", nil)
	if rec.Code == http.StatusSeeOther && strings.HasSuffix(rec.Header().Get("Location"), "/deployments/d1") {
		t.Errorf("an anonymous promote was accepted: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if len(en.promoteCalls) != 0 {
		t.Errorf("Promote calls = %+v, want none without a session", en.promoteCalls)
	}
}

func TestPromoteRefusals(t *testing.T) {
	for name, tt := range map[string]struct {
		err  error
		code int
		body string
	}{
		"not waiting any more": {engine.ErrNotWaitingForPromotion, http.StatusConflict, "Nothing to promote"},
		"unknown deployment":   {store.ErrNotFound, http.StatusNotFound, ""},
		"nomad refuses":        {errors.New("permission denied"), http.StatusInternalServerError, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, &fakeStore{}, &fakeEngine{promoteErr: tt.err}, "")
			rec := ts.do("POST", "/deployments/d1/promote", nil, mintSession(t, ts.auth, "alice"))
			if rec.Code != tt.code || !strings.Contains(rec.Body.String(), tt.body) {
				t.Errorf("status %d, want %d with %q; body:\n%s", rec.Code, tt.code, tt.body, rec.Body)
			}
		})
	}
}

// The button is only offered while the deployment waits for a promotion.
func TestPromoteButtonOnlyDuringTheWait(t *testing.T) {
	waiting := applyingDeployment()
	notWaiting := applyingDeployment()
	notWaiting.PromotionWaitSince = time.Time{}
	promoted := applyingDeployment()
	promoted.PromotedAt = testNow.Add(-time.Second)
	for name, tt := range map[string]struct {
		d    *store.Deployment
		want bool
	}{"waiting": {waiting, true}, "not waiting": {notWaiting, false}, "promoted": {promoted, false}} {
		page := newTestServerWithNomad(t, &fakeStore{deployment: tt.d}, &fakeEngine{}, "", canaryNomad()).get("/deployments/d1")
		if got := strings.Contains(page, `action="/deployments/d1/promote"`); got != tt.want {
			t.Errorf("%s: button shown = %v, want %v", name, got, tt.want)
		}
	}
}

// A timeline entry that stays in a state (the wait, the promotion request) is
// not a retry.
func TestTimelineNamesEventsThatStayInAState(t *testing.T) {
	d := applyingDeployment()
	st := &fakeStore{deployment: d, events: []store.Event{
		{From: store.StateApplying, To: store.StateApplying, Actor: "nops", Message: "waiting for canary promotion in Nomad", Time: testNow},
		{From: store.StateApplying, To: store.StateApplying, Actor: "alice", Message: "promotion requested", Time: testNow},
		{From: store.StateFailed, To: store.StateFailed, Actor: "bob", Message: "retry requested", Time: testNow},
	}}
	page := newTestServer(t, st, &fakeEngine{}, "").get("/deployments/d1")
	mustContain(t, page, "waiting for canary promotion in Nomad", "promotion requested", `<span class="row__transition">Applying</span>`,
		`<span class="row__transition">retry requested</span>`)
	if n := strings.Count(page, `<span class="row__transition">retry requested</span>`); n != 1 {
		t.Errorf("%d retry rows, want 1 (only the terminal one)", n)
	}
}

func TestUntil(t *testing.T) {
	srv := &server{now: func() time.Time { return testNow }}
	for name, tt := range map[string]struct {
		t    time.Time
		want string
	}{
		"none":            {time.Time{}, ""},
		"seconds ahead":   {testNow.Add(20 * time.Second), "in under a minute"},
		"minutes ahead":   {testNow.Add(150 * time.Second), "in 2m"},
		"hours ahead":     {testNow.Add(5 * time.Hour), "in 5h"},
		"days ahead":      {testNow.Add(50 * time.Hour), "in 2d"},
		"a minute behind": {testNow.Add(-90 * time.Second), "1m ago"},
	} {
		if got := srv.until(tt.t).Rel; got != tt.want {
			t.Errorf("%s: until = %q, want %q", name, got, tt.want)
		}
	}
}
