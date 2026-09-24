package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

func diffJSON(t *testing.T, d *api.JobDiff) string {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sampleDiff changes one job field and, inside task group g / task t, two more.
func sampleDiff() *api.JobDiff {
	return &api.JobDiff{
		Type:   "Edited",
		Fields: []*api.FieldDiff{{Type: "Edited", Name: "Priority", Old: "50", New: "60"}},
		TaskGroups: []*api.TaskGroupDiff{{
			Type: "Edited", Name: "g",
			Tasks: []*api.TaskDiff{{
				Type: "Edited", Name: "t",
				Fields:  []*api.FieldDiff{{Type: "Added", Name: "Env[FOO]", Old: "", New: "<redacted>"}},
				Objects: []*api.ObjectDiff{{Type: "Edited", Name: "Resources", Fields: []*api.FieldDiff{{Type: "Deleted", Name: "CPU", Old: "100"}}}},
			}},
		}},
	}
}

// -- helpers -----------------------------------------------------------------

func TestRelative(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{
		{-time.Hour, "just now"}, // a clock a little ahead of ours
		{0, "just now"},
		{59 * time.Second, "just now"},
		{time.Minute, "1m ago"},
		{59*time.Minute + 59*time.Second, "59m ago"},
		{time.Hour, "1h ago"},
		{23 * time.Hour, "23h ago"},
		{24 * time.Hour, "1d ago"},
		{29 * 24 * time.Hour, "29d ago"},
		{30 * 24 * time.Hour, "2026-08-25"},
		{400 * 24 * time.Hour, "2025-08-20"},
	} {
		if got := relative(now, now.Add(-c.ago)); got != c.want {
			t.Errorf("relative(%v ago) = %q, want %q", c.ago, got, c.want)
		}
	}
}

func TestWhen(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	_ = ts
	s := &server{now: func() time.Time { return testNow }}
	if got := s.when(time.Time{}); got != (timeView{}) {
		t.Errorf("when(zero) = %+v, want the zero timeView", got)
	}
	got := s.when(testNow.Add(-90 * time.Minute))
	if got.Rel != "1h ago" || got.Full != "2026-01-02 02:34:05 UTC" || got.ISO != "2026-01-02T02:34:05Z" {
		t.Errorf("when = %+v", got)
	}
}

func TestDayLabel(t *testing.T) {
	today := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), "Today"},
		{time.Date(2026, 9, 24, 23, 59, 0, 0, time.UTC), "Today"},
		{time.Date(2026, 9, 23, 23, 59, 0, 0, time.UTC), "Yesterday"},
		{time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), "Tue, 22 Sep 2026"},
	} {
		if got := dayLabel(today, c.t); got != c.want {
			t.Errorf("dayLabel(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestShort(t *testing.T) {
	for _, c := range []struct {
		n    int
		in   string
		want string
	}{{12, "0123456789abcdef", "0123456789ab"}, {12, "short", "short"}, {8, "", ""}, {3, "abc", "abc"}} {
		if got := short(c.n, c.in); got != c.want {
			t.Errorf("short(%d, %q) = %q, want %q", c.n, c.in, got, c.want)
		}
	}
}

func TestDuration(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{10 * time.Minute, "10m"},
		{5 * time.Minute, "5m"},
		{time.Hour, "1h"},
		{90 * time.Minute, "1h30m"},
		{time.Minute + 30*time.Second, "1m30s"},
		{45 * time.Second, "45s"},
		{10 * time.Second, "10s"},
		{20 * time.Hour, "20h"},
		{0, "0s"},
	} {
		if got := duration(c.d); got != c.want {
			t.Errorf("duration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestSummarize(t *testing.T) {
	if s := summarize(nil); s.Total() != 0 || len(s.Places) != 0 {
		t.Errorf("summarize(nil) = %+v, want nothing", s)
	}
	s := summarize(sampleDiff())
	if s.Added != 1 || s.Edited != 1 || s.Deleted != 1 || s.Total() != 3 {
		t.Errorf("counts = +%d ~%d -%d, want one each", s.Added, s.Edited, s.Deleted)
	}
	want := []diffPlace{
		{Kind: "job", Type: "Edited", Changes: 1},
		{Kind: "group", Name: "g", Type: "Edited", Changes: 0}, // nothing directly in the group: not listed
		{Kind: "task", Name: "g/t", Type: "Edited", Changes: 2},
	}
	// The group has no field of its own and is only Edited because a task in it
	// is: it is not a place by itself.
	want = []diffPlace{want[0], want[2]}
	if len(s.Places) != len(want) {
		t.Fatalf("places = %+v, want %+v", s.Places, want)
	}
	for i := range want {
		if s.Places[i] != want[i] {
			t.Errorf("place %d = %+v, want %+v", i, s.Places[i], want[i])
		}
	}

	// A whole task group or task that appears or goes counts as a place even
	// with no field of its own, and an unchanged field counts for nothing.
	added := summarize(&api.JobDiff{TaskGroups: []*api.TaskGroupDiff{
		{Type: "Added", Name: "new", Fields: []*api.FieldDiff{{Type: "None", Name: "Count"}}},
		{Type: "Deleted", Name: "old", Tasks: []*api.TaskDiff{{Type: "Deleted", Name: "t"}}},
	}})
	if added.Total() != 0 || len(added.Places) != 3 {
		t.Errorf("summary of added and deleted groups = %+v, want 3 places and no field", added)
	}
}

func TestPlanSteps(t *testing.T) {
	d := sampleDeployment()
	steps, err := planSteps(d)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, len(steps))
	for i, st := range steps {
		kinds[i] = st.Kind
	}
	if got := strings.Join(kinds, ","); got != "pre,register,health,post" {
		t.Fatalf("steps = %s, want pre,register,health,post", got)
	}
	if steps[0].Job != "web-migrate" || steps[0].Timeout != "10m" || steps[3].Job != "web-smoke" {
		t.Errorf("hooks = %+v / %+v", steps[0], steps[3])
	}
	if !strings.Contains(steps[1].Text, "index 42") {
		t.Errorf("register step = %q, want the CAS index", steps[1].Text)
	}

	d.CASIndex = 0
	d.JobSpec = `{"ID":"web"}`
	steps, err = planSteps(d)
	if err != nil || len(steps) != 2 || !strings.HasPrefix(steps[0].Text, "Create the job") {
		t.Errorf("a new job with no hooks: steps %+v, err %v; want create + health", steps, err)
	}

	d.JobSpec = "not json"
	if _, err := planSteps(d); err == nil || !strings.Contains(err.Error(), "d1") {
		t.Errorf("an unreadable spec: err = %v, want one that names the deployment", err)
	}
}

// -- Overview ------------------------------------------------------------------

func TestOverviewEmpty(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	page := ts.get("/")
	mustContain(t, page, "Overview", "Nothing needs your attention.", "No deployment is in progress.",
		"no commit read yet", "waiting for the first cycle")
	mustNotContain(t, page, specMarker)
}

func TestOverviewGitAndCycle(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{status: engine.Status{
		At: testNow.Add(-30 * time.Second), Duration: 250 * time.Millisecond, Managed: 3,
	}}, "")
	ts.git.snap = gitwatch.Snapshot{Commit: "0123456789abcdef", Subject: "fix: the thing", Author: "Iacopo", CommittedAt: testNow.Add(-2 * time.Hour)}
	ts.git.status = gitwatch.Status{CheckedAt: testNow.Add(-3 * time.Minute)}

	page := ts.get("/")
	mustContain(t, page,
		`href="https://git.test/commit/0123456789abcdef"`, "0123456", "fix: the thing", "Iacopo", "2h ago",
		"checked", "3m ago",
		"took 250ms", "3 managed jobs", `<span class="pill state-success">ok</span>`,
		`action="/fetch"`, "Fetch now")
	mustNotContain(t, page, "flash--error", "flash--warn", "degraded", "aborted")
}

func TestOverviewFetchNowNeedsATrigger(t *testing.T) {
	// A dashboard built with no trigger does not offer what it cannot do.
	a := newTestAuth(t)
	h, err := New(Options{Auth: a, Store: &fakeStore{}, Engine: &fakeEngine{}, Git: &fakeGit{}, Log: slogDiscard()})
	if err != nil {
		t.Fatal(err)
	}
	rec := doWith(h, "GET", "/", mintSession(t, a, "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	mustNotContain(t, rec.Body.String(), "Fetch now", `action="/fetch"`)
}

func TestOverviewGitFailure(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	ts.git.snap = gitwatch.Snapshot{Commit: "0123456789abcdef"}
	ts.git.status = gitwatch.Status{CheckedAt: testNow.Add(-3 * time.Hour), Error: "authentication required", ErrorAt: testNow.Add(-2 * time.Minute)}

	page := ts.get("/")
	mustContain(t, page, "The last poll of the repository failed", "2m ago", "working from the last commit it read", "authentication required", "3h ago")
}

func TestOverviewCycleProblems(t *testing.T) {
	t.Run("aborted", func(t *testing.T) {
		ts := newTestServer(t, &fakeStore{}, &fakeEngine{status: engine.Status{At: testNow, Error: "disk on fire"}}, "")
		mustContain(t, ts.get("/"), "aborted", "The last cycle stopped: disk on fire")
	})
	t.Run("degraded", func(t *testing.T) {
		ts := newTestServer(t, &fakeStore{}, &fakeEngine{status: engine.Status{At: testNow, Managed: 5, Skipped: 2, Unparsed: 1}}, "")
		page := ts.get("/")
		mustContain(t, page, "degraded", "2 jobs were skipped", "Nomad failed on them", "1 file in the repository could not be parsed", "5 managed jobs")
		mustNotContain(t, page, "aborted")
	})
	t.Run("one of each is singular", func(t *testing.T) {
		ts := newTestServer(t, &fakeStore{}, &fakeEngine{status: engine.Status{At: testNow, Managed: 1, Skipped: 1, Unparsed: 1}}, "")
		mustContain(t, ts.get("/"), "1 managed job", "1 job was skipped", "1 file in the repository could not be parsed by Nomad and is ignored")
	})
}

func TestOverviewNeedsAttention(t *testing.T) {
	pending := dep("d1", "web", store.StatePendingApproval, time.Hour)
	applying := dep("d2", "db", store.StateApplying, 5*time.Minute)

	failedRecent := dep("f1", "api", store.StateFailed, 2*time.Hour)
	failedRecent.Error = "pre-hook failed"
	failedRetried := dep("f2", "old1", store.StateFailed, 2*time.Hour)
	failedRetried.RetriedAt = testNow.Add(-time.Hour)
	failedOld := dep("f3", "old2", store.StateFailed, 8*24*time.Hour)
	blocker := dep("blk", "web2", store.StateFailed, 3*time.Hour)
	completed := dep("c1", "fine", store.StateCompleted, time.Hour)

	st := &fakeStore{
		active: []*store.Deployment{pending, applying},
		latest: []*store.Deployment{failedRecent, failedRetried, failedOld, blocker, completed},
	}
	en := &fakeEngine{observations: []engine.Observation{
		{JobID: "web2", Namespace: "default", Policy: meta.PolicyApproval, Drift: true, BlockedBy: "blk", BlockedReason: "failed on the same live job; push a new commit or retry it"},
		{JobID: "bad", Namespace: "default", Issues: []meta.Issue{
			{Severity: meta.SeverityWarn, Key: "nops_pre_hook_timeout", Message: "a warning"},
			{Severity: meta.SeverityError, Key: "nops_policy", Message: "unknown value"},
		}},
		{JobID: "batch", Namespace: "default", Policy: meta.PolicyNone, Drift: true}, // drift under "none" is its policy, not news
	}}
	ts := newTestServer(t, st, en, "")

	page := ts.get("/")
	mustContain(t, page,
		// approval: what it is, the subject, where to go
		"Needs approval", `href="/deployments/d1"`, "feat(web): scale up", ">Review<",
		// blocked: the reason and the retry, which comes back to the Overview
		"Blocked", "failed on the same live job; push a new commit or retry it", `action="/jobs/default/web2/retry"`, `name="back" value="overview"`,
		// a failure nobody dealt with
		"pre-hook failed", `href="/deployments/f1"`,
		// invalid meta names the error, not the warning
		"nops_policy: unknown value",
		// in progress
		"In progress", "Applying", "default/db")
	mustNotContain(t, page,
		"old1", "old2", // retried, and too old
		"a warning", "default/batch", "default/fine",
		specMarker)
	if n := strings.Count(page, `>Failed<`); n != 1 {
		t.Errorf("%d Failed lines, want 1: the failure that blocks web2 is listed once, as Blocked", n)
	}
	if n := strings.Count(page, "Retry</button>"); n != 1 {
		t.Errorf("%d Retry buttons, want 1", n)
	}

	// Most urgent first: approvals, blocked, failures, invalid meta.
	order := []string{"Needs approval", ">Blocked<", ">Failed<", ">Invalid meta<"}
	last := -1
	for _, want := range order {
		i := strings.Index(page, want)
		if i < 0 || i < last {
			t.Fatalf("%q is at %d, after %d: the list is not ordered %v", want, i, last, order)
		}
		last = i
	}
}

func TestOverviewErrors(t *testing.T) {
	for name, st := range map[string]*fakeStore{
		"active": {activeErr: fmt.Errorf("database is locked")},
		"latest": {latestErr: fmt.Errorf("database is locked")},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, st, &fakeEngine{}, "")
			rec := ts.do("GET", "/", nil, mintSession(t, ts.auth, "alice"))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status %d, want 500", rec.Code)
			}
			mustNotContain(t, rec.Body.String(), "database is locked")
			mustContain(t, ts.logs.String(), "level=ERROR", "database is locked")
		})
	}
}

