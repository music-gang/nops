package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

func allowAll(string) bool { return true }

// stateOf is what a plan must leave as it found: every job's latest
// deployment with its state, the registers, the notifications and the pages.
type stateOf struct {
	latest       map[string]store.State
	registers    int
	notified     int
	observations []Observation
	orphans      []Orphan
}

func (h *harness) state() stateOf {
	h.t.Helper()
	list, err := h.store.LatestPerJob(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	s := stateOf{latest: map[string]store.State{}, observations: h.engine.Observations(), orphans: h.engine.Orphans()}
	for _, d := range list {
		s.latest[d.Namespace+"/"+d.JobID+"/"+d.ID] = d.State
	}
	h.nomad.mu.Lock()
	s.registers = len(h.nomad.registerCalls)
	h.nomad.mu.Unlock()
	h.notifier.mu.Lock()
	s.notified = len(h.notifier.calls)
	h.notifier.mu.Unlock()
	return s
}

// plan runs Plan and fails the test if it changed anything.
func (h *harness) plan(snap gitwatch.Snapshot) Plan {
	h.t.Helper()
	return h.planAllowing(snap, func(string) bool { return true })
}

// planAllowing is plan for a caller that may plan the namespaces allow says.
func (h *harness) planAllowing(snap gitwatch.Snapshot, allow func(string) bool) Plan {
	h.t.Helper()
	before := h.state()
	p, err := h.engine.Plan(context.Background(), snap, allow)
	if err != nil {
		h.t.Fatalf("Plan: %v", err)
	}
	if after := h.state(); !reflect.DeepEqual(before, after) {
		h.t.Fatalf("Plan changed something:\nbefore %+v\nafter  %+v", before, after)
	}
	return p
}

// A plan is what the next cycle does: for each case, the jobs it plans are
// the observations and the latest deployments Detect then leaves.
func TestPlanIsWhatTheNextCycleDoes(t *testing.T) {
	for name, setup := range map[string]func(h *harness){
		"auto with drift": func(h *harness) {
			h.nomad.setFile("web-v2", managed("web", "auto", nil))
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		},
		"approval with drift": func(h *harness) {
			h.nomad.setFile("web-v2", managed("web", "approval", nil))
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		},
		"a newer spec supersedes the one waiting": func(h *harness) {
			h.nomad.setFile("web-v1", managed("web", "approval", nil))
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
			h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
			h.detect()
			h.nomad.setFile("web-v2", managed("web", "approval", map[string]string{"x": "2"}))
		},
		"in sync": func(h *harness) {
			h.nomad.setFile("web-v2", managed("web", "auto", nil))
		},
		"paused": func(h *harness) {
			h.nomad.setFile("web-v1", managed("web", "auto", nil))
			h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
			h.detect()
			if err := h.engine.Pause(context.Background(), testNamespace, "web", "alice", "db incident"); err != nil {
				t.Fatal(err)
			}
			h.nomad.setFile("web-v2", managed("web", "auto", nil))
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		},
		"a missing hook": func(h *harness) {
			h.nomad.setFile("web-v2", managed("web", "auto", map[string]string{"nops_pre_hook": "migrate"}))
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		},
		"policy none": func(h *harness) {
			h.nomad.setFile("web-v2", managed("web", "none", nil))
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			setup(h)
			next := gitwatch.Snapshot{Repository: "default", Commit: "c2", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web-v2"}}}
			if len(h.snap.Snapshots()) == 0 {
				h.snap.set("c0")
			}

			p := h.plan(next)

			h.snap.setRepo(next)
			h.detect()
			obs := h.engine.Observations()
			if len(p.Jobs) != len(obs) {
				t.Fatalf("plan has %d jobs, the cycle %d: %+v", len(p.Jobs), len(obs), p.Jobs)
			}
			for i, o := range obs {
				got, want := p.Jobs[i].Observation, o
				got.ObservedAt = want.ObservedAt
				if !reflect.DeepEqual(got, want) {
					t.Errorf("planned %+v\ncycle   %+v", got, want)
				}
				var latest store.State
				if d, err := h.store.LatestDeployment(context.Background(), o.Namespace, o.JobID); err == nil {
					latest = d.State
				}
				if p.Jobs[i].Latest != latest {
					t.Errorf("planned latest %q, the cycle left %q", p.Jobs[i].Latest, latest)
				}
			}
		})
	}
}

// Only the jobs of the repository planned are planned in Nomad; the other
// repositories still count, for their hooks.
func TestPlanPlansOnlyItsRepository(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("api", managed("api", "auto", nil))
	h.nomad.setFile("migrate", hookJob("migrate"))
	h.snap.setRepo(gitwatch.Snapshot{Repository: "infra", Commit: "i1", Files: []gitwatch.File{
		{Path: "api.nomad.hcl", Content: "api"}, {Path: "migrate.nomad.hcl", Content: "migrate"},
	}})
	h.snap.set("c1")
	h.nomad.setFile("web", managed("web", "auto", map[string]string{"nops_pre_hook": "migrate"}))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})

	p := h.plan(gitwatch.Snapshot{Repository: "default", Commit: "c2", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web"}}})

	if len(p.Jobs) != 1 || p.Jobs[0].JobID != "web" {
		t.Fatalf("jobs = %+v, want web only", p.Jobs)
	}
	if p.Jobs[0].HookProblem != "" || p.Jobs[0].Latest != store.StateDetected {
		t.Errorf("web = %+v, want its hook found in infra and a deployment to start", p.Jobs[0])
	}
	h.nomad.mu.Lock()
	_, planned := h.nomad.lastTG["api"]
	h.nomad.mu.Unlock()
	if planned {
		t.Error("the plan of default planned api, a job of infra")
	}
}

func TestPlanReportsTheFilesThatMakeNoJob(t *testing.T) {
	h := newHarness(t)
	h.engine.repoJobs = map[string][]string{"default": {"default/*"}}
	h.nomad.setParseErr("broken", errors.New("bad HCL"))
	h.nomad.setFile("dup-a", managed("dup", "auto", nil))
	h.nomad.setFile("dup-b", managed("dup", "auto", nil))
	other := job("other", map[string]string{"nops_managed": "true"})
	ns := "prod"
	other.Namespace = &ns
	h.nomad.setFile("other", other)
	h.nomad.setFile("hook", hookJob("bad"))
	h.nomad.parseByContent["hook"].Meta["nops_timeout"] = "soon"
	h.snap.set("c1")
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{{Path: "dup.nomad.hcl", Content: "dup-b"}}})

	p := h.plan(gitwatch.Snapshot{Repository: "default", Commit: "c2", Files: []gitwatch.File{
		{Path: "broken.nomad.hcl", Content: "broken"},
		{Path: "dup.nomad.hcl", Content: "dup-a"},
		{Path: "other.nomad.hcl", Content: "other"},
		{Path: "hook.nomad.hcl", Content: "hook"},
	}})

	got := map[string]string{}
	for _, f := range p.Errors {
		if f.Repository != "default" {
			t.Errorf("error of another repository: %+v", f)
		}
		got[f.Path] = f.Error
	}
	for path, want := range map[string]string{
		"broken.nomad.hcl": "bad HCL",
		"dup.nomad.hcl":    "apps:dup.nomad.hcl",
		"other.nomad.hcl":  "may not declare",
		"hook.nomad.hcl":   "nops_timeout",
	} {
		if !strings.Contains(got[path], want) {
			t.Errorf("error of %s = %q, want it to say %q", path, got[path], want)
		}
	}
	if len(p.Jobs) != 0 {
		t.Errorf("jobs = %+v, want none", p.Jobs)
	}
	if !p.RemovedUnchecked {
		t.Error("a file does not parse: the removed jobs should be unchecked")
	}
}

