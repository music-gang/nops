package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

const testPoll = 2 * time.Second

// clock is a fake time source shared by the store and the runner.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeNomad is an in-memory Nomad: jobs, allocations and idempotent dispatch.
type fakeNomad struct {
	jobs   map[string]*api.Job
	allocs map[string][]nomadx.Alloc
	tokens map[string]string // idempotency token -> child ID

	dispatches []dispatchCall
	stopped    []string
	calls      int // every call, of any method

	jobErr, dispatchErr, allocsErr, stopErr error
}

type dispatchCall struct {
	Parent string
	Meta   map[string]string
	Token  string
}

func newFakeNomad() *fakeNomad {
	return &fakeNomad{
		jobs:   map[string]*api.Job{},
		allocs: map[string][]nomadx.Alloc{},
		tokens: map[string]string{},
	}
}

func (f *fakeNomad) Job(_ context.Context, id string) (*api.Job, error) {
	f.calls++
	if f.jobErr != nil {
		return nil, f.jobErr
	}
	j, ok := f.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s: %w", id, nomadx.ErrJobNotFound)
	}
	return j, nil
}

func (f *fakeNomad) Dispatch(_ context.Context, parent string, meta map[string]string, token string) (*nomadx.DispatchResult, error) {
	f.calls++
	f.dispatches = append(f.dispatches, dispatchCall{parent, meta, token})
	if f.dispatchErr != nil {
		return nil, f.dispatchErr
	}
	if id, ok := f.tokens[token]; ok {
		return &nomadx.DispatchResult{JobID: id}, nil
	}
	id := fmt.Sprintf("%s/dispatch-%d", parent, len(f.tokens)+1)
	f.tokens[token] = id
	f.jobs[id] = &api.Job{ID: &id, Status: ptr("pending")}
	return &nomadx.DispatchResult{JobID: id}, nil
}

func (f *fakeNomad) Allocations(_ context.Context, jobID string) ([]nomadx.Alloc, error) {
	f.calls++
	if f.allocsErr != nil {
		return nil, f.allocsErr
	}
	return f.allocs[jobID], nil
}

// StopJob marks the child dead and its allocations complete but not desired to run.
func (f *fakeNomad) StopJob(_ context.Context, id string) error {
	f.calls++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, id)
	if j, ok := f.jobs[id]; ok {
		j.Status = ptr("dead")
	}
	for i := range f.allocs[id] {
		f.allocs[id][i].ClientStatus, f.allocs[id][i].DesiredStatus = "complete", "stop"
	}
	return nil
}

// finish simulates a child whose allocations ended in the given client status.
func (f *fakeNomad) finish(childID, clientStatus, failure string) {
	f.jobs[childID].Status = ptr("dead")
	f.allocs[childID] = []nomadx.Alloc{{ID: "alloc-" + childID, ClientStatus: clientStatus, DesiredStatus: "run", Failure: failure}}
}

func ptr[T any](v T) *T { return &v }

// harness wires a runner to a fake Nomad and a real SQLite store.
type harness struct {
	t      *testing.T
	clk    *clock
	nomad  *fakeNomad
	store  *store.Store
	runner *Runner
	depID  string
	// onPoll runs at every wait, after the clock advanced; polls counts them.
	onPoll func(n int)
	polls  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		clk:   &clock{t: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)},
		nomad: newFakeNomad(),
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "nops.db"), store.WithClock(h.clk.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.store = st

	d := &store.Deployment{JobID: "api", Namespace: "default", CommitSHA: "abc123", SpecHash: "h", JobSpec: "{}", Policy: store.PolicyAuto}
	if err := st.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	h.depID = d.ID

	h.runner = New(h.nomad, st, slog.New(slog.NewTextHandler(io.Discard, nil)), testPoll,
		WithClock(h.clk.now),
		WithWait(func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			h.clk.advance(d)
			h.polls++
			if h.onPoll != nil {
				h.onPoll(h.polls)
			}
			return nil
		}))
	return h
}

// addHook registers a valid parameterized hook job.
func (h *harness) addHook(id string, required, optional []string) {
	h.nomad.jobs[id] = &api.Job{
		ID:               &id,
		Type:             ptr("batch"),
		Meta:             map[string]string{"nops_role": "hook"},
		ParameterizedJob: &api.ParameterizedJobConfig{MetaRequired: required, MetaOptional: optional},
	}
}

func (h *harness) request(hookID string, timeout time.Duration) Request {
	return Request{
		DeploymentID: h.depID,
		Phase:        "pre",
		HookJobID:    hookID,
		Commit:       "abc123",
		Timeout:      timeout,
		Target:       targetJob("api", group("g", dockerTask("api", "reg/api:2"))),
	}
}