// -- Jobs ----------------------------------------------------------------------

func TestJobSync(t *testing.T) {
	errIssue := []meta.Issue{{Severity: meta.SeverityError, Key: "k"}}
	warnIssue := []meta.Issue{{Severity: meta.SeverityWarn, Key: "k"}}
	st := func(s store.State) *store.Deployment { return &store.Deployment{State: s} }

	for _, c := range []struct {
		name   string
		obs    engine.Observation
		latest *store.Deployment
		want   syncKey
	}{
		{"nothing to say", engine.Observation{}, nil, syncInSync},
		{"in sync after a deployment", engine.Observation{}, st(store.StateCompleted), syncInSync},
		{"drift and nobody on it", engine.Observation{Drift: true}, nil, syncDrift},
		{"drift after a failure with a new commit not yet seen", engine.Observation{Drift: true}, st(store.StateFailed), syncDrift},
		{"waiting for a decision", engine.Observation{Drift: true}, st(store.StatePendingApproval), syncPending},
		{"detected", engine.Observation{Drift: true}, st(store.StateDetected), syncDeploying},
		{"pre-hook", engine.Observation{Drift: true}, st(store.StatePreHook), syncDeploying},
		{"applying", engine.Observation{Drift: true}, st(store.StateApplying), syncDeploying},
		{"post-hook", engine.Observation{Drift: true}, st(store.StatePostHook), syncDeploying},
		{"blocked", engine.Observation{Drift: true, BlockedBy: "x"}, st(store.StateFailed), syncBlocked},
		{"a warning is not invalid", engine.Observation{Issues: warnIssue}, nil, syncInSync},
		{"an error is invalid", engine.Observation{Issues: errIssue}, nil, syncInvalid},
		{"invalid beats blocked", engine.Observation{Issues: errIssue, BlockedBy: "x"}, nil, syncInvalid},
		{"blocked beats pending", engine.Observation{BlockedBy: "x"}, st(store.StatePendingApproval), syncBlocked},
		{"pending beats drift", engine.Observation{Drift: true}, st(store.StatePendingApproval), syncPending},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := jobSync(c.obs, c.latest); got != c.want {
				t.Errorf("jobSync = %q, want %q", got, c.want)
			}
		})
	}

	for _, k := range syncOrder {
		if !k.valid() || k.label() == "" || !strings.HasPrefix(k.class(), "state-") {
			t.Errorf("sync state %q has no label or class", k)
		}
	}
	if syncKey("nope").valid() || syncKey("").valid() {
		t.Error("an unknown sync state is valid")
	}
}

