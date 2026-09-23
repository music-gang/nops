// Package hooks runs the pre and post deployment hooks: it dispatches the hook
// job, waits for its outcome, enforces the timeout and records everything in
// the store's hook_runs table.
//
// Run is idempotent and resumable. Every step is persisted before the next one
// and re-reading the store tells where a crashed run stopped, so the engine
// (and the recovery at startup) simply call Run again.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// Nomad is what the runner needs from Nomad. *nomadx.Client implements it.
type Nomad interface {
	Job(ctx context.Context, id string) (*api.Job, error)
	Dispatch(ctx context.Context, parentID string, meta map[string]string, idempotencyToken string) (*nomadx.DispatchResult, error)
	FindDispatched(ctx context.Context, parentID, idempotencyToken string) (string, error)
	Allocations(ctx context.Context, jobID string) ([]nomadx.Alloc, error)
	StopJob(ctx context.Context, id string) error
}

// Store is what the runner needs from the store. *store.Store implements it.
type Store interface {
	EnsureHookRun(ctx context.Context, deploymentID, phase, hookJobID string, timeout time.Duration) (run *store.HookRun, created bool, err error)
	UpdateHookRun(ctx context.Context, runID string, u store.HookUpdate) error
}

// Request describes one hook run.
type Request struct {
	DeploymentID string
	// Phase is "pre" or "post".
	Phase string
	// HookJobID is the parameterized hook job declared in nops_pre_hook/nops_post_hook.
	HookJobID string
	// Commit is the git commit that produced the deployment.
	Commit string
	// Timeout is counted from the moment the run was first recorded. It must be
	// positive; the store keeps whole seconds, so a sub-second part is rounded up.
	Timeout time.Duration
	// Target is the job spec being deployed (the new version): the source of
	// the nops_image_<task> meta.
	Target *api.Job
}

// Result is the terminal outcome of a hook run. A hook that failed or timed
// out is a Result, not an error: errors are reserved for Nomad or SQLite
// failing, where the caller should retry.
type Result struct {
	// State is succeeded, failed or timed_out.
	State           store.HookState
	DispatchedJobID string
	// Error explains a failed or timed out run; empty on success.
	Error string
}

// Runner runs hooks. Create it with New.
type Runner struct {
	nomad Nomad
	store Store
	log   *slog.Logger
	poll  time.Duration
	now   func() time.Time
	wait  func(ctx context.Context, d time.Duration) error
}

// Option configures New.
type Option func(*Runner)

// WithClock injects the time source (tests). It must be the same clock the
// store uses, since the timeout is counted from the store's started_at.
func WithClock(now func() time.Time) Option { return func(r *Runner) { r.now = now } }

// WithWait injects how the runner sleeps between polls (tests).
func WithWait(wait func(ctx context.Context, d time.Duration) error) Option {
	return func(r *Runner) { r.wait = wait }
}

// New creates a Runner that polls Nomad every poll (a non-positive value falls
// back to two seconds).
func New(n Nomad, s Store, log *slog.Logger, poll time.Duration, opts ...Option) *Runner {
	if log == nil {
		log = slog.Default()
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	r := &Runner{nomad: n, store: s, log: log, poll: poll, now: time.Now, wait: sleep}
	for _, o := range opts {
		o(r)
	}
	return r
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run drives the hook to a terminal state and blocks until it gets there. If
// ctx is cancelled it returns ctx.Err() and leaves the run as it is, to be
// resumed by calling Run again with the same request.
//
// A run that is already terminal returns its stored result without touching
// Nomad. The hook job, timeout and start time recorded in the store win over
// the request on a resume.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	if req.HookJobID == "" {
		return Result{}, errors.New("run hook: HookJobID is required")
	}
	if req.Timeout <= 0 {
		return Result{}, fmt.Errorf("run hook: timeout %s must be positive", req.Timeout)
	}
	// Whole seconds, rounded up: the stored deadline must never be earlier than asked.
	timeout := (req.Timeout + time.Second - 1) / time.Second * time.Second
	run, _, err := r.store.EnsureHookRun(ctx, req.DeploymentID, req.Phase, req.HookJobID, timeout)
	if err != nil {
		return Result{}, err
	}
	log := r.log.With("deployment_id", run.DeploymentID, "phase", run.Phase, "hook_job", run.HookJobID)

	switch run.State {
	case store.HookSucceeded, store.HookFailed, store.HookTimedOut:
		return resultOf(run), nil
	}
	deadline := run.StartedAt.Add(run.Timeout)

	if run.DispatchedJobID == "" {
		var (
			res  Result
			done bool
		)
		if run.State == store.HookDispatching {
			res, done, err = r.dispatch(ctx, log, req, run, deadline)
		} else {
			// running without a child ID: the dispatch may have been sent.
			res, done, err = r.recoverChild(ctx, log, run)
		}
		if err != nil || done {
			return res, err
		}
	}
	return r.await(ctx, log, run, deadline)
}