func (h *harness) hookRun() *store.HookRun {
	h.t.Helper()
	run, err := h.store.GetHookRun(context.Background(), h.depID, "pre")
	if err != nil {
		h.t.Fatal(err)
	}
	return run
}

func mustRun(t *testing.T, r *Runner, req Request) Result {
	t.Helper()
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestRunSucceeds(t *testing.T) {
	h := newHarness(t)
	h.addHook("api-migrate", []string{"nops_deployment_id"}, []string{"nops_phase", "nops_image_api"})
	h.onPoll = func(n int) {
		if n == 2 {
			h.nomad.finish("api-migrate/dispatch-1", "complete", "")
		}
	}

	res := mustRun(t, h.runner, h.request("api-migrate", time.Minute))
	if res.State != store.HookSucceeded || res.Error != "" || res.DispatchedJobID != "api-migrate/dispatch-1" {
		t.Fatalf("result = %+v", res)
	}
	if h.polls != 2 {
		t.Errorf("polls = %d, want 2", h.polls)
	}

	if len(h.nomad.dispatches) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(h.nomad.dispatches))
	}
	d := h.nomad.dispatches[0]
	wantMeta := map[string]string{"nops_deployment_id": h.depID, "nops_phase": "pre", "nops_image_api": "reg/api:2"}
	if d.Parent != "api-migrate" || d.Token != h.depID+":pre" || !reflect.DeepEqual(d.Meta, wantMeta) {
		t.Errorf("dispatch = %+v, want meta %v and token %s:pre", d, wantMeta, h.depID)
	}

	run := h.hookRun()
	if run.State != store.HookSucceeded || run.DispatchedJobID != "api-migrate/dispatch-1" || run.FinishedAt.IsZero() {
		t.Errorf("stored run = %+v", run)
	}
	if len(h.nomad.stopped) != 0 {
		t.Errorf("a successful hook must not be stopped: %v", h.nomad.stopped)
	}
}

func TestRunFailed(t *testing.T) {
	for _, status := range []string{"failed", "lost"} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t)
			h.addHook("hook", []string{"nops_deployment_id"}, nil)
			h.onPoll = func(n int) { h.nomad.finish("hook/dispatch-1", status, "task t: Exit Code: 1") }

			res := mustRun(t, h.runner, h.request("hook", time.Minute))
			if res.State != store.HookFailed || !strings.Contains(res.Error, "Exit Code: 1") || !strings.Contains(res.Error, status) {
				t.Fatalf("result = %+v", res)
			}
			if run := h.hookRun(); run.State != store.HookFailed || run.Error != res.Error {
				t.Errorf("stored run = %+v", run)
			}
		})
	}
}

func TestRunStoppedFromOutside(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	h.onPoll = func(n int) {
		h.nomad.jobs["hook/dispatch-1"].Status = ptr("dead")
		h.nomad.allocs["hook/dispatch-1"] = []nomadx.Alloc{{ID: "a1", ClientStatus: "complete", DesiredStatus: "stop"}}
	}
	res := mustRun(t, h.runner, h.request("hook", time.Minute))
	if res.State != store.HookFailed || !strings.Contains(res.Error, "stopped from outside") {
		t.Fatalf("result = %+v", res)
	}
	if len(h.nomad.stopped) != 0 {
		t.Errorf("nops stopped a job that was already stopped: %v", h.nomad.stopped)
	}
}

func TestRunTimesOut(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	h.nomad.allocs["hook/dispatch-1"] = []nomadx.Alloc{{ID: "a1", ClientStatus: "running", DesiredStatus: "run"}}

	start := h.clk.now()
	res := mustRun(t, h.runner, h.request("hook", 10*time.Second))
	if res.State != store.HookTimedOut || !strings.Contains(res.Error, "timed out after 10s") {
		t.Fatalf("result = %+v", res)
	}
	if !reflect.DeepEqual(h.nomad.stopped, []string{"hook/dispatch-1"}) {
		t.Errorf("stopped = %v, want the child", h.nomad.stopped)
	}
	if elapsed := h.clk.now().Sub(start); elapsed != 10*time.Second {
		t.Errorf("waited %s, want the timeout (10s) to the poll", elapsed)
	}
	if run := h.hookRun(); run.State != store.HookTimedOut || run.FinishedAt.IsZero() {
		t.Errorf("stored run = %+v", run)
	}
}