func jobsFixture() (*fakeStore, *fakeEngine) {
	pending := dep("d1", "web", store.StatePendingApproval, time.Hour)
	done := dep("d2", "db", store.StateCompleted, 26*time.Hour)
	st := &fakeStore{latest: []*store.Deployment{pending, done}}
	en := &fakeEngine{observations: []engine.Observation{
		{JobID: "batch", Namespace: "default", Policy: meta.PolicyNone, Drift: true, FilePath: "batch.nomad.hcl", ObservedAt: testNow.Add(-time.Minute)},
		{JobID: "bad", Namespace: "default", FilePath: "bad.nomad.hcl", Issues: []meta.Issue{{Severity: meta.SeverityError, Key: "nops_policy", Message: "m"}}},
		{JobID: "db", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "db.nomad.hcl"},
		{JobID: "web", Namespace: "default", Policy: meta.PolicyApproval, Drift: true, FilePath: "apps/web.nomad.hcl",
			Issues: []meta.Issue{{Severity: meta.SeverityWarn, Key: "k", Message: "m"}}},
		{JobID: "stuck", Namespace: "default", Policy: meta.PolicyAuto, Drift: true, BlockedBy: "x", BlockedReason: "failed on the same live job; push a new commit or retry it"},
	}}
	return st, en
}

