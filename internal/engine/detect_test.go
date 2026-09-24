package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// -- test job builders -------------------------------------------------

func job(id string, meta map[string]string, groups ...*api.TaskGroup) *api.Job {
	return &api.Job{ID: &id, Meta: meta, TaskGroups: groups}
}

func hookJob(id string) *api.Job {
	return job(id, map[string]string{"nops_role": "hook"})
}

func managed(id, policy string, extra map[string]string, groups ...*api.TaskGroup) *api.Job {
	m := map[string]string{"nops_managed": "true", "nops_policy": policy}
	for k, v := range extra {
		m[k] = v
	}
	return job(id, m, groups...)
}

func taskGroup(name string, count int, scaling bool) *api.TaskGroup {
	g := &api.TaskGroup{Name: &name, Count: &count}
	if scaling {
		g.Scaling = &api.ScalingPolicy{}
	}
	return g
}

// -- fake Nomad ----------------------------------------------------------

type planFixture struct {
	diff *api.JobDiff
	err  error
}

// fakeNomad is an in-memory Nomad: ParseHCL resolves file content to a
// pre-registered job (by identity, deep-copied on every call so the engine's
// cache and mutations never touch the fixture); Job/Plan/RegisterCAS operate
// on a small in-memory live-job table.
type fakeNomad struct {
	mu    sync.Mutex
	calls int // every call, of any method (RunDetection tests: parsing may be cached, this never is)

	parseByContent map[string]*api.Job
	parseErr       map[string]error
	parseCalls     map[string]int

	live map[string]*api.Job // absent: ErrJobNotFound

	plan    map[string]planFixture    // jobID -> what Plan returns; absent: {Type: "None"}
	lastTG  map[string]map[string]int // jobID -> group name -> Count Plan was called with
	planErr map[string]error

	registerErr   map[string]error
	registerCalls []registerCall
	versions      map[string]uint64 // jobID -> version bumped on every RegisterCAS

	allocs     map[string][]nomadx.Alloc
	allocsErr  map[string]error
	deployment map[string]*api.Deployment
	deployErr  map[string]error
}

type registerCall struct {
	id       string
	index    uint64
	preserve bool
}

func newFakeNomad() *fakeNomad {
	return &fakeNomad{
		parseByContent: map[string]*api.Job{},
		parseErr:       map[string]error{},
		parseCalls:     map[string]int{},
		live:           map[string]*api.Job{},
		plan:           map[string]planFixture{},
		lastTG:         map[string]map[string]int{},
		planErr:        map[string]error{},
		registerErr:    map[string]error{},
		versions:       map[string]uint64{},
		allocs:         map[string][]nomadx.Alloc{},
		allocsErr:      map[string]error{},
		deployment:     map[string]*api.Deployment{},
		deployErr:      map[string]error{},
	}
}

func (f *fakeNomad) setFile(content string, j *api.Job)    { f.parseByContent[content] = j }
func (f *fakeNomad) setParseErr(content string, err error) { f.parseErr[content] = err }
func (f *fakeNomad) setLive(j *api.Job)                    { f.live[*j.ID] = j }
func (f *fakeNomad) setDrift(id string, diff *api.JobDiff) { f.plan[id] = planFixture{diff: diff} }

func (f *fakeNomad) setAllocs(jobID string, allocs ...nomadx.Alloc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allocs[jobID] = allocs
}

func (f *fakeNomad) setDeployment(jobID string, d *api.Deployment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployment[jobID] = d
}

func (f *fakeNomad) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeNomad) ParseHCL(_ context.Context, content, _ string) (*api.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.parseCalls[content]++
	if err, ok := f.parseErr[content]; ok {
		return nil, err
	}
	j, ok := f.parseByContent[content]
	if !ok {
		return nil, fmt.Errorf("fakeNomad: no fixture registered for content %q", content)
	}
	return deepCopyJob(j)
}