// A hook that ends right at the deadline is a success: the outcome is read
// before the timeout is applied.
func TestRunOutcomeBeatsTimeout(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	h.onPoll = func(n int) { h.nomad.finish("hook/dispatch-1", "complete", "") }

	res := mustRun(t, h.runner, h.request("hook", 2*time.Second))
	if res.State != store.HookSucceeded {
		t.Fatalf("result = %+v", res)
	}
}

// The timeout counts from started_at: a restart does not give the hook more time.
func TestRunTimeoutCountsFromStartedAt(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	ctx := context.Background()

	run, _, err := h.store.EnsureHookRun(ctx, h.depID, "pre", "hook", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	child, err := h.nomad.Dispatch(ctx, "hook", nil, run.IdempotencyToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateHookRun(ctx, run.ID, store.HookUpdate{State: store.HookRunning, DispatchedJobID: child.JobID}); err != nil {
		t.Fatal(err)
	}
	h.nomad.allocs[child.JobID] = []nomadx.Alloc{{ID: "a1", ClientStatus: "running", DesiredStatus: "run"}}

	h.clk.advance(5 * time.Minute) // nops was down for longer than the timeout

	// The request asks for a much longer timeout: the stored one wins.
	res := mustRun(t, h.runner, h.request("hook", time.Hour))
	if res.State != store.HookTimedOut {
		t.Fatalf("result = %+v", res)
	}
	if h.polls != 0 {
		t.Errorf("polls = %d: the expired run must not wait again", h.polls)
	}
	if !reflect.DeepEqual(h.nomad.stopped, []string{child.JobID}) {
		t.Errorf("stopped = %v", h.nomad.stopped)
	}
	if len(h.nomad.dispatches) != 1 {
		t.Errorf("dispatches = %d: resuming must not dispatch again", len(h.nomad.dispatches))
	}
}

func TestRunTerminalRunTouchesNothing(t *testing.T) {
	for _, state := range []store.HookState{store.HookSucceeded, store.HookFailed, store.HookTimedOut} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			run, _, err := h.store.EnsureHookRun(ctx, h.depID, "pre", "hook", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.store.UpdateHookRun(ctx, run.ID, store.HookUpdate{State: state, DispatchedJobID: "hook/dispatch-9", Error: "stored reason"}); err != nil {
				t.Fatal(err)
			}

			res := mustRun(t, h.runner, h.request("hook", time.Minute))
			want := Result{State: state, DispatchedJobID: "hook/dispatch-9", Error: "stored reason"}
			if res != want {
				t.Errorf("result = %+v, want %+v", res, want)
			}
			if h.nomad.calls != 0 {
				t.Errorf("Nomad was called %d times for a finished run", h.nomad.calls)
			}
		})
	}
}

