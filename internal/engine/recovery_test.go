package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// -- helpers ----------------------------------------------------------------

// restart simulates a process restart: a fresh Engine (empty in-flight set,
// empty caches) over the same store, fake Nomad and clock, with hooks as the
// hook runner. It replaces h.engine.
func (h *harness) restart(hk Hooks) *Engine {
	h.t.Helper()
	e := New(Options{
		Store: h.store, Nomad: h.nomad, Snapshots: h.snap, Notifier: h.notifier, Hooks: hk,
		Namespace: testNamespace, DriftInterval: time.Hour, EngineInterval: time.Hour,
		ApplyTimeout: h.engine.applyTimeout,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	e.now = h.clock.Now
	h.engine = e
	return e
}

// settle waits until no deployment is being stepped by e.
func settle(t *testing.T, e *Engine) {
	t.Helper()
	waitUntil(t, func() bool {
		e.applyMu.Lock()
		defer e.applyMu.Unlock()
		return len(e.inFlight) == 0
	}, "apply steps did not finish")
}

// newDeployment creates a detected deployment for job, as detection would.
func (h *harness) newDeployment(jobID string, job *api.Job, policy store.Policy, casIndex uint64) *store.Deployment {
	h.t.Helper()
	d := &store.Deployment{
		JobID: jobID, Namespace: testNamespace, CommitSHA: "c1", SpecHash: "hash-" + jobID,
		JobSpec: mustMarshal(h.t, job), Hooks: frozenFromSpec(mustMarshal(h.t, job)), Policy: policy, CASIndex: casIndex,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		h.t.Fatalf("CreateDeployment: %v", err)
	}
	return h.get(d.ID)
}

func (h *harness) move(d *store.Deployment, to store.State) *store.Deployment {
	h.t.Helper()
	if err := h.store.Transition(context.Background(), d.ID, to,
		store.Transition{From: d.State, Actor: "nops"}); err != nil {
		h.t.Fatalf("Transition %s -> %s: %v", d.State, to, err)
	}
	return h.get(d.ID)
}

// -- the first RunApply cycle is the recovery pass ---------------------------

// Every case leaves the store and Nomad as a crash would, restarts the engine
// and checks where the very first RunApply cycle takes the deployment: the
// interval is an hour, so no ticker fires during the test.
func TestRecoveryAfterRestart(t *testing.T) {
	preHookJob := func() *api.Job {
		return managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"})
	}
	postHookJob := func() *api.Job {
		return managed("web", "auto", map[string]string{"nops_post_hook": "web-smoke"})
	}
	drift := &api.JobDiff{Type: "Edited", ID: "web"}

	tests := []struct {
		name string
		// setup leaves the crash's state behind and returns the deployment.
		setup func(h *harness) *store.Deployment
		// done reports whether the recovery pass has taken the deployment where expected.
		done func(d *store.Deployment) bool
		// verify checks the side effects once done holds.
		verify func(t *testing.T, h *harness, d *store.Deployment)
	}{
		{
			name: "detected auto goes to applying",
			setup: func(h *harness) *store.Deployment {
				return h.newDeployment("web", managed("web", "auto", nil), store.PolicyAuto, 0)
			},
			done: func(d *store.Deployment) bool { return d.State == store.StateApplying },
		},
		{
			name: "detected auto with a pre-hook goes to pre_hook",
			setup: func(h *harness) *store.Deployment {
				return h.newDeployment("web", preHookJob(), store.PolicyAuto, 0)
			},
			done: func(d *store.Deployment) bool { return d.State == store.StatePreHook },
		},
		{
			name: "pre_hook resumed, hook succeeded meanwhile",
			setup: func(h *harness) *store.Deployment {
				d := h.newDeployment("web", preHookJob(), store.PolicyAuto, 0)
				return h.move(d, store.StatePreHook)
			},
			done: func(d *store.Deployment) bool { return d.State == store.StateApplying },
			verify: func(t *testing.T, h *harness, d *store.Deployment) {
				if calls := h.hooks.calls; len(calls) != 1 || calls[0].Phase != "pre" || calls[0].HookJobID != revisionOfPlainHook("web-migrate") {
					t.Errorf("hook calls = %+v, want one pre run of web-migrate", calls)
				}
			},
		},
		{
			name: "crash before the register: registers with the saved cas_index",
			setup: func(h *harness) *store.Deployment {
				h.nomad.setDrift("web", drift)
				return h.createApplying("web", managed("web", "auto", nil), 0)
			},
			done: func(d *store.Deployment) bool { return d.AppliedIndex != 0 },
			verify: func(t *testing.T, h *harness, d *store.Deployment) {
				if len(h.nomad.registerCalls) != 1 || h.nomad.registerCalls[0].index != 0 {
					t.Errorf("register calls = %+v, want exactly one at index 0", h.nomad.registerCalls)
				}
				if d.EvalID == "" {
					t.Error("eval_id not recorded for a fresh register")
				}
			},
		},
		{
			name: "crash after the register, before applied_index: no second register",
			setup: func(h *harness) *store.Deployment {
				d := h.createApplying("web", managed("web", "auto", nil), 0)
				live := managed("web", "auto", nil)
				idx := uint64(1)
				live.JobModifyIndex = &idx
				h.nomad.setLive(live) // our spec is live; no drift fixture: the plan is empty
				return d
			},
			done: func(d *store.Deployment) bool { return d.AppliedIndex == 1 },
			verify: func(t *testing.T, h *harness, d *store.Deployment) {
				if len(h.nomad.registerCalls) != 0 {
					t.Errorf("register calls = %+v, want none", h.nomad.registerCalls)
				}
				if d.EvalID != "" {
					t.Errorf("eval_id = %q, want empty: there is no eval to link", d.EvalID)
				}
			},
		},
		{
			name: "job modified outside nops while down: conflict found only after the restart",
			setup: func(h *harness) *store.Deployment {
				d := h.createApplying("web", managed("web", "auto", nil), 0)
				live := managed("web", "auto", nil)
				idx := uint64(4)
				live.JobModifyIndex = &idx
				h.nomad.setLive(live)
				h.nomad.setDrift("web", drift)
				return d
			},
			done: func(d *store.Deployment) bool { return d.State == store.StateFailed },
			verify: func(t *testing.T, h *harness, d *store.Deployment) {
				if len(h.nomad.registerCalls) != 0 {
					t.Errorf("register calls = %+v, want none", h.nomad.registerCalls)
				}
			},
		},
		{
			name: "CAS conflict on the resumed register",
			setup: func(h *harness) *store.Deployment {
				h.nomad.setDrift("web", drift)
				h.nomad.registerErr["web"] = errNomadCASConflict()
				return h.createApplying("web", managed("web", "auto", nil), 0)
			},
			done: func(d *store.Deployment) bool { return d.State == store.StateFailed },
		},
		{
			name: "applied while down and already healthy",
			setup: func(h *harness) *store.Deployment {
				d := h.applyingWithIndex("web", managed("web", "auto", nil), 9)
				h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusSuccessful})
				return d
			},
			done: func(d *store.Deployment) bool { return d.State == store.StateCompleted },
		},
		{
			name: "applied, apply timeout expired while down",
			setup: func(h *harness) *store.Deployment {
				h.engine.applyTimeout = time.Minute
				d := h.applyingWithIndex("web", managed("web", "auto", nil), 9)
				h.clock.Advance(time.Hour) // nops was down
				return d
			},
			done: func(d *store.Deployment) bool { return d.State == store.StateFailed },
		},
		{
			name: "applied, live job modified outside nops while down",
			setup: func(h *harness) *store.Deployment {
				job := managed("web", "auto", nil)
				d := h.applyingWithIndex("web", job, 9)
				h.liveApplied("web", job, 12, 4)
				h.nomad.setAllocs("web", nomadx.Alloc{ID: "a1", JobVersion: 4, ClientStatus: "running"})
				return d
			},
			done: func(d *store.Deployment) bool { return d.State == store.StateFailed },
			verify: func(t *testing.T, h *harness, d *store.Deployment) {
				h.notifier.waitFor(t, 1)
			},
		},
		{
			name: "post_hook resumed, hook succeeded meanwhile",
			setup: func(h *harness) *store.Deployment {
				d := h.applyingWithIndex("web", postHookJob(), 9)
				return h.move(d, store.StatePostHook)
			},
			done: func(d *store.Deployment) bool { return d.State == store.StateCompleted },
			verify: func(t *testing.T, h *harness, d *store.Deployment) {
				if calls := h.hooks.calls; len(calls) != 1 || calls[0].Phase != "post" || calls[0].HookJobID != revisionOfPlainHook("web-smoke") {
					t.Errorf("hook calls = %+v, want one post run of web-smoke", calls)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			d := tt.setup(h)

			e := h.restart(h.hooks)
			stop := runApplyInBackground(t, e)
			defer stop()
			waitUntil(t, func() bool { return tt.done(h.get(d.ID)) },
				"the first RunApply cycle did not take the deployment where expected")
			settle(t, e)

			if tt.verify != nil {
				tt.verify(t, h, h.get(d.ID))
			}
		})
	}
}

// pending_approval needs a human: a restart neither approves it nor touches
// Nomad for it, and detected/hook state of other deployments does not change that.
func TestRecoveryLeavesPendingApprovalAlone(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "approval", nil)
	d := h.pendingApproval("web", job, "h1")

	e := h.restart(h.hooks)
	e.applyCycle(context.Background())
	settle(t, e)

	if got := h.get(d.ID); got.State != store.StatePendingApproval {
		t.Fatalf("state = %s, want still pending_approval", got.State)
	}
	if len(h.nomad.registerCalls) != 0 || h.nomad.callCount() != 0 || len(h.hooks.calls) != 0 {
		t.Errorf("Nomad or hooks touched: register=%+v calls=%d hooks=%+v",
			h.nomad.registerCalls, h.nomad.callCount(), h.hooks.calls)
	}
}