func TestJobsPage(t *testing.T) {
	ts := newTestServer(t, func() *fakeStore { st, _ := jobsFixture(); return st }(), func() *fakeEngine { _, en := jobsFixture(); return en }(), "")
	page := ts.get("/jobs")

	mustContain(t, page,
		"Jobs", `href="/jobs/default/web"`, `href="/jobs/default/batch"`,
		// every sync state that is present
		"Awaiting approval", "In sync", "Drift", "Invalid meta", "Blocked",
		// policies, files, the last deployment
		"apps/web.nomad.hcl", "batch.nomad.hcl", ">approval<", ">none<", `href="/deployments/d1"`, "1h ago", "1d ago", "never",
		// what is under the state
		"failed on the same live job; push a new commit or retry it", "1 meta warning",
		// the filter, with counts
		`href="?"`, `href="?state=drift"`, `href="?state=blocked"`)
	mustNotContain(t, page, specMarker, `href="?state=deploying"`) // no deploying job: no chip
	if n := strings.Count(page, `class="row__title"`); n != 5 {
		t.Errorf("%d rows, want 5", n)
	}
}

func TestJobsPageFilter(t *testing.T) {
	newServer := func() *testServer {
		st, en := jobsFixture()
		return newTestServer(t, st, en, "")
	}

	page := newServer().get("/jobs?state=drift")
	mustContain(t, page, `href="/jobs/default/batch"`, `is-active" href="?state=drift"`)
	mustNotContain(t, page, `href="/jobs/default/db"`, `href="/jobs/default/web"`)
	if n := strings.Count(page, `class="row__title"`); n != 1 {
		t.Errorf("%d rows for the drift filter, want 1", n)
	}

	// Not one of ours: shows everything, and marks All.
	page = newServer().get("/jobs?state=who-knows")
	if n := strings.Count(page, `class="row__title"`); n != 5 {
		t.Errorf("%d rows for an unknown filter, want all 5", n)
	}
	mustContain(t, page, `is-active" href="?"`)

	// A known state with no job: an empty page that still says which filter it is.
	page = newServer().get("/jobs?state=deploying")
	mustContain(t, page, "No job matches this filter.", `is-active" href="?state=deploying"`)
}

