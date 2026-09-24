package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// -- fake Hooks --------------------------------------------------------

// fakeHooks is an in-memory hooks.Runner: Run returns the fixture set for
// (deploymentID, phase), or a fixed Result{State: succeeded} if none was set.
type fakeHooks struct {
	mu      sync.Mutex
	results map[string]hooks.Result
	errs    map[string]error
	calls   []hooks.Request
}

func newFakeHooks() *fakeHooks {
	return &fakeHooks{results: map[string]hooks.Result{}, errs: map[string]error{}}
}

func hookKey(deploymentID, phase string) string { return deploymentID + ":" + phase }

func (f *fakeHooks) setResult(deploymentID, phase string, res hooks.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[hookKey(deploymentID, phase)] = res
}

func (f *fakeHooks) setErr(deploymentID, phase string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[hookKey(deploymentID, phase)] = err
}

func (f *fakeHooks) Run(_ context.Context, req hooks.Request) (hooks.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	key := hookKey(req.DeploymentID, req.Phase)
	if err, ok := f.errs[key]; ok {
		return hooks.Result{}, err
	}
	if res, ok := f.results[key]; ok {
		return res, nil
	}
	return hooks.Result{State: store.HookSucceeded}, nil
}

// -- harness helpers for apply ------------------------------------------

func (h *harness) step(d *store.Deployment) {
	h.t.Helper()
	h.engine.applyStep(context.Background(), d)
}

func (h *harness) get(id string) *store.Deployment {
	h.t.Helper()
	d, err := h.store.GetDeployment(context.Background(), id)
	if err != nil {
		h.t.Fatalf("GetDeployment(%s): %v", id, err)
	}
	return d
}