// A deployment from another namespace is not this engine's to resume.
func TestRecoveryIgnoresOtherNamespaces(t *testing.T) {
	h := newHarness(t)
	d := &store.Deployment{
		JobID: "web", Namespace: "other", CommitSHA: "c1", SpecHash: "h1",
		JobSpec: mustMarshal(t, managed("web", "auto", nil)), Policy: store.PolicyAuto,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	e := h.restart(h.hooks)
	e.applyCycle(context.Background())
	settle(t, e)

	if got := h.get(d.ID); got.State != store.StateDetected {
		t.Fatalf("state = %s, want untouched detected", got.State)
	}
}

// -- crash between Dispatch and the saved dispatched job ID -------------------

// fakeHookNomad is the part of Nomad a hooks.Runner uses, with one hook child
// that Nomad accepted (and finished) before nops crashed.
type fakeHookNomad struct {
	mu          sync.Mutex
	child       string // ID of the dispatched child, "" if the dispatch never reached Nomad
	token       string // idempotency token the child was dispatched with
	dispatches  int
	childStatus string // client status of the child's allocation
}

func (f *fakeHookNomad) Job(_ context.Context, id string) (*api.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.child != "" && id == f.child {
		status := "dead"
		return &api.Job{ID: &id, Status: &status}, nil
	}
	return nil, fmt.Errorf("job %s: %w", id, nomadx.ErrJobNotFound)
}

func (f *fakeHookNomad) Dispatch(context.Context, string, map[string]string, string) (*nomadx.DispatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatches++
	return nil, fmt.Errorf("unexpected dispatch after the crash")
}

func (f *fakeHookNomad) FindDispatched(_ context.Context, _, token string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.child != "" && token == f.token {
		return f.child, nil
	}
	return "", nil
}

func (f *fakeHookNomad) Allocations(_ context.Context, jobID string) ([]nomadx.Alloc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if jobID != f.child {
		return nil, fmt.Errorf("job %s: %w", jobID, nomadx.ErrJobNotFound)
	}
	return []nomadx.Alloc{{ID: "alloc-1", ClientStatus: f.childStatus, DesiredStatus: "run"}}, nil
}

func (f *fakeHookNomad) StopJob(context.Context, string) error { return nil }

// A pre-hook whose run was saved as running, but whose dispatched job ID never
// made it to the store: the real hooks.Runner adopts the child through the
// idempotency token (never dispatching twice) and the deployment moves on.
func TestRecoveryAdoptsHookDispatchedBeforeCrash(t *testing.T) {
	for _, tt := range []struct {
		name      string
		child     string // "" = the dispatch never reached Nomad, or its child was garbage-collected
		wantState store.State
	}{
		{"child found by its token", "web-migrate/dispatch-1", store.StateApplying},
		{"no child: outcome unknown, never dispatched again", "", store.StateFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDeployment("web", managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"}), store.PolicyAuto, 0)
			d = h.move(d, store.StatePreHook)

			// What the crash left: the hook run is running, with no child ID.
			ctx := context.Background()
			run, _, err := h.store.EnsureHookRun(ctx, d.ID, "pre", 0, "web-migrate", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.store.UpdateHookRun(ctx, run.ID, store.HookUpdate{State: store.HookRunning}); err != nil {
				t.Fatal(err)
			}
			hn := &fakeHookNomad{child: tt.child, token: run.IdempotencyToken, childStatus: "complete"}
			runner := hooks.New(hn, h.store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond,
				hooks.WithClock(h.clock.Now))

			e := h.restart(runner)
			stop := runApplyInBackground(t, e)
			defer stop()
			waitUntil(t, func() bool { return h.get(d.ID).State == tt.wantState },
				"the first RunApply cycle did not resolve the hook")
			settle(t, e)

			if hn.dispatches != 0 {
				t.Errorf("dispatches = %d, want none: the crash window must never dispatch again", hn.dispatches)
			}
			got, err := h.store.GetHookRun(ctx, d.ID, "pre", 0)
			if err != nil {
				t.Fatal(err)
			}
			if tt.child != "" && (got.State != store.HookSucceeded || got.DispatchedJobID != tt.child) {
				t.Errorf("hook run = %+v, want succeeded on the adopted child %s", got, tt.child)
			}
			if tt.child == "" && got.State != store.HookFailed {
				t.Errorf("hook run state = %s, want failed", got.State)
			}
		})
	}
}