func TestJobsPageEmptyAndErrors(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	mustContain(t, ts.get("/jobs"), "No managed job observed yet.")

	ts = newTestServer(t, &fakeStore{latestErr: fmt.Errorf("database is locked")}, &fakeEngine{}, "")
	rec := ts.do("GET", "/jobs", nil, mintSession(t, ts.auth, "alice"))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database is locked") {
		t.Errorf("status %d, body leaks the error: %v", rec.Code, strings.Contains(rec.Body.String(), "database is locked"))
	}
}

// -- Job -----------------------------------------------------------------------

func TestJobPage(t *testing.T) {
	pending := dep("d1", "web", store.StatePendingApproval, time.Hour)
	older := dep("d0", "web", store.StateCompleted, 30*time.Hour)
	older.CommitSubject = ""
	st := &fakeStore{byJob: []*store.Deployment{pending, older}}
	en := &fakeEngine{observations: []engine.Observation{{
		JobID: "web", Namespace: "default", Policy: meta.PolicyApproval, Drift: true, FilePath: "apps/web.nomad.hcl",
		PlanDiff: diffJSON(t, sampleDiff()), ObservedAt: testNow.Add(-2 * time.Minute),
		PreHook: &meta.Hook{JobID: "web-migrate", Timeout: 10 * time.Minute},
		Issues:  []meta.Issue{{Severity: meta.SeverityWarn, Key: "nops_post_hook_timeout", Message: "unusual value"}},
	}}}
	ts := newTestServer(t, st, en, "")

	page := ts.get("/jobs/default/web")
	mustContain(t, page,
		"default/web", "Awaiting approval", "apps/web.nomad.hcl", "2m ago",
		// the hooks it declares
		"web-migrate", "10m", ">none<",
		// the drift, summarized and in full
		"3 field changes", "+1 added", "~1 edited", "removed", "Priority", "Env[FOO]", "&lt;redacted&gt;",
		// meta issues
		"Meta issues", "nops_post_hook_timeout", "unusual value",
		// its deployments, newest first
		`href="/deployments/d1"`, `href="/deployments/d0"`, "feat(web): scale up", "no commit message")
	mustNotContain(t, page, specMarker, "Blocked.", "Retry")
	if strings.Index(page, `href="/deployments/d1"`) > strings.Index(page, `href="/deployments/d0"`) {
		t.Error("deployments are not listed newest first")
	}
}

func TestJobPageInSync(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{{JobID: "db", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "db.nomad.hcl"}}}
	ts := newTestServer(t, &fakeStore{}, en, "")
	page := ts.get("/jobs/default/db")
	mustContain(t, page, "In sync", "In sync with git.", "This job never had a deployment.")
	mustNotContain(t, page, "Meta issues", "field change")
}

func TestJobPageBlockedOffersARetryThatComesBack(t *testing.T) {
	failed := dep("d1", "web", store.StateFailed, time.Hour)
	st := &fakeStore{byJob: []*store.Deployment{failed}}
	en := &fakeEngine{observations: []engine.Observation{{
		JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, Drift: true,
		BlockedBy: "d1", BlockedReason: "failed on the same live job; push a new commit or retry it",
	}}}
	ts := newTestServer(t, st, en, "")

	page := ts.get("/jobs/default/web")
	mustContain(t, page, "Blocked.", "failed on the same live job; push a new commit or retry it", `href="/deployments/d1"`,
		`action="/jobs/default/web/retry"`, `name="back" value="job"`, "Retry</button>")
}

func TestJobPageOnlyDeploymentsRemain(t *testing.T) {
	st := &fakeStore{byJob: []*store.Deployment{dep("d1", "gone", store.StateCompleted, time.Hour)}}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	page := ts.get("/jobs/default/gone")
	mustContain(t, page, "default/gone", "Not in the repository any more", `href="/deployments/d1"`)
	mustNotContain(t, page, "Drift", "Details", "Retry")
}

func TestJobPageErrors(t *testing.T) {
	t.Run("unknown job", func(t *testing.T) {
		ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
		rec := ts.do("GET", "/jobs/default/nope", nil, mintSession(t, ts.auth, "alice"))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "This job does not exist.") {
			t.Errorf("status %d, body %s", rec.Code, rec.Body)
		}
	})
	t.Run("store error", func(t *testing.T) {
		ts := newTestServer(t, &fakeStore{byJobErr: fmt.Errorf("database is locked")}, &fakeEngine{}, "")
		rec := ts.do("GET", "/jobs/default/web", nil, mintSession(t, ts.auth, "alice"))
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database is locked") {
			t.Errorf("status %d, leaks the error: %v", rec.Code, strings.Contains(rec.Body.String(), "database is locked"))
		}
		mustContain(t, ts.logs.String(), "level=ERROR", "database is locked")
	})
	t.Run("a diff that does not parse", func(t *testing.T) {
		en := &fakeEngine{observations: []engine.Observation{{JobID: "web", Namespace: "default", Drift: true, PlanDiff: "not json"}}}
		ts := newTestServer(t, &fakeStore{}, en, "")
		rec := ts.do("GET", "/jobs/default/web", nil, mintSession(t, ts.auth, "alice"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500", rec.Code)
		}
		mustContain(t, ts.logs.String(), "level=ERROR")
	})
}