func TestRunResumesWithoutDispatching(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	ctx := context.Background()

	run, _, err := h.store.EnsureHookRun(ctx, h.depID, "pre", "hook", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	child, _ := h.nomad.Dispatch(ctx, "hook", nil, run.IdempotencyToken)
	if err := h.store.UpdateHookRun(ctx, run.ID, store.HookUpdate{State: store.HookRunning, DispatchedJobID: child.JobID}); err != nil {
		t.Fatal(err)
	}
	h.nomad.finish(child.JobID, "complete", "")

	res := mustRun(t, h.runner, h.request("hook", time.Minute))
	if res.State != store.HookSucceeded || res.DispatchedJobID != child.JobID {
		t.Fatalf("result = %+v", res)
	}
	if len(h.nomad.dispatches) != 1 {
		t.Errorf("dispatches = %d, want only the setup one", len(h.nomad.dispatches))
	}
}

// Crash between the dispatch and saving the child ID: the row is still
// "dispatching", the token makes the second dispatch return the same child.
func TestRunResumesFromDispatchingRow(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	ctx := context.Background()

	run, _, err := h.store.EnsureHookRun(ctx, h.depID, "pre", "hook", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := h.nomad.Dispatch(ctx, "hook", map[string]string{"nops_deployment_id": h.depID}, run.IdempotencyToken)
	h.nomad.finish(first.JobID, "complete", "")

	res := mustRun(t, h.runner, h.request("hook", time.Minute))
	if res.State != store.HookSucceeded || res.DispatchedJobID != first.JobID {
		t.Fatalf("result = %+v, want the child of the first dispatch %s", res, first.JobID)
	}
	if len(h.nomad.dispatches) != 2 || h.nomad.dispatches[1].Token != run.IdempotencyToken {
		t.Errorf("dispatches = %+v", h.nomad.dispatches)
	}
	if len(h.nomad.tokens) != 1 {
		t.Errorf("children created = %d, want 1", len(h.nomad.tokens))
	}
}

func TestRunExpiredBeforeDispatch(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	if _, _, err := h.store.EnsureHookRun(context.Background(), h.depID, "pre", "hook", time.Minute); err != nil {
		t.Fatal(err)
	}
	h.clk.advance(2 * time.Minute)

	res := mustRun(t, h.runner, h.request("hook", time.Minute))
	if res.State != store.HookTimedOut || !strings.Contains(res.Error, "before the hook was dispatched") {
		t.Fatalf("result = %+v", res)
	}
	if h.nomad.calls != 0 {
		t.Errorf("Nomad was called %d times", h.nomad.calls)
	}
}

func TestRunInvalidHook(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
		want  string
	}{
		{"hook job missing", func(h *harness) {}, "not found in Nomad"},
		{"not marked as hook", func(h *harness) {
			h.addHook("hook", []string{"nops_deployment_id"}, nil)
			h.nomad.jobs["hook"].Meta = nil
		}, `nops_role = "hook"`},
		{"not batch", func(h *harness) {
			h.addHook("hook", []string{"nops_deployment_id"}, nil)
			h.nomad.jobs["hook"].Type = ptr("service")
		}, "must be of type batch"},
		{"type unset", func(h *harness) {
			h.addHook("hook", []string{"nops_deployment_id"}, nil)
			h.nomad.jobs["hook"].Type = nil
		}, "must be of type batch"},
		{"not parameterized", func(h *harness) {
			h.addHook("hook", nil, nil)
			h.nomad.jobs["hook"].ParameterizedJob = nil
		}, "not parameterized"},
		{"required meta nops cannot provide", func(h *harness) {
			h.addHook("hook", []string{"nops_deployment_id", "nops_image_web"}, nil)
		}, "nops_image_web"},
		{"image collision", func(h *harness) {
			h.addHook("hook", []string{"nops_deployment_id"}, nil)
		}, "different images"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)
			req := h.request("hook", time.Minute)
			if tc.name == "image collision" {
				req.Target = targetJob("api", group("a", dockerTask("x-y", "i:1")), group("b", dockerTask("x_y", "i:2")))
			}
			res := mustRun(t, h.runner, req)
			if res.State != store.HookFailed || !strings.Contains(res.Error, tc.want) {
				t.Fatalf("result = %+v, want a failure containing %q", res, tc.want)
			}
			if len(h.nomad.dispatches) != 0 {
				t.Errorf("an invalid hook was dispatched: %+v", h.nomad.dispatches)
			}
			if run := h.hookRun(); run.State != store.HookFailed || run.Error != res.Error {
				t.Errorf("stored run = %+v", run)
			}
		})
	}
}

func TestRunChildDisappeared(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	h.onPoll = func(n int) { delete(h.nomad.jobs, "hook/dispatch-1") }

	res := mustRun(t, h.runner, h.request("hook", time.Minute))
	if res.State != store.HookFailed || !strings.Contains(res.Error, "disappeared") {
		t.Fatalf("result = %+v", res)
	}
	if len(h.nomad.dispatches) != 1 {
		t.Errorf("dispatches = %d: a vanished child must not be dispatched again", len(h.nomad.dispatches))
	}
}

func TestRunAllocationsGone(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	h.nomad.allocsErr = fmt.Errorf("allocations: %w", nomadx.ErrJobNotFound)

	res := mustRun(t, h.runner, h.request("hook", time.Minute))
	if res.State != store.HookFailed || !strings.Contains(res.Error, "disappeared") {
		t.Fatalf("result = %+v", res)
	}
}

// Nomad and SQLite failures are errors, not hook failures, and leave the run
// resumable.
func TestRunInfrastructureErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		setup     func(h *harness)
		wantState store.HookState
	}{
		{"reading the hook job", func(h *harness) { h.nomad.jobErr = boom }, store.HookDispatching},
		{"dispatch", func(h *harness) { h.nomad.dispatchErr = boom }, store.HookDispatching},
		{"reading the child", func(h *harness) {
			h.onPoll = nil
			h.nomad.jobErr = nil
			h.nomad.allocsErr = boom
		}, store.HookRunning},
		{"stopping on timeout", func(h *harness) {
			h.nomad.stopErr = boom
			h.nomad.allocs["hook/dispatch-1"] = []nomadx.Alloc{{ID: "a", ClientStatus: "running", DesiredStatus: "run"}}
		}, store.HookRunning},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.addHook("hook", []string{"nops_deployment_id"}, nil)
			tc.setup(h)

			_, err := h.runner.Run(context.Background(), h.request("hook", 4*time.Second))
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want boom", err)
			}
			if run := h.hookRun(); run.State != tc.wantState {
				t.Errorf("stored state = %s, want %s", run.State, tc.wantState)
			}
		})
	}
}