func resultOf(run *store.HookRun) Result {
	return Result{State: run.State, DispatchedJobID: run.DispatchedJobID, Error: run.Error}
}

// finish records a terminal state.
func (r *Runner) finish(ctx context.Context, log *slog.Logger, run *store.HookRun, state store.HookState, msg string) (Result, error) {
	if err := r.store.UpdateHookRun(ctx, run.ID, store.HookUpdate{State: state, Error: msg}); err != nil {
		return Result{}, err
	}
	run.State, run.Error = state, msg
	if state == store.HookSucceeded {
		log.Info("hook succeeded", "hook_job_run", run.DispatchedJobID)
	} else {
		log.Warn("hook did not succeed", "state", string(state), "hook_job_run", run.DispatchedJobID, "error", msg)
	}
	return resultOf(run), nil
}

// dispatch validates the hook job and dispatches it, for a run that has not
// sent anything to Nomad yet (state dispatching). done is true when the run
// ended here (invalid hook, or timeout already expired: nothing was sent, so
// there is no child to stop).
//
// The run is saved as running BEFORE the dispatch, so a crash after Nomad
// accepted it is recognisable on resume (see recoverChild). Saving the child ID
// comes right after.
func (r *Runner) dispatch(ctx context.Context, log *slog.Logger, req Request, run *store.HookRun, deadline time.Time) (res Result, done bool, err error) {
	if !r.now().Before(deadline) {
		res, err = r.finish(ctx, log, run, store.HookTimedOut, fmt.Sprintf("timed out after %s before the hook was dispatched", run.Timeout))
		return res, true, err
	}
	fail := func(format string, args ...any) (Result, bool, error) {
		res, err := r.finish(ctx, log, run, store.HookFailed, fmt.Sprintf(format, args...))
		return res, true, err
	}

	parent, err := r.nomad.Job(ctx, run.HookJobID)
	if errors.Is(err, nomadx.ErrJobNotFound) {
		return fail("hook job %q not found in Nomad: it must be in the repo and synced before the deployment", run.HookJobID)
	}
	if err != nil {
		return Result{}, false, err
	}
	if !meta.Parse(parent.Meta).IsHook {
		return fail("job %q is not a hook: it must set meta nops_role = %q", run.HookJobID, "hook")
	}
	if parent.Type == nil || *parent.Type != api.JobTypeBatch {
		return fail("hook job %q must be of type batch", run.HookJobID)
	}
	all, err := BuildMeta(run.DeploymentID, req.Commit, run.Phase, req.Target)
	if err != nil {
		if req.Target == nil {
			return Result{}, false, err // a caller bug, not a bad hook
		}
		return fail("hook job %q: %v", run.HookJobID, err)
	}
	dm, err := DispatchMeta(parent, all)
	if err != nil {
		return fail("hook job %q: %v", run.HookJobID, err)
	}

	if err := r.store.UpdateHookRun(ctx, run.ID, store.HookUpdate{State: store.HookRunning}); err != nil {
		return Result{}, false, err
	}
	run.State = store.HookRunning
	child, err := r.nomad.Dispatch(ctx, run.HookJobID, dm, run.IdempotencyToken)
	if err != nil {
		return Result{}, false, err
	}
	if err := r.saveChild(ctx, log, run, child.JobID); err != nil {
		return Result{}, false, err
	}
	return Result{}, false, nil
}