// -- Activity ------------------------------------------------------------------

func TestActivityPage(t *testing.T) {
	active := dep("a1", "web", store.StatePendingApproval, 30*time.Minute)
	completed := dep("c1", "db", store.StateCompleted, 25*time.Hour)
	completed.DecidedBy = "alice"
	failed := dep("f1", "api", store.StateFailed, 4*24*time.Hour)
	failed.Error = "apply did not become healthy"
	st := &fakeStore{active: []*store.Deployment{active}, history: []*store.Deployment{completed, failed}}
	ts := newTestServer(t, st, &fakeEngine{}, "")

	page := ts.get("/history")
	mustContain(t, page, "Activity", "Today", "Yesterday", "Mon, 29 Dec 2025",
		`href="/deployments/a1"`, `href="/deployments/c1"`, `href="/deployments/f1"`,
		"apply did not become healthy", "alice", "feat(web): scale up")
	mustNotContain(t, page, specMarker, "Only the most recent")
	if !(strings.Index(page, "Today") < strings.Index(page, "Yesterday") && strings.Index(page, "Yesterday") < strings.Index(page, "Mon, 29 Dec 2025")) {
		t.Error("days are not newest first")
	}
}

func TestActivityFilter(t *testing.T) {
	active := dep("a1", "web", store.StatePendingApproval, 30*time.Minute)
	completed := dep("c1", "db", store.StateCompleted, time.Hour)
	failed := dep("f1", "api", store.StateFailed, 2*time.Hour)
	st := &fakeStore{active: []*store.Deployment{active}, history: []*store.Deployment{completed, failed}}
	ts := newTestServer(t, st, &fakeEngine{}, "")

	page := ts.get("/history?state=failed")
	mustContain(t, page, `href="/deployments/f1"`, `is-active" href="?state=failed"`)
	mustNotContain(t, page, `href="/deployments/a1"`, `href="/deployments/c1"`)

	page = ts.get("/history?state=active")
	mustContain(t, page, `href="/deployments/a1"`)
	mustNotContain(t, page, `href="/deployments/c1"`)

	page = ts.get("/history?state=nonsense")
	mustContain(t, page, `href="/deployments/a1"`, `href="/deployments/c1"`, `href="/deployments/f1"`, `is-active" href="?"`)

	page = ts.get("/history?state=rejected")
	mustContain(t, page, "No deployment matches this filter.")
}

func TestActivityEmptyLimitedAndErrors(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	mustContain(t, ts.get("/history"), "No deployment yet.")

	// The history is capped: when it is full, the page says there is more.
	many := make([]*store.Deployment, historyLimit)
	for i := range many {
		many[i] = dep(fmt.Sprintf("h%03d", i), "web", store.StateCompleted, time.Duration(i+1)*time.Minute)
	}
	ts = newTestServer(t, &fakeStore{history: many}, &fakeEngine{}, "")
	mustContain(t, ts.get("/history"), "Only the most recent finished deployments are listed.")

	for name, st := range map[string]*fakeStore{
		"active":  {activeErr: fmt.Errorf("database is locked")},
		"history": {historyErr: fmt.Errorf("database is locked")},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, st, &fakeEngine{}, "")
			rec := ts.do("GET", "/history", nil, mintSession(t, ts.auth, "alice"))
			if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database is locked") {
				t.Errorf("status %d, leaks the error: %v", rec.Code, strings.Contains(rec.Body.String(), "database is locked"))
			}
		})
	}
}

func TestDriftMovedToJobs(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	rec := ts.do("GET", "/drift", nil, mintSession(t, ts.auth, "alice"))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/jobs" {
		t.Errorf("status %d, Location %q, want 301 to /jobs", rec.Code, rec.Header().Get("Location"))
	}
}

// -- Deployment ----------------------------------------------------------------