// createApplying creates a deployment for jobID directly in the store and
// drives it to applying, as detection would for an auto job with no pre-hook.
func (h *harness) createApplying(jobID string, job *api.Job, casIndex uint64) *store.Deployment {
	h.t.Helper()
	specJSON := mustMarshal(h.t, job)
	d := &store.Deployment{
		JobID: jobID, Namespace: testNamespace, CommitSHA: "c1", SpecHash: "hash-" + jobID,
		JobSpec: specJSON, PlanDiff: "", Policy: store.PolicyAuto, CASIndex: casIndex,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		h.t.Fatalf("CreateDeployment: %v", err)
	}
	if err := h.store.Transition(context.Background(), d.ID, store.StateApplying,
		store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		h.t.Fatalf("Transition to applying: %v", err)
	}
	return h.get(d.ID)
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// errNomadCASConflict builds an error shaped like nomadx.RegisterCAS's own on
// a CAS conflict, for tests of the fake Nomad's registerErr fixture.
func errNomadCASConflict() error {
	return fmt.Errorf("register job web at index 0: %w: conflict", nomadx.ErrCASConflict)
}

// -- tests: stepDetected / nextAfterDecision ------------------------------

func TestStepDetectedRoutesToApplyingWithoutPreHook(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: specJSON, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateApplying {
		t.Fatalf("state = %s, want applying", got.State)
	}
}

func TestStepDetectedRoutesToPreHookWhenDeclared(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"})
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: specJSON, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StatePreHook {
		t.Fatalf("state = %s, want pre_hook", got.State)
	}
}

func TestStepDetectedWithCorruptJobSpecLogsAndDoesNotPanic(t *testing.T) {
	h := newHarness(t)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: `not json`, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	h.step(d) // must not panic

	got := h.get(d.ID)
	if got.State != store.StateDetected {
		t.Fatalf("state = %s, want unchanged (detected)", got.State)
	}
}

// -- tests: hooks ---------------------------------------------------------

func TestStepPreHookSucceededMovesToApplying(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"})
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: specJSON, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(context.Background(), d.ID, store.StatePreHook,
		store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	d = h.get(d.ID)
	h.hooks.setResult(d.ID, "pre", hooks.Result{State: store.HookSucceeded})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateApplying {
		t.Fatalf("state = %s, want applying", got.State)
	}
	if len(h.hooks.calls) != 1 || h.hooks.calls[0].HookJobID != "web-migrate" || h.hooks.calls[0].Phase != "pre" {
		t.Errorf("hook call = %+v", h.hooks.calls)
	}
}

func TestStepPreHookFailedFailsDeploymentAndNotifies(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"})
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: specJSON, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(context.Background(), d.ID, store.StatePreHook,
		store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	d = h.get(d.ID)
	h.hooks.setResult(d.ID, "pre", hooks.Result{State: store.HookFailed, Error: "exit code 1"})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if h.nomad.registerCalls != nil {
		t.Errorf("a failed pre-hook must never register: %+v", h.nomad.registerCalls)
	}
	h.notifier.waitFor(t, 1)
}

func TestStepPostHookTimedOutFails(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_post_hook": "web-smoke"})
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: specJSON, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	for _, to := range []store.State{store.StateApplying, store.StatePostHook} {
		if err := h.store.Transition(context.Background(), d.ID, to,
			store.Transition{From: h.get(d.ID).State, Actor: "nops"}); err != nil {
			t.Fatal(err)
		}
	}
	d = h.get(d.ID)
	h.hooks.setResult(d.ID, "post", hooks.Result{State: store.HookTimedOut, Error: "deadline exceeded"})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
}

func TestStepHookErrorRetriesNextCycle(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"})
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: specJSON, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(context.Background(), d.ID, store.StatePreHook,
		store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	d = h.get(d.ID)
	h.hooks.setErr(d.ID, "pre", errors.New("nomad unreachable"))

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StatePreHook {
		t.Fatalf("state = %s, want pre_hook unchanged after a hook error", got.State)
	}
}

// -- tests: register --------------------------------------------------

func TestStepRegisterSuccessRecordsAppliedIndex(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	d := h.createApplying("web", job, 0)

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateApplying || got.AppliedIndex == 0 || got.EvalID == "" {
		t.Fatalf("deployment = %+v", got)
	}
	if len(h.nomad.registerCalls) != 1 || h.nomad.registerCalls[0].index != 0 {
		t.Errorf("register calls = %+v", h.nomad.registerCalls)
	}
}

func TestStepRegisterEmptyPlanCompletes(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	live := managed("web", "auto", nil)
	idx := uint64(5)
	live.JobModifyIndex = &idx
	h.nomad.setLive(live)
	// No drift set: fakeNomad.Plan defaults to {Type: "None"} for a job with
	// no fixture registered.
	d := h.createApplying("web", job, 5)

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateCompleted {
		t.Fatalf("state = %s, want completed", got.State)
	}
	if len(h.nomad.registerCalls) != 0 {
		t.Errorf("must not register on an empty plan: %+v", h.nomad.registerCalls)
	}
}

func TestStepRegisterCASConflictFails(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.nomad.registerErr["web"] = errNomadCASConflict()
	d := h.createApplying("web", job, 0)

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	h.notifier.waitFor(t, 1)
}

func TestStepRegisterAlreadyAppliedAfterCrashRecovers(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.createApplying("web", job, 0)

	// Simulate the register having already happened (a crash between it
	// succeeding and SetApplied): the live job now matches our spec exactly,
	// but at a different index than cas_index.
	live := managed("web", "auto", nil)
	idx := uint64(1)
	live.JobModifyIndex = &idx
	h.nomad.setLive(live)
	// No drift fixture for "web": Plan(job) against the live job returns
	// {Type: "None"}, i.e. our spec is already live.

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateApplying || got.AppliedIndex != 1 || got.EvalID != "" {
		t.Fatalf("deployment = %+v", got)
	}
	if len(h.nomad.registerCalls) != 0 {
		t.Errorf("must not register again: %+v", h.nomad.registerCalls)
	}
}

func TestStepRegisterRealConflictFails(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.createApplying("web", job, 0)

	// The live job moved to a different index AND still differs from our spec.
	live := managed("web", "auto", nil)
	idx := uint64(1)
	live.JobModifyIndex = &idx
	h.nomad.setLive(live)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
}

// -- tests: health ------------------------------------------------------

func (h *harness) applyingWithIndex(jobID string, job *api.Job, appliedIndex uint64) *store.Deployment {
	h.t.Helper()
	d := h.createApplying(jobID, job, 0)
	if err := h.store.SetApplied(context.Background(), d.ID, appliedIndex, "eval-1"); err != nil {
		h.t.Fatal(err)
	}
	return h.get(d.ID)
}

func TestStepHealthNomadDeploymentSuccessfulCompletes(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.applyingWithIndex("web", job, 9)
	h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusSuccessful})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateCompleted {
		t.Fatalf("state = %s, want completed", got.State)
	}
}

func TestStepHealthNomadDeploymentFailedFails(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.applyingWithIndex("web", job, 9)
	h.nomad.setDeployment("web", &api.Deployment{
		ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusFailed, StatusDescription: "progress deadline hit",
	})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	h.notifier.waitFor(t, 1)
}

func TestStepHealthNomadDeploymentRunningWaits(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.applyingWithIndex("web", job, 9)
	h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusRunning})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateApplying {
		t.Fatalf("state = %s, want still applying", got.State)
	}
}