func (r *Runner) saveChild(ctx context.Context, log *slog.Logger, run *store.HookRun, childID string) error {
	if err := r.store.UpdateHookRun(ctx, run.ID, store.HookUpdate{State: store.HookRunning, DispatchedJobID: childID}); err != nil {
		return err
	}
	run.DispatchedJobID = childID
	log.Info("hook dispatched", "hook_job_run", childID)
	return nil
}

// recoverChild resumes a run that is running but has no child ID: the dispatch
// may have reached Nomad before a crash (or a failed save) lost the ID. The
// idempotency token finds the child if it still exists, and the run carries on
// with it, timeout included. If there is none, the outcome cannot be known: the
// dispatch may never have been sent, or the child may have run and been
// garbage-collected. Dispatching again could run the hook twice, so the run
// fails instead.
func (r *Runner) recoverChild(ctx context.Context, log *slog.Logger, run *store.HookRun) (Result, bool, error) {
	id, err := r.nomad.FindDispatched(ctx, run.HookJobID, run.IdempotencyToken)
	if err != nil {
		return Result{}, false, err
	}
	if id == "" {
		res, err := r.finish(ctx, log, run, store.HookFailed, fmt.Sprintf(
			"the dispatch of hook job %q may have been sent before nops stopped, but no run with its token exists in Nomad (never created, or garbage-collected): the outcome is unknown, so it is not dispatched again; check %s/dispatch-* in Nomad",
			run.HookJobID, run.HookJobID))
		return res, true, err
	}
	if err := r.saveChild(ctx, log, run, id); err != nil {
		return Result{}, false, err
	}
	return Result{}, false, nil
}

// await polls the dispatched child until it has an outcome or the deadline passes.
func (r *Runner) await(ctx context.Context, log *slog.Logger, run *store.HookRun, deadline time.Time) (Result, error) {
	gone := func() (Result, error) {
		return r.finish(ctx, log, run, store.HookFailed, fmt.Sprintf(
			"hook run %s disappeared from Nomad (garbage-collected or purged) before nops saw its outcome; the outcome is unknown, so it is not dispatched again",
			run.DispatchedJobID))
	}
	for {
		child, err := r.nomad.Job(ctx, run.DispatchedJobID)
		if errors.Is(err, nomadx.ErrJobNotFound) {
			return gone()
		}
		if err != nil {
			return Result{}, err
		}
		allocs, err := r.nomad.Allocations(ctx, run.DispatchedJobID)
		if errors.Is(err, nomadx.ErrJobNotFound) {
			return gone()
		}
		if err != nil {
			return Result{}, err
		}

		expired := !r.now().Before(deadline)
		v, msg := evaluate(child, allocs)
		switch {
		case v == verdictSucceeded:
			return r.finish(ctx, log, run, store.HookSucceeded, "")
		case v == verdictFailed:
			return r.finish(ctx, log, run, store.HookFailed, msg)
		case v == verdictStopped && !expired:
			return r.finish(ctx, log, run, store.HookFailed, msg)
		case expired:
			// A stopped verdict past the deadline is our own stop from an earlier
			// attempt whose result could not be saved: stopping again is harmless.
			if err := r.nomad.StopJob(ctx, run.DispatchedJobID); err != nil {
				return Result{}, err
			}
			return r.finish(ctx, log, run, store.HookTimedOut, fmt.Sprintf(
				"timed out after %s: hook run %s was stopped", run.Timeout, run.DispatchedJobID))
		}
		if err := r.wait(ctx, r.poll); err != nil {
			return Result{}, err
		}
	}
}