func TestDeploymentPage(t *testing.T) {
	d := sampleDeployment()
	d.PlanDiff = diffJSON(t, sampleDiff())
	st := &fakeStore{deployment: d,
		events: []store.Event{
			{From: "", To: store.StateDetected, Actor: "nops", Message: "detected at commit abc123def456", Time: testNow.Add(-time.Hour)},
			{From: store.StateDetected, To: store.StatePendingApproval, Actor: "nops", Message: "drift detected", Time: testNow.Add(-59 * time.Minute)},
		},
		hookRuns: map[string]*store.HookRun{"d1/pre": {Phase: "pre", HookJobID: "web-migrate", State: store.HookSucceeded,
			StartedAt: testNow.Add(-10 * time.Minute), FinishedAt: testNow.Add(-9 * time.Minute)}},
	}
	ts := newTestServer(t, st, &fakeEngine{}, "")

	page := ts.get("/deployments/d1")
	mustContain(t, page,
		// the header: which job, which commit, by whom, when
		"default/web", "Pending approval", `href="/jobs/default/web"`,
		`href="https://git.test/commit/abc123def456"`, "abc123d", "feat(web): scale up", "by Iacopo", "1h ago",
		// the decision, with what it will do
		"Review", "Run the pre-hook", "web-migrate", "timeout 10m", "Wait for the new version to become healthy",
		"Run the post-hook", "web-smoke", "only if it has not changed since (index 42)",
		`action="/deployments/d1/approve"`, `action="/deployments/d1/reject"`, "Approve", "Reject",
		// the diff, summarized and in full
		"3 field changes", "Priority", "Env[FOO]",
		// hooks and timeline
		"pre-hook", "Succeeded", "drift detected", "Pending approval",
		// the details
		"Live index", "spec-hash-1")
	mustNotContain(t, page, specMarker, "hx-swap-oob")
	// Invariant 3: the approval is for this exact spec, in full, whatever is
	// shown short.
	mustContain(t, page, `<input type="hidden" name="spec_hash" value="spec-hash-1">`)
}

func TestDeploymentPageShortensLongHashes(t *testing.T) {
	d := sampleDeployment()
	d.SpecHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	d.EvalID = "abcdef12-3456-7890-abcd-ef1234567890"
	d.State = store.StateApplying
	ts := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "")

	page := ts.get("/deployments/d1")
	mustContain(t, page,
		`title="`+d.SpecHash+`">0123456789ab<`, // short in the text, whole on hover
		`title="`+d.EvalID+`">abcdef12<`)
	mustNotContain(t, page, ">"+d.SpecHash+"<", ">"+d.EvalID+"<")
}

func TestDeploymentPageStepsForANewJobWithoutHooks(t *testing.T) {
	d := sampleDeployment()
	d.CASIndex, d.JobSpec = 0, `{"ID":"web"}`
	ts := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "")

	page := ts.get("/deployments/d1")
	mustContain(t, page, "Create the job in Nomad (it is not registered yet).", "Wait for the new version to become healthy")
	mustNotContain(t, page, "pre-hook", "post-hook")
}

func TestDeploymentPageWithoutCommitInfo(t *testing.T) {
	// A deployment created before the commit subject and author were recorded.
	d := sampleDeployment()
	d.CommitSubject, d.CommitAuthor = "", ""
	ts := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "")
	page := ts.get("/deployments/d1")
	mustContain(t, page, "abc123d")
	mustNotContain(t, page, "by ")
}

func TestDeploymentPageNotPending(t *testing.T) {
	d := sampleDeployment()
	d.State, d.Error = store.StateFailed, "pre-hook web-migrate failed"
	d.DecidedBy, d.DecidedAt = "alice", testNow.Add(-50*time.Minute)
	d.RetriedBy, d.RetriedAt = "bob", testNow.Add(-5*time.Minute)
	ts := newTestServer(t, &fakeStore{deployment: d, events: []store.Event{
		{From: store.StateFailed, To: store.StateFailed, Actor: "bob", Message: "retry requested", Time: testNow.Add(-5 * time.Minute)},
	}}, &fakeEngine{}, "")

	page := ts.get("/deployments/d1")
	mustContain(t, page, "Failed", "pre-hook web-migrate failed", "Decided", "alice", "50m ago", "Retried", "bob", "5m ago", "retry requested")
	mustNotContain(t, page, "Approve", "Reject", "Run the pre-hook", specMarker)
}

// The diff is open to be reviewed and folded once there is nothing to decide,
// so a finished deployment's hooks and timeline are not pushed off the page.
func TestDeploymentPageFoldsTheDiffOnceDecided(t *testing.T) {
	pending := newTestServer(t, &fakeStore{deployment: sampleDeployment()}, &fakeEngine{}, "").get("/deployments/d1")
	mustContain(t, pending, `<details class="box fold" open>`)

	for _, st := range []store.State{store.StateApplying, store.StateFailed, store.StateCompleted} {
		d := sampleDeployment()
		d.State = st
		page := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "").get("/deployments/d1")
		mustContain(t, page, `<details class="box fold">`)
		mustNotContain(t, page, `<details class="box fold" open>`)
	}
}