// liveApplied sets fakeNomad's live job for jobID to a copy of job at the
// given modify index and version, as if it had already been applied: the
// allocations-path health tests need a live job to read Version from,
// without actually driving a register through stepRegister.
func (h *harness) liveApplied(jobID string, job *api.Job, modifyIndex, version uint64) {
	h.t.Helper()
	live, err := deepCopyJob(job)
	if err != nil {
		h.t.Fatal(err)
	}
	live.JobModifyIndex, live.Version = &modifyIndex, &version
	h.nomad.setLive(live)
}

func TestStepHealthAllocationsPathNoNomadDeployment(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.applyingWithIndex("web", job, 9)
	// No Nomad deployment tracks this job (e.g. batch): health falls back to
	// the allocations of the applied job version.
	h.liveApplied("web", job, 9, 3)
	h.nomad.setAllocs("web", nomadx.Alloc{ID: "a1", JobVersion: 3, ClientStatus: "running"})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateCompleted {
		t.Fatalf("state = %s, want completed", got.State)
	}
}

func TestStepHealthAllocationsPathFailedAlloc(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.applyingWithIndex("web", job, 9)
	h.liveApplied("web", job, 9, 3)
	h.nomad.setAllocs("web", nomadx.Alloc{ID: "a1", JobVersion: 3, ClientStatus: "failed", Failure: "oom"})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
}

func TestStepHealthCompletesToPostHookWhenDeclared(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_post_hook": "web-smoke"})
	d := h.applyingWithIndex("web", job, 9)
	h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusSuccessful})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StatePostHook {
		t.Fatalf("state = %s, want post_hook", got.State)
	}
}

func TestStepHealthTimeoutFails(t *testing.T) {
	h := newHarness(t)
	h.engine.applyTimeout = time.Minute
	job := managed("web", "auto", nil)
	d := h.applyingWithIndex("web", job, 9)
	// Still waiting: no Nomad deployment, no allocation yet.
	h.clock.Advance(2 * time.Minute)

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed (timeout)", got.State)
	}
}

// -- tests: Approve / Reject ---------------------------------------------

func (h *harness) pendingApproval(jobID string, job *api.Job, specHash string) *store.Deployment {
	h.t.Helper()
	specJSON := mustMarshal(h.t, job)
	d := &store.Deployment{
		JobID: jobID, Namespace: testNamespace, CommitSHA: "c1", SpecHash: specHash,
		JobSpec: specJSON, Policy: store.PolicyApproval, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.Transition(context.Background(), d.ID, store.StatePendingApproval,
		store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		h.t.Fatal(err)
	}
	return h.get(d.ID)
}

func TestApproveRoutesLikeDetected(t *testing.T) {
	h := newHarness(t)
	d := h.pendingApproval("web", managed("web", "approval", nil), "h1")

	if err := h.engine.Approve(context.Background(), d.ID, "h1", "alice"); err != nil {
		t.Fatal(err)
	}

	got := h.get(d.ID)
	if got.State != store.StateApplying || got.DecidedBy != "alice" {
		t.Fatalf("deployment = %+v", got)
	}
}

func TestApproveWithPreHookGoesToPreHook(t *testing.T) {
	h := newHarness(t)
	d := h.pendingApproval("web", managed("web", "approval", map[string]string{"nops_pre_hook": "web-migrate"}), "h1")

	if err := h.engine.Approve(context.Background(), d.ID, "h1", "alice"); err != nil {
		t.Fatal(err)
	}

	got := h.get(d.ID)
	if got.State != store.StatePreHook {
		t.Fatalf("state = %s, want pre_hook", got.State)
	}
}

func TestApproveRefusesStaleSpecHash(t *testing.T) {
	h := newHarness(t)
	d := h.pendingApproval("web", managed("web", "approval", nil), "h1")

	err := h.engine.Approve(context.Background(), d.ID, "different-hash", "alice")
	if !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("err = %v, want ErrStaleApproval", err)
	}
	if got := h.get(d.ID); got.State != store.StatePendingApproval {
		t.Errorf("a refused approval must not change state: %s", got.State)
	}
}