func (f *fakeNomad) Job(_ context.Context, id string) (*api.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.live[id]
	if !ok {
		return nil, fmt.Errorf("job %s: %w", id, nomadx.ErrJobNotFound)
	}
	return deepCopyJob(j)
}

func (f *fakeNomad) Plan(_ context.Context, j *api.Job) (*api.JobPlanResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	id := *j.ID
	tg := make(map[string]int, len(j.TaskGroups))
	for _, g := range j.TaskGroups {
		if g != nil && g.Name != nil && g.Count != nil {
			tg[*g.Name] = *g.Count
		}
	}
	f.lastTG[id] = tg
	if err, ok := f.planErr[id]; ok {
		return nil, err
	}
	fx, ok := f.plan[id]
	if !ok {
		return &api.JobPlanResponse{Diff: &api.JobDiff{Type: "None", ID: id}}, nil
	}
	if fx.err != nil {
		return nil, fx.err
	}
	return &api.JobPlanResponse{Diff: fx.diff}, nil
}

func (f *fakeNomad) RegisterCAS(_ context.Context, j *api.Job, index uint64, preserve bool) (*nomadx.RegisterResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := *j.ID
	if err, ok := f.registerErr[id]; ok {
		return nil, err
	}
	f.registerCalls = append(f.registerCalls, registerCall{id, index, preserve})
	cp, err := deepCopyJob(j)
	if err != nil {
		return nil, err
	}
	newIndex := index + 1
	cp.JobModifyIndex = &newIndex
	f.versions[id]++
	version := f.versions[id]
	cp.Version = &version
	f.live[id] = cp
	return &nomadx.RegisterResult{EvalID: "eval-" + id, JobModifyIndex: newIndex}, nil
}

func (f *fakeNomad) Allocations(_ context.Context, jobID string) ([]nomadx.Alloc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.allocsErr[jobID]; ok {
		return nil, err
	}
	return append([]nomadx.Alloc(nil), f.allocs[jobID]...), nil
}

func (f *fakeNomad) LatestDeployment(_ context.Context, jobID string) (*api.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.deployErr[jobID]; ok {
		return nil, err
	}
	return f.deployment[jobID], nil
}

// -- fake Snapshots and Notifier ------------------------------------------

type fakeSnapshots struct {
	mu      sync.Mutex
	snap    gitwatch.Snapshot
	changed chan struct{}
}

func newFakeSnapshots() *fakeSnapshots { return &fakeSnapshots{changed: make(chan struct{}, 1)} }

func (f *fakeSnapshots) set(commit string, files ...gitwatch.File) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = gitwatch.Snapshot{Commit: commit, Files: files}
}

func (f *fakeSnapshots) Snapshot() gitwatch.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeSnapshots) Changed() <-chan struct{} { return f.changed }

type fakeNotifier struct {
	mu    sync.Mutex
	calls []*store.Deployment
}

func (n *fakeNotifier) Notify(_ context.Context, d *store.Deployment) {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := *d
	n.calls = append(n.calls, &cp)
}