// A failed deployment that holds its job back offers the retry where its
// failure is read, and only while it is the one holding it.
func TestDeploymentPageThatBlocksItsJobOffersARetry(t *testing.T) {
	d := sampleDeployment()
	d.State, d.Error = store.StateFailed, "pre-hook failed"
	en := &fakeEngine{observations: []engine.Observation{
		{JobID: "web", Namespace: "default", BlockedBy: "d1", BlockedReason: "failed on the same live job; push a new commit or retry it"},
		{JobID: "other", Namespace: "default", BlockedBy: "someone-else"},
	}}
	page := newTestServer(t, &fakeStore{deployment: d}, en, "").get("/deployments/d1")
	mustContain(t, page, "This deployment blocks its job.", "failed on the same live job; push a new commit or retry it",
		`action="/jobs/default/web/retry"`, `name="back" value="job"`, "Retry</button>")

	// Not the blocker (a newer deployment replaced it, or it was retried): no button.
	page = newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "").get("/deployments/d1")
	mustNotContain(t, page, "blocks its job", "Retry")
}

func TestDeploymentPageWithNothingToShow(t *testing.T) {
	d := sampleDeployment()
	d.State = store.StateCompleted
	ts := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "")
	mustContain(t, ts.get("/deployments/d1"), "No changes.", "No hook ran.", "No events yet.")
}

func TestDeploymentPageStaleApprovalNotice(t *testing.T) {
	st := &fakeStore{deployment: sampleDeployment()}
	ts := newTestServer(t, st, &fakeEngine{approveErr: engine.ErrStaleApproval}, "")
	rec := ts.do("POST", "/deployments/d1/approve", formBody(map[string][]string{"spec_hash": {"old"}}), mintSession(t, ts.auth, "alice"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
	// The page that comes back is the review page, with the new spec to decide on.
	mustContain(t, rec.Body.String(), "The spec changed since this page loaded", `value="spec-hash-1"`, "Approve")
}

func TestDeploymentPageUnreadableSpecIsAServerError(t *testing.T) {
	d := sampleDeployment()
	d.JobSpec = "not json"
	ts := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "")
	rec := ts.do("GET", "/deployments/d1", nil, mintSession(t, ts.auth, "alice"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	mustContain(t, ts.logs.String(), "level=ERROR", "d1")
	mustNotContain(t, rec.Body.String(), "not json")
}

func TestDeploymentPageErrors(t *testing.T) {
	d := sampleDeployment()
	for name, st := range map[string]*fakeStore{
		"bad diff": {deployment: func() *store.Deployment { x := *d; x.PlanDiff = "not json"; return &x }()},
		"events":   {deployment: d, eventsErr: fmt.Errorf("database is locked")},
		"hooks":    {deployment: d, hookErr: fmt.Errorf("database is locked")},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, st, &fakeEngine{}, "")
			rec := ts.do("GET", "/deployments/d1", nil, mintSession(t, ts.auth, "alice"))
			if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database is locked") {
				t.Errorf("status %d, leaks the error: %v", rec.Code, strings.Contains(rec.Body.String(), "database is locked"))
			}
			mustContain(t, ts.logs.String(), "level=ERROR")
		})
	}
}

func TestDeploymentPageNotFoundAndStoreError(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	rec := ts.do("GET", "/deployments/missing", nil, mintSession(t, ts.auth, "alice"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	mustContain(t, rec.Body.String(), "This deployment does not exist.")

	ts = newTestServer(t, &fakeStore{getErr: fmt.Errorf("database is locked")}, &fakeEngine{}, "")
	rec = ts.do("GET", "/deployments/d1", nil, mintSession(t, ts.auth, "alice"))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database is locked") {
		t.Errorf("status %d, leaks the error: %v", rec.Code, strings.Contains(rec.Body.String(), "database is locked"))
	}
	mustContain(t, ts.logs.String(), "database is locked") // logged, never shown
}

func TestDeploymentStatusFragment(t *testing.T) {
	ts := newTestServer(t, &fakeStore{deployment: sampleDeployment()}, &fakeEngine{}, "")
	frag := ts.get("/deployments/d1/status")

	// The head replaces itself and keeps polling; the activity and the details
	// swap out of band; there is no page around them and no diff.
	mustContain(t, frag,
		`id="deployment-status"`, `hx-get="/deployments/d1/status"`, `hx-trigger="every 3s"`,
		`id="deployment-activity" hx-swap-oob="true"`, `id="deployment-details" hx-swap-oob="true"`,
		"Approve")
	mustNotContain(t, frag, "<html", "Plan diff", specMarker)
}

func TestDeploymentStatusFragmentStopsPollingWhenTerminal(t *testing.T) {
	for _, st := range []store.State{store.StateCompleted, store.StateFailed, store.StateRejected, store.StateSuperseded} {
		d := sampleDeployment()
		d.State = st
		ts := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "")
		if frag := ts.get("/deployments/d1/status"); strings.Contains(frag, "hx-trigger") {
			t.Errorf("%s: the status fragment of a finished deployment must not keep polling", st)
		}
	}
}

// -- small test helpers --------------------------------------------------------

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// doWith serves one request through h with the given cookies.
func doWith(h http.Handler, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