func TestApproveRefusesWrongState(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.createApplying("web", job, 0) // already applying, not pending_approval

	if err := h.engine.Approve(context.Background(), d.ID, "h1", "alice"); err == nil {
		t.Fatal("want an error approving a non-pending deployment")
	}
}

func TestApproveValidatesActorAndDeploymentID(t *testing.T) {
	h := newHarness(t)
	d := h.pendingApproval("web", managed("web", "approval", nil), "h1")

	if err := h.engine.Approve(context.Background(), d.ID, "h1", ""); err == nil {
		t.Error("want an error with no actor")
	}
	if err := h.engine.Approve(context.Background(), "missing-id", "h1", "alice"); err == nil {
		t.Error("want an error for an unknown deployment ID")
	}
}

func TestRejectRefusesWrongState(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.createApplying("web", job, 0) // already applying, not pending_approval

	if err := h.engine.Reject(context.Background(), d.ID, "alice"); err == nil {
		t.Fatal("want an error rejecting a non-pending deployment")
	}
	if err := h.engine.Reject(context.Background(), d.ID, ""); err == nil {
		t.Error("want an error with no actor")
	}
}

func TestRejectMovesToRejected(t *testing.T) {
	h := newHarness(t)
	d := h.pendingApproval("web", managed("web", "approval", nil), "h1")

	if err := h.engine.Reject(context.Background(), d.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	got := h.get(d.ID)
	if got.State != store.StateRejected || got.DecidedBy != "alice" {
		t.Fatalf("deployment = %+v", got)
	}
}

// -- tests: the apply cycle's in-flight guard and the detection race -----

func TestApplyCycleNeverStartsTheSameDeploymentTwice(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	d := h.createApplying("web", job, 0)

	if !h.engine.startApply(d.ID) {
		t.Fatal("first startApply should succeed")
	}
	if h.engine.startApply(d.ID) {
		t.Fatal("a second startApply for the same ID must fail while the first is in flight")
	}
	h.engine.finishApply(d.ID)
	if !h.engine.startApply(d.ID) {
		t.Fatal("startApply should succeed again once the deployment is no longer in flight")
	}
}

func runApplyInBackground(t *testing.T, e *Engine) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.RunApply(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunApply did not stop after ctx was cancelled")
		}
	}
}

// TestRunApplyInitialAndTicker checks that RunApply advances a deployment
// right away and again on every EngineInterval tick.
func TestRunApplyInitialAndTicker(t *testing.T) {
	h := newHarness(t)
	h.engine.engineInterval = 20 * time.Millisecond
	job := managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"})
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: specJSON, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	stop := runApplyInBackground(t, h.engine)
	defer stop()

	waitUntil(t, func() bool { return h.get(d.ID).State == store.StatePreHook },
		"RunApply did not advance detected -> pre_hook on the initial cycle")

	h.hooks.setResult(d.ID, "pre", hooks.Result{State: store.HookSucceeded})
	waitUntil(t, func() bool { return h.get(d.ID).State == store.StateApplying },
		"RunApply did not advance pre_hook -> applying on the ticker")
}

func TestApplyTransitionLogsStoreErrorWithoutPanicking(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: specJSON, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	h.step(d) // the store is closed: applyTransition must log and return, not panic
}

func TestApplyStepDropsSilentlyOnDetectionRace(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1",
		JobSpec: specJSON, Policy: store.PolicyAuto, CASIndex: 0,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	// Detection superseded it concurrently.
	if err := h.store.Transition(context.Background(), d.ID, store.StateSuperseded,
		store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}

	// applyStep still holds the stale, in-memory "detected" copy: it must not
	// panic or corrupt the row, just drop it (logged, not asserted here).
	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateSuperseded {
		t.Fatalf("state = %s, want superseded (untouched)", got.State)
	}
}