// waitFor polls until n calls were recorded, or fails the test.
func (n *fakeNotifier) waitFor(t *testing.T, want int) []*store.Deployment {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		n.mu.Lock()
		got := len(n.calls)
		out := append([]*store.Deployment(nil), n.calls...)
		n.mu.Unlock()
		if got >= want {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("notify called %d times, want >= %d", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeClock is a controllable clock shared by the store and the engine in
// tests that need deterministic timing (the apply timeout). Like store's own
// test clock, Now ticks forward a little on every call so writes and events
// keep a strict order without real time passing; Advance moves it further,
// to simulate a timeout elapsing.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{t: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// -- harness ---------------------------------------------------------------

const testNamespace = "default"

type harness struct {
	t        *testing.T
	nomad    *fakeNomad
	store    *store.Store
	snap     *fakeSnapshots
	notifier *fakeNotifier
	hooks    *fakeHooks
	clock    *fakeClock
	engine   *Engine
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clock := newFakeClock(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "nops.db"), store.WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := &harness{
		t:        t,
		nomad:    newFakeNomad(),
		store:    st,
		snap:     newFakeSnapshots(),
		notifier: &fakeNotifier{},
		hooks:    newFakeHooks(),
		clock:    clock,
	}
	h.engine = New(Options{
		Store: st, Nomad: h.nomad, Snapshots: h.snap, Notifier: h.notifier, Hooks: h.hooks,
		Namespace: testNamespace, DriftInterval: time.Hour, EngineInterval: time.Hour, ApplyTimeout: time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.engine.now = clock.Now
	return h
}

func (h *harness) detect() {
	h.t.Helper()
	if err := h.engine.Detect(context.Background()); err != nil {
		h.t.Fatalf("Detect: %v", err)
	}
}

func (h *harness) active(jobID string) *store.Deployment {
	h.t.Helper()
	d, err := h.store.ActiveDeployment(context.Background(), testNamespace, jobID)
	if err != nil {
		h.t.Fatalf("ActiveDeployment(%s): %v", jobID, err)
	}
	return d
}

func (h *harness) latest(jobID string) *store.Deployment {
	h.t.Helper()
	d, err := h.store.LatestDeployment(context.Background(), testNamespace, jobID)
	if err != nil {
		h.t.Fatalf("LatestDeployment(%s): %v", jobID, err)
	}
	return d
}

func (h *harness) noActive(jobID string) {
	h.t.Helper()
	if _, err := h.store.ActiveDeployment(context.Background(), testNamespace, jobID); !errors.Is(err, store.ErrNotFound) {
		h.t.Fatalf("ActiveDeployment(%s): err = %v, want ErrNotFound", jobID, err)
	}
}

// -- tests -------------------------------------------------------------

func TestNoDrift(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	h.noActive("web")
	obs := h.engine.Observations()
	if len(obs) != 1 || obs[0].JobID != "web" || obs[0].Drift {
		t.Fatalf("observations = %+v", obs)
	}
}

func TestDriftAutoStaysDetected(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	d := h.active("web")
	if d.State != store.StateDetected || d.Policy != store.PolicyAuto || d.CommitSHA != "c1" {
		t.Fatalf("deployment = %+v", d)
	}
	if len(h.notifier.calls) != 0 {
		t.Errorf("auto must not notify: %+v", h.notifier.calls)
	}
	obs := h.engine.Observations()
	if len(obs) != 1 || !obs[0].Drift || obs[0].PlanDiff == "" {
		t.Fatalf("observations = %+v", obs)
	}
}

func TestDriftApprovalNotifies(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	d := h.active("web")
	if d.State != store.StatePendingApproval || d.Policy != store.PolicyApproval {
		t.Fatalf("deployment = %+v", d)
	}
	calls := h.notifier.waitFor(t, 1)
	if calls[0].ID != d.ID || calls[0].State != store.StatePendingApproval {
		t.Errorf("notify called with = %+v", calls[0])
	}
}

func TestPolicyNoneNeverCreatesButObserves(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "none", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	h.noActive("web")
	obs := h.engine.Observations()
	if len(obs) != 1 || !obs[0].Drift || obs[0].PlanDiff == "" {
		t.Fatalf("observations = %+v", obs)
	}
}

func TestSupersedeOnNewerSpec(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	first := h.active("web")
	if first.State != store.StatePendingApproval {
		t.Fatalf("setup: deployment = %+v", first)
	}

	// A new commit changes the job content (so it re-parses to a different
	// job, and its spec_hash differs) while still drifting.
	h.nomad.setFile("web-v2", managed("web", "approval", nil, taskGroup("g", 2, false)))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"})
	h.detect()

	old, err := h.store.GetDeployment(context.Background(), first.ID)
	if err != nil || old.State != store.StateSuperseded {
		t.Fatalf("old deployment = %+v, %v", old, err)
	}
	next := h.active("web")
	if next.ID == first.ID || next.CommitSHA != "c2" {
		t.Fatalf("new deployment = %+v", next)
	}
}

func TestUnrelatedCommitDoesNotSupersede(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	first := h.active("web")

	// A new commit that does not touch this job's content: same file content,
	// different commit SHA (e.g. another job changed).
	h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()

	second := h.active("web")
	if second.ID != first.ID || second.CommitSHA != "c1" {
		t.Fatalf("deployment changed on an unrelated commit: %+v", second)
	}
}

func TestRevalidationSupersedesOnLiveChange(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	first := h.active("web")
	if first.CASIndex != 0 {
		t.Fatalf("setup: cas_index = %d, want 0 (job did not exist)", first.CASIndex)
	}

	// Someone registers the job outside nops: the live index moves.
	live := managed("web", "approval", nil)
	idx := uint64(7)
	live.JobModifyIndex = &idx
	h.nomad.setLive(live)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"}) // still drifting
	h.detect()

	old, _ := h.store.GetDeployment(context.Background(), first.ID)
	if old.State != store.StateSuperseded {
		t.Fatalf("old deployment = %+v, want superseded", old)
	}
	next := h.active("web")
	if next.CASIndex != 7 {
		t.Fatalf("new deployment cas_index = %d, want 7", next.CASIndex)
	}
}

func TestRevalidationCompletesOnEmptyPlan(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	first := h.active("web")

	// The job converges (someone applied it by hand): no more drift.
	delete(h.nomad.plan, "web")
	h.detect()

	got, _ := h.store.GetDeployment(context.Background(), first.ID)
	if got.State != store.StateCompleted {
		t.Fatalf("deployment = %+v, want completed", got)
	}
	h.noActive("web")
}

func TestInFlightDeploymentIsUntouched(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	d := h.active("web")
	if err := h.store.Transition(context.Background(), d.ID, store.StatePreHook,
		store.Transition{From: store.StatePendingApproval, Actor: "alice", DecidedBy: "alice"}); err != nil {
		t.Fatal(err)
	}

	// A newer commit, and the job even disappears from the repo entirely.
	h.snap.set("c2")
	h.detect()

	got := h.active("web")
	if got.ID != d.ID || got.State != store.StatePreHook {
		t.Fatalf("in-flight deployment was touched: %+v", got)
	}
}

func TestJobRemovedFromRepo(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	first := h.active("web")

	h.snap.set("c2") // no files at all
	h.detect()

	got, _ := h.store.GetDeployment(context.Background(), first.ID)
	if got.State != store.StateSuperseded || got.Error != "" {
		t.Fatalf("deployment = %+v, want superseded", got)
	}
	evs, _ := h.store.Events(context.Background(), first.ID)
	if last := evs[len(evs)-1]; last.Message != "job removed from repo" {
		t.Errorf("last event message = %q", last.Message)
	}
}

func TestMissingHookFailsImmediately(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"}))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"}) // no hook file at all

	h.detect()

	d, err := h.store.LatestDeployment(context.Background(), testNamespace, "web")
	if err != nil || d.State != store.StateFailed {
		t.Fatalf("deployment = %+v, %v, want failed", d, err)
	}
	if want := `pre-hook "web-migrate" not found in repo at commit c1`; d.Error != want {
		t.Errorf("error = %q, want %q", d.Error, want)
	}
	calls := h.notifier.waitFor(t, 1)
	if calls[0].State != store.StateFailed {
		t.Errorf("notify called with = %+v", calls[0])
	}
}

func TestDeclaredHookIsFoundAndSynced(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"}))
	h.nomad.setFile("hook-v1", hookJob("web-migrate"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.nomad.setDrift("web-migrate", &api.JobDiff{Type: "Edited", ID: "web-migrate"}) // hook not registered yet
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "web-migrate.nomad.hcl", Content: "hook-v1"})

	h.detect()

	d := h.active("web")
	if d.State != store.StateDetected {
		t.Fatalf("deployment = %+v, want detected (hook found)", d)
	}
	if len(h.nomad.registerCalls) != 1 || h.nomad.registerCalls[0].id != "web-migrate" {
		t.Fatalf("hook sync calls = %+v", h.nomad.registerCalls)
	}
}

func TestRetryRuleDoesNotRecreateSameFailure(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{"nops_pre_hook": "missing"}))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	first := h.latest("web")
	if first.State != store.StateFailed {
		t.Fatalf("setup: deployment = %+v", first)
	}

	// Same commit, same spec, same live state: detection runs again (drift ticker).
	h.detect()

	second := h.latest("web")
	if second.ID != first.ID {
		t.Fatalf("a new deployment was created for the same failure: %+v", second)
	}

	// The live job changes outside nops: the retry rule no longer applies.
	live := managed("web", "auto", nil)
	idx := uint64(3)
	live.JobModifyIndex = &idx
	h.nomad.setLive(live)
	h.detect()

	third := h.latest("web")
	if third.ID == first.ID {
		t.Fatalf("no new deployment after the live job changed")
	}
}

// TestBlockedByAppliedFailureSurvivesLiveIndexChange covers the anti-loop
// rule (docs/design/engine-apply.md, decision 6): a deployment that reached
// the register and then failed blocks a retry for the same spec_hash even
// once the live index changes (Nomad's own auto_revert, for example), unlike
// the ordinary retry rule above. Only a new commit unblocks it, and the
// block is visible as Observation.BlockedBy/BlockedReason (decision 7).
func TestBlockedByAppliedFailureSurvivesLiveIndexChange(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	h.nomad.setFile("web-v1", job)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	hash, err := specHash(job)
	if err != nil {
		t.Fatal(err)
	}

	// A deployment that reached the register and then failed (as if the
	// apply never became healthy and Nomad's own auto_revert moved the live
	// job back since).
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: hash,
		JobSpec: `{"ID":"web"}`, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(context.Background(), d.ID, store.StateApplying,
		store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetApplied(context.Background(), d.ID, 1, "eval-1"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(context.Background(), d.ID, store.StateFailed,
		store.Transition{From: store.StateApplying, Actor: "nops", Error: "apply did not become healthy"}); err != nil {
		t.Fatal(err)
	}

	h.detect()
	if latest := h.latest("web"); latest.ID != d.ID {
		t.Fatalf("a new deployment was created: %+v", latest)
	}
	obs := h.engine.Observations()
	if len(obs) != 1 || obs[0].BlockedBy != d.ID || obs[0].BlockedReason == "" {
		t.Fatalf("observations = %+v", obs)
	}

	// The live index changes (Nomad's own auto_revert): the ordinary retry
	// rule above would now allow a retry, but this deployment reached the
	// register, so it still blocks.
	live := managed("web", "auto", nil)
	idx := uint64(3)
	live.JobModifyIndex = &idx
	h.nomad.setLive(live)
	h.detect()

	if latest := h.latest("web"); latest.ID != d.ID {
		t.Fatalf("a new deployment was created after the live index changed: %+v", latest)
	}
	obs = h.engine.Observations()
	if len(obs) != 1 || obs[0].BlockedBy != d.ID {
		t.Fatalf("observations = %+v, want still blocked", obs)
	}

	// A new commit (a different spec_hash) unblocks it.
	h.nomad.setFile("web-v2", managed("web", "auto", nil, taskGroup("g", 2, false)))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"})
	h.detect()

	if latest := h.latest("web"); latest.ID == d.ID {
		t.Fatalf("no new deployment was created after a new commit")
	}
	obs = h.engine.Observations()
	if len(obs) != 1 || obs[0].BlockedBy != "" {
		t.Fatalf("observations = %+v, want unblocked", obs)
	}
}

func TestBlockedRetry(t *testing.T) {
	dep := func(state store.State, hash string, casIndex, appliedIndex uint64) *store.Deployment {
		return &store.Deployment{ID: "dep-1", State: state, SpecHash: hash, CASIndex: casIndex, AppliedIndex: appliedIndex}
	}
	cases := []struct {
		name          string
		latest        *store.Deployment
		hash          string
		liveIndex     uint64
		wantBlockedBy string
	}{
		{"not failed or rejected", dep(store.StateCompleted, "h1", 0, 0), "h1", 0, ""},
		{"different spec hash", dep(store.StateFailed, "h1", 0, 0), "h2", 0, ""},
		{"failed before register, live unchanged: blocked", dep(store.StateFailed, "h1", 5, 0), "h1", 5, "dep-1"},
		{"failed before register, live changed: unblocked", dep(store.StateFailed, "h1", 5, 0), "h1", 6, ""},
		{"rejected before register, live unchanged: blocked", dep(store.StateRejected, "h1", 5, 0), "h1", 5, "dep-1"},
		{"failed after register: blocked regardless of live index", dep(store.StateFailed, "h1", 5, 9), "h1", 6, "dep-1"},
		{"rejected after register (never happens, still safe)", dep(store.StateRejected, "h1", 5, 9), "h1", 6, "dep-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blockedBy, reason := blockedRetry(c.latest, c.hash, c.liveIndex)
			if blockedBy != c.wantBlockedBy {
				t.Errorf("blockedBy = %q, want %q", blockedBy, c.wantBlockedBy)
			}
			if (reason == "") != (blockedBy == "") {
				t.Errorf("reason = %q inconsistent with blockedBy = %q", reason, blockedBy)
			}
		})
	}
}

func TestDuplicateJobIDsAreBothIgnored(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-a", managed("web", "auto", nil))
	h.nomad.setFile("web-b", managed("web", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1",
		gitwatch.File{Path: "a.nomad.hcl", Content: "web-a"},
		gitwatch.File{Path: "b.nomad.hcl", Content: "web-b"})

	h.detect()

	h.noActive("web")
	if obs := h.engine.Observations(); len(obs) != 0 {
		t.Errorf("observations = %+v, want none for a duplicate job", obs)
	}
}

func TestParseCacheAvoidsReparsing(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()
	h.detect()
	h.detect()

	if n := h.nomad.parseCalls["web-v1"]; n != 1 {
		t.Errorf("ParseHCL called %d times for unchanged content, want 1", n)
	}

	// A change to the content is a cache miss and drops the old entry.
	h.nomad.setFile("web-v2", managed("web", "auto", nil))
	h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"})
	h.detect()
	if n := h.nomad.parseCalls["web-v2"]; n != 1 {
		t.Errorf("ParseHCL called %d times for the new content, want 1", n)
	}
	if got := len(h.engine.snapshotParseCache()); got != 1 {
		t.Errorf("parse cache has %d entries, want 1 (old content dropped)", got)
	}
}

func TestScalingCountIsNotFought(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil, taskGroup("g", 1, true)))
	live := managed("web", "auto", nil, taskGroup("g", 1, true))
	live.TaskGroups[0].Count = intPtr(9) // the autoscaler moved it
	idx := uint64(5)
	live.JobModifyIndex = &idx
	h.nomad.setLive(live)
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	if got := h.nomad.lastTG["web"]["g"]; got != 9 {
		t.Errorf("Plan was called with count %d for the scaling group, want 9 (the live count)", got)
	}
	h.noActive("web") // no diff was reported (fakeNomad.Plan defaults to "None")
}

func TestObservationsResetEachCycle(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "none", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	if obs := h.engine.Observations(); len(obs) != 1 {
		t.Fatalf("observations = %+v", obs)
	}

	h.snap.set("c2") // the job leaves the repository
	h.detect()
	if obs := h.engine.Observations(); len(obs) != 0 {
		t.Fatalf("observations = %+v, want none", obs)
	}
}

func TestDetectAbortsOnStoreFailure(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.store.Close() // every store call now fails

	if err := h.engine.Detect(context.Background()); err == nil {
		t.Fatal("Detect: want an error when the store is unreachable")
	}
}

func intPtr(v int) *int { return &v }