// A job the change takes out of the repository: its waiting deployment is
// superseded, and the job, deployed and running, would be an orphan.
func TestPlanReportsTheJobsItRemoves(t *testing.T) {
	h := newHarness(t)
	h.deployed("web", store.PolicyApproval)
	h.runningInNomad("web", "running")
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setFile("api", managed("api", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"}, gitwatch.File{Path: "api.nomad.hcl", Content: "api"})
	h.detect()
	waiting := h.active("web")

	p := h.plan(gitwatch.Snapshot{Repository: "default", Commit: "c2", Files: []gitwatch.File{{Path: "api.nomad.hcl", Content: "api"}}})

	want := []RemovedJob{{Namespace: testNamespace, JobID: "web", Superseded: waiting.ID, Orphan: true}}
	if !reflect.DeepEqual(p.Removed, want) {
		t.Errorf("removed = %+v, want %+v", p.Removed, want)
	}
	if len(p.Jobs) != 1 || p.Jobs[0].JobID != "api" {
		t.Errorf("jobs = %+v, want api", p.Jobs)
	}
}

func TestPlanOfAnUnknownRepository(t *testing.T) {
	h := newHarness(t)
	h.snap.set("c1")
	if _, err := h.engine.Plan(context.Background(), gitwatch.Snapshot{Repository: "nope"}, allowAll); !errors.Is(err, ErrUnknownRepository) {
		t.Errorf("Plan = %v, want ErrUnknownRepository", err)
	}
}

// Plans run one at a time: one that cannot start waits, until its context ends.
func TestPlansRunOneAtATime(t *testing.T) {
	h := newHarness(t)
	h.snap.set("c1")
	h.engine.planSem <- struct{}{} // a plan in progress
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.engine.Plan(ctx, gitwatch.Snapshot{Repository: "default"}, allowAll); !errors.Is(err, context.Canceled) {
		t.Errorf("Plan while another runs = %v, want it to wait for its context", err)
	}
	<-h.engine.planSem
	if _, err := h.engine.Plan(context.Background(), gitwatch.Snapshot{Repository: "default"}, allowAll); err != nil {
		t.Errorf("Plan once the other ended = %v", err)
	}
}

// A job a cycle cannot look at in Nomad is an error, not a job left out: here
// the orphan check of a job the change removes.
func TestPlanReportsWhatItCouldNotLookAt(t *testing.T) {
	h := newHarness(t)
	h.deployed("web", store.PolicyAuto)
	h.nomad.setFile("web", managed("web", "auto", nil))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web"})
	h.detect()
	h.nomad.jobErr["web"] = errors.New("timeout")

	p := h.plan(gitwatch.Snapshot{Repository: "default", Commit: "c2"})

	if len(p.Errors) != 1 || p.Errors[0].JobID != "web" || !strings.Contains(p.Errors[0].Error, "timeout") {
		t.Errorf("errors = %+v, want the failed orphan check of web", p.Errors)
	}
}

// A file of the repository that does not parse now keeps the cycle from
// reading any job as removed. A change that fixes it lets the next cycle do
// it, for its own jobs and any other: the plan says so.
func TestPlanOfAChangeThatFixesAFileReadsTheRemovals(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	waiting := h.active("web")
	h.nomad.setParseErr("web-broken", errors.New("bad HCL"))
	h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-broken"})
	h.detect()
	h.active("web") // still waiting: a file that does not parse removes nothing

	p := h.plan(gitwatch.Snapshot{Repository: "default", Commit: "c3"})

	want := []RemovedJob{{Namespace: testNamespace, JobID: "web", Superseded: waiting.ID}}
	if !reflect.DeepEqual(p.Removed, want) || p.RemovedUnchecked {
		t.Errorf("removed = %+v (unchecked %v), want %+v", p.Removed, p.RemovedUnchecked, want)
	}
}

// A job of a namespace the caller may not plan is not planned in Nomad, and
// is listed by name.
func TestPlanSkipsTheNamespacesItMayNotPlan(t *testing.T) {
	h := newHarnessIn(t, "ci", "prod")
	web := managed("web", "auto", nil)
	ns := "prod"
	web.Namespace = &ns
	h.nomad.setFile("web", web)
	h.snap.set("c1")

	p := h.planAllowing(gitwatch.Snapshot{Repository: "default", Commit: "c2", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web"}}},
		func(ns string) bool { return ns == "ci" })

	want := []FileError{{Repository: "default", Path: "web.nomad.hcl", Namespace: "prod", JobID: "web"}}
	if len(p.Jobs) != 0 || !reflect.DeepEqual(p.Unplanned, want) {
		t.Errorf("jobs %+v, unplanned %+v: want web unplanned", p.Jobs, p.Unplanned)
	}
	h.nomad.mu.Lock()
	defer h.nomad.mu.Unlock()
	if len(h.nomad.lastTG) != 0 {
		t.Errorf("Nomad planned %v", h.nomad.lastTG)
	}
}

// A file in a namespace Nops does not manage names it in its error, not as a
// namespace of its own: no rule of a token is about it.
func TestPlanOfAnUnmanagedNamespace(t *testing.T) {
	h := newHarness(t)
	j := managed("web", "auto", nil)
	ns := "stagng"
	j.Namespace = &ns
	h.nomad.setFile("web", j)
	h.snap.set("c1")

	p := h.plan(gitwatch.Snapshot{Repository: "default", Commit: "c2", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web"}}})

	if len(p.Errors) != 1 || p.Errors[0].Namespace != "" || !strings.Contains(p.Errors[0].Error, `"stagng"`) {
		t.Errorf("errors = %+v, want one that names stagng in its text", p.Errors)
	}
}

// A change to a hook changes the jobs that run it, in any repository: the
// plan of a change that deletes a hook plans them, and their deployment would
// fail.
func TestPlanOfAHookPlansTheJobsThatRunIt(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("migrate", hookJob("migrate"))
	h.nomad.setFile("api", managed("api", "auto", nil))
	h.nomad.setFile("web", managed("web", "auto", map[string]string{"nops_pre_hook": "migrate"}))
	h.nomad.setFile("db", managed("db", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.setRepo(gitwatch.Snapshot{Repository: "infra", Commit: "i1", Files: []gitwatch.File{
		{Path: "migrate.nomad.hcl", Content: "migrate"}, {Path: "api.nomad.hcl", Content: "api"},
	}})
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{
		{Path: "web.nomad.hcl", Content: "web"}, {Path: "db.nomad.hcl", Content: "db"},
	}})

	p := h.plan(gitwatch.Snapshot{Repository: "infra", Commit: "i2", Files: []gitwatch.File{{Path: "api.nomad.hcl", Content: "api"}}})

	got := map[string]PlannedJob{}
	for _, j := range p.Jobs {
		got[j.JobID] = j
	}
	if _, ok := got["db"]; ok || len(got) != 2 {
		t.Fatalf("jobs = %+v, want api and web, which runs the hook", p.Jobs)
	}
	if w := got["web"]; w.Repository != "apps" || !strings.Contains(w.HookProblem, "migrate") || w.Latest != store.StateFailed {
		t.Errorf("web = %+v, want its deployment to fail on the missing hook", w)
	}
}