// flakyStore fails UpdateHookRun for a given target state, once.
type flakyStore struct {
	Store
	failState store.HookState
	failed    bool
}

func (f *flakyStore) UpdateHookRun(ctx context.Context, id string, u store.HookUpdate) error {
	if u.State == f.failState && !f.failed {
		f.failed = true
		return errors.New("disk full")
	}
	return f.Store.UpdateHookRun(ctx, id, u)
}

// The child is stopped, then saving "timed_out" fails. The retry finds the
// child stopped past the deadline and must still report a timeout, not a
// failure caused by "stopped from outside".
func TestRunTimeoutSurvivesFailedSave(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	h.nomad.allocs["hook/dispatch-1"] = []nomadx.Alloc{{ID: "a1", ClientStatus: "running", DesiredStatus: "run"}}

	flaky := &flakyStore{Store: h.store, failState: store.HookTimedOut}
	r := New(h.nomad, flaky, nil, testPoll, WithClock(h.clk.now), WithWait(func(_ context.Context, d time.Duration) error {
		h.clk.advance(d)
		return nil
	}))

	if _, err := r.Run(context.Background(), h.request("hook", 4*time.Second)); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("first run: err = %v, want the store error", err)
	}
	if len(h.nomad.stopped) != 1 {
		t.Fatalf("stopped = %v", h.nomad.stopped)
	}

	res, err := r.Run(context.Background(), h.request("hook", 4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != store.HookTimedOut {
		t.Fatalf("second run: %+v, want timed_out", res)
	}
}

func TestRunSaveErrorAfterDispatchKeepsRowResumable(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	flaky := &flakyStore{Store: h.store, failState: store.HookRunning}
	r := New(h.nomad, flaky, nil, testPoll, WithClock(h.clk.now), WithWait(func(_ context.Context, d time.Duration) error {
		h.clk.advance(d)
		h.nomad.finish("hook/dispatch-1", "complete", "")
		return nil
	}))

	if _, err := r.Run(context.Background(), h.request("hook", time.Minute)); err == nil {
		t.Fatal("want the store error")
	}
	if run := h.hookRun(); run.State != store.HookDispatching || run.DispatchedJobID != "" {
		t.Errorf("stored run = %+v, want an untouched dispatching row", run)
	}
	res, err := r.Run(context.Background(), h.request("hook", time.Minute))
	if err != nil || res.State != store.HookSucceeded || res.DispatchedJobID != "hook/dispatch-1" {
		t.Fatalf("resume: %+v, %v", res, err)
	}
	if len(h.nomad.tokens) != 1 {
		t.Errorf("children = %d, the token must dedupe the second dispatch", len(h.nomad.tokens))
	}
}

func TestRunCancelledContext(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	h.runner.wait = func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}

	_, err := h.runner.Run(ctx, h.request("hook", time.Minute))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if run := h.hookRun(); run.State != store.HookRunning {
		t.Errorf("stored state = %s, want running (resumable)", run.State)
	}
}

func TestRunNoTarget(t *testing.T) {
	h := newHarness(t)
	h.addHook("hook", []string{"nops_deployment_id"}, nil)
	req := h.request("hook", time.Minute)
	req.Target = nil
	if _, err := h.runner.Run(context.Background(), req); err == nil {
		t.Fatal("a missing target is a caller bug and must be an error")
	}
	if run := h.hookRun(); run.State != store.HookDispatching {
		t.Errorf("stored state = %s: a caller bug must not fail the hook", run.State)
	}
}

func TestRunInvalidRequest(t *testing.T) {
	h := newHarness(t)
	for name, mutate := range map[string]func(*Request){
		"no hook job":   func(r *Request) { r.HookJobID = "" },
		"short timeout": func(r *Request) { r.Timeout = 500 * time.Millisecond },
		"bad phase":     func(r *Request) { r.Phase = "during" },
	} {
		req := h.request("hook", time.Minute)
		mutate(&req)
		if _, err := h.runner.Run(context.Background(), req); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if h.nomad.calls != 0 {
		t.Errorf("Nomad was called %d times for invalid requests", h.nomad.calls)
	}
}

func TestNewDefaults(t *testing.T) {
	r := New(newFakeNomad(), nil, nil, 0)
	if r.poll != 2*time.Second || r.log == nil {
		t.Errorf("defaults: poll %s, log %v", r.poll, r.log)
	}
}

func TestSleep(t *testing.T) {
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleep: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled sleep: %v", err)
	}
}
