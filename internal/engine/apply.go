package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// ErrStaleApproval is returned by Approve when the caller's specHash no
// longer matches the deployment's current one: invariant 3, an approval is
// valid for (deployment_id, spec_hash) and never transfers to another spec.
var ErrStaleApproval = errors.New("approval no longer matches the deployment's current spec")

// RunApply runs the apply loop: every EngineInterval it picks up every
// non-terminal deployment and advances it by one step, one goroutine per
// deployment, until ctx is done. See docs/design/engine-apply.md.
func (e *Engine) RunApply(ctx context.Context) {
	e.applyCycle(ctx)
	ticker := time.NewTicker(e.engineInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.applyCycle(ctx)
		}
	}
}

// applyCycle starts one goroutine per active deployment not already being
// worked on. A deployment already in flight from a previous, still-running
// cycle is left alone: the in-process set (not a new store column) is enough
// to keep two overlapping ticks from starting the same deployment twice,
// since the DB's own per-job lock (invariant 6) is about which job has an
// active deployment, not which deployment is currently being stepped.
func (e *Engine) applyCycle(ctx context.Context) {
	active, err := e.store.ListActive(ctx)
	if err != nil {
		e.log.ErrorContext(ctx, "apply cycle: list active deployments", "error", err)
		return
	}
	for _, d := range active {
		if d.Namespace != e.namespace || !e.startApply(d.ID) {
			continue
		}
		go func(d *store.Deployment) {
			defer e.finishApply(d.ID)
			e.applyStep(ctx, d)
		}(d)
	}
}

func (e *Engine) startApply(id string) bool {
	e.applyMu.Lock()
	defer e.applyMu.Unlock()
	if _, ok := e.inFlight[id]; ok {
		return false
	}
	e.inFlight[id] = struct{}{}
	return true
}

func (e *Engine) finishApply(id string) {
	e.applyMu.Lock()
	defer e.applyMu.Unlock()
	delete(e.inFlight, id)
}

// applyStep advances one deployment by exactly one step, driven by its
// current state. A Nomad or SQLite error is logged and leaves the deployment
// where it is for the next cycle to retry (fail loud, retry); only a hook
// Result or a CAS conflict actually closes it. pending_approval is never
// reached here: the only way out of it is Approve or Reject.
func (e *Engine) applyStep(ctx context.Context, d *store.Deployment) {
	log := e.log.With("deployment_id", d.ID, "job", d.JobID, "namespace", d.Namespace)
	switch d.State {
	case store.StateDetected:
		e.stepDetected(ctx, log, d)
	case store.StatePreHook:
		e.stepHook(ctx, log, d, "pre")
	case store.StateApplying:
		e.stepApplying(ctx, log, d)
	case store.StatePostHook:
		e.stepHook(ctx, log, d, "post")
	}
}

// applyTransition wraps transition for apply's own steps: a store failure or
// an illegal/conflicting move is logged here and does not stop the caller.
func (e *Engine) applyTransition(ctx context.Context, log *slog.Logger, d *store.Deployment, to store.State, message string) {
	if err := e.transition(ctx, log, d, to, message); err != nil {
		log.ErrorContext(ctx, "transition", "to", to, "error", err)
	}
}

// parseJobSpec decodes the exact job nops will register, as stored at
// detection time (docs/design/engine-detection.md#job_spec-keeps-the-full-unredacted-spec).
func parseJobSpec(d *store.Deployment) (*api.Job, error) {
	var job api.Job
	if err := json.Unmarshal([]byte(d.JobSpec), &job); err != nil {
		return nil, fmt.Errorf("decode job spec of %s: %w", d.ID, err)
	}
	return &job, nil
}

// nextAfterDecision picks pre_hook or applying for a deployment about to
// start applying: pre_hook if the target spec declares nops_pre_hook, else
// applying straight away. Used both for an auto deployment leaving detected
// and for Approve leaving pending_approval.
func nextAfterDecision(job *api.Job) store.State {
	if meta.Parse(job.Meta).PreHook != nil {
		return store.StatePreHook
	}
	return store.StateApplying
}

func (e *Engine) stepDetected(ctx context.Context, log *slog.Logger, d *store.Deployment) {
	job, err := parseJobSpec(d)
	if err != nil {
		log.ErrorContext(ctx, "decode job spec", "error", err)
		return
	}
	e.applyTransition(ctx, log, d, nextAfterDecision(job), "")
}

// stepHook runs the deployment's hook for phase ("pre" or "post") and moves
// it on once the hook reaches a terminal state. The hook is the one the
// deployment froze at detection: its revision is registered here, right
// before dispatch, from that spec (nothing of it is in Nomad before the
// deployment is approved, and what runs is what was approved). An error from
// registering it or from Hooks.Run (Nomad or SQLite) is left for the next
// cycle to retry, and a registration that keeps failing for the hook's
// timeout fails the deployment, as an unreachable hook would.
func (e *Engine) stepHook(ctx context.Context, log *slog.Logger, d *store.Deployment, phase string) {
	job, err := parseJobSpec(d)
	if err != nil {
		log.ErrorContext(ctx, "decode job spec", "error", err)
		return
	}
	cfg := meta.Parse(job.Meta)
	timeout := meta.DefaultHookTimeout
	if h := cfg.PreHook; phase == "pre" && h != nil {
		timeout = h.Timeout
	} else if h := cfg.PostHook; phase == "post" && h != nil {
		timeout = h.Timeout
	}

	frozen, err := e.store.DeploymentHooks(ctx, d.ID)
	if err != nil {
		log.ErrorContext(ctx, "read frozen hooks", "phase", phase, "error", err)
		return
	}
	var hook *store.DeploymentHook
	for i := range frozen {
		if frozen[i].Phase == phase {
			hook = &frozen[i]
			break
		}
	}
	if hook == nil {
		// Detection freezes every hook a deployment declares and only routes
		// here for one that does: no row means a deployment made before hooks
		// were frozen. Nothing to run and nothing to wait for: fail it, and
		// the job's next deployment starts from a clean state.
		e.applyTransition(ctx, log, d, store.StateFailed,
			fmt.Sprintf("%s-hook: the deployment has no frozen hook (created by an older nops): push a new commit or retry it", phase))
		return
	}

	if err := e.registerRevision(ctx, log, *hook); err != nil {
		log.ErrorContext(ctx, "register hook revision", "phase", phase, "hook", hook.HookID, "revision", hook.Revision, "error", err)
		if e.now().Sub(d.UpdatedAt) >= timeout {
			e.applyTransition(ctx, log, d, store.StateFailed, hookFailure(phase,
				fmt.Sprintf("could not register hook %q within %s: %v", hook.HookID, timeout, err)))
		}
		return
	}

	res, err := e.hooks.Run(ctx, hooks.Request{
		DeploymentID: d.ID, Phase: phase, HookJobID: hook.Revision,
		Commit: d.CommitSHA, Timeout: timeout, Target: job,
	})
	if err != nil {
		log.ErrorContext(ctx, "run hook", "phase", phase, "error", err)
		return
	}
	switch res.State {
	case store.HookSucceeded:
		next := store.StateApplying
		if phase == "post" {
			next = store.StateCompleted
		}
		e.applyTransition(ctx, log, d, next, "")
	case store.HookFailed, store.HookTimedOut:
		e.applyTransition(ctx, log, d, store.StateFailed, hookFailure(phase, fmt.Sprintf("%s: %s", res.State, res.Error)))
	}
	// running/dispatching: Run only returns once terminal or on error, so
	// this case does not occur; nothing to do either way.
}

// hookFailure words why a deployment failed in a hook phase, and what that
// leaves behind: a pre-hook has not touched the job, a post-hook runs after it
// is live.
func hookFailure(phase, what string) string {
	reason := fmt.Sprintf("%s-hook %s", phase, what)
	if phase == "pre" {
		return reason + " (live job left untouched)"
	}
	return reason + " (apply already live, not undone)"
}

func (e *Engine) stepApplying(ctx context.Context, log *slog.Logger, d *store.Deployment) {
	if d.AppliedIndex == 0 {
		e.stepRegister(ctx, log, d)
		return
	}
	e.stepHealth(ctx, log, d)
}

// stepRegister performs (or recovers) the CAS register of a deployment
// entering applying, following invariant 1 (a fresh plan right before) and
// invariant 2 (CAS on the saved cas_index).
func (e *Engine) stepRegister(ctx context.Context, log *slog.Logger, d *store.Deployment) {
	job, err := parseJobSpec(d)
	if err != nil {
		log.ErrorContext(ctx, "decode job spec", "error", err)
		return
	}

	live, err := e.nomad.Job(ctx, d.JobID)
	var liveIndex uint64
	switch {
	case errors.Is(err, nomadx.ErrJobNotFound):
	case err != nil:
		log.ErrorContext(ctx, "get live job", "error", err)
		return
	default:
		liveIndex = derefUint64(live.JobModifyIndex)
	}

	var evalID string
	if liveIndex == d.CASIndex {
		plan, err := e.nomad.Plan(ctx, job)
		if err != nil {
			log.ErrorContext(ctx, "plan job", "error", err)
			return
		}
		if plan.Diff == nil || plan.Diff.Type == "None" {
			e.applyTransition(ctx, log, d, store.StateCompleted, "already in sync")
			return
		}
		res, err := e.nomad.RegisterCAS(ctx, job, d.CASIndex, false)
		if err != nil {
			if errors.Is(err, nomadx.ErrCASConflict) {
				e.applyTransition(ctx, log, d, store.StateFailed, "job modified outside nops: "+err.Error())
				return
			}
			log.ErrorContext(ctx, "register", "error", err)
			return
		}
		evalID = res.EvalID
	} else {
		// The live index moved since detection: either the register already
		// happened (a crash between it succeeding and the next step) or a
		// real conflict. A plan of our spec against the live job tells them
		// apart (state-machine.md#recovery-after-a-crash).
		plan, err := e.nomad.Plan(ctx, job)
		if err != nil {
			log.ErrorContext(ctx, "plan job", "error", err)
			return
		}
		if plan.Diff != nil && plan.Diff.Type != "None" {
			e.applyTransition(ctx, log, d, store.StateFailed, "conflict: live job changed and our spec no longer applies")
			return
		}
		// Already applied: evalID stays empty, nothing more to register.
	}

	applied, err := e.nomad.Job(ctx, d.JobID)
	if err != nil {
		log.ErrorContext(ctx, "re-read live job after register", "error", err)
		return
	}
	appliedIndex := derefUint64(applied.JobModifyIndex)
	if err := e.store.SetApplied(ctx, d.ID, appliedIndex, evalID); err != nil {
		log.ErrorContext(ctx, "record applied index", "error", err)
		return
	}
	log.InfoContext(ctx, "apply registered", "applied_index", appliedIndex)
}

// stepHealth waits for the applied job version to become healthy, bounded by
// ApplyTimeout counted from the "-> applying" event.
func (e *Engine) stepHealth(ctx context.Context, log *slog.Logger, d *store.Deployment) {
	since, err := e.store.AppliedSince(ctx, d.ID)
	if err != nil {
		log.ErrorContext(ctx, "read applied timestamp", "error", err)
		return
	}
	if e.now().After(since.Add(e.applyTimeout)) {
		e.applyTransition(ctx, log, d, store.StateFailed,
			fmt.Sprintf("apply did not become healthy within %s", e.applyTimeout))
		return
	}

	healthy, failed, reason, err := e.applyHealth(ctx, d)
	if err != nil {
		log.ErrorContext(ctx, "check apply health", "error", err)
		return // retried next cycle, still bounded by the timeout above
	}
	switch {
	case failed:
		e.applyTransition(ctx, log, d, store.StateFailed, "apply did not become healthy: "+reason)
	case healthy:
		job, err := parseJobSpec(d)
		if err != nil {
			log.ErrorContext(ctx, "decode job spec", "error", err)
			return
		}
		next := store.StateCompleted
		if meta.Parse(job.Meta).PostHook != nil {
			next = store.StatePostHook
		}
		e.applyTransition(ctx, log, d, next, "")
	default:
		// still waiting: nothing to do this cycle.
	}
}

// applyHealth decides whether the job version nops applied is healthy: via
// its Nomad deployment when there is one tracking exactly that apply, else
// via the allocations of that job version (a batch job, or an update stanza
// that produces no Nomad deployment). Neither healthy nor failed means still
// waiting.
func (e *Engine) applyHealth(ctx context.Context, d *store.Deployment) (healthy, failed bool, reason string, err error) {
	dep, err := e.nomad.LatestDeployment(ctx, d.JobID)
	if err != nil {
		return false, false, "", err
	}
	if dep != nil && dep.JobModifyIndex == d.AppliedIndex {
		switch dep.Status {
		case api.DeploymentStatusSuccessful:
			return true, false, "", nil
		case api.DeploymentStatusFailed, api.DeploymentStatusCancelled:
			return false, true, fmt.Sprintf("nomad deployment %s: %s", dep.Status, dep.StatusDescription), nil
		default:
			return false, false, "", nil
		}
	}

	live, err := e.nomad.Job(ctx, d.JobID)
	if err != nil {
		return false, false, "", err
	}
	// The allocations of the live version are ours only while the live job is
	// still the one nops registered: after an outside edit (for instance while
	// nops was down) they belong to someone else's spec.
	if liveIndex := derefUint64(live.JobModifyIndex); liveIndex != d.AppliedIndex {
		return false, true, fmt.Sprintf("job modified outside nops while waiting for health (live index %d, applied %d)",
			liveIndex, d.AppliedIndex), nil
	}
	version := derefUint64(live.Version)
	allocs, err := e.nomad.Allocations(ctx, d.JobID)
	if err != nil {
		return false, false, "", err
	}
	var ours []nomadx.Alloc
	for _, a := range allocs {
		if a.JobVersion == version {
			ours = append(ours, a)
		}
	}
	if len(ours) == 0 {
		return false, false, "", nil // scheduling not done yet
	}
	for _, a := range ours {
		if a.ClientStatus == "failed" || a.ClientStatus == "lost" {
			return false, true, a.Failure, nil
		}
	}
	for _, a := range ours {
		if a.ClientStatus != "running" && a.ClientStatus != "complete" {
			return false, false, "", nil // still starting
		}
	}
	return true, false, "", nil
}

// Approve moves a pending_approval deployment forward: pre_hook if the
// target spec declares nops_pre_hook, else applying. It refuses without
// touching Nomad or the store if specHash no longer matches the deployment's
// current one (invariant 3).
func (e *Engine) Approve(ctx context.Context, id, specHash, actor string) error {
	if actor == "" {
		return errors.New("approve: actor is required")
	}
	d, err := e.store.GetDeployment(ctx, id)
	if err != nil {
		return fmt.Errorf("approve %s: %w", id, err)
	}
	if d.State != store.StatePendingApproval {
		return fmt.Errorf("approve %s: not pending approval (state is %s)", id, d.State)
	}
	if d.SpecHash != specHash {
		return fmt.Errorf("approve %s: %w", id, ErrStaleApproval)
	}
	job, err := parseJobSpec(d)
	if err != nil {
		return fmt.Errorf("approve %s: %w", id, err)
	}
	next := nextAfterDecision(job)
	t := store.Transition{From: d.State, Actor: actor, Message: "approved", DecidedBy: actor}
	if err := e.store.Transition(ctx, id, next, t); err != nil {
		return fmt.Errorf("approve %s: %w", id, err)
	}
	e.log.InfoContext(ctx, "deployment approved", "deployment_id", id, "actor", actor, "to", next)
	return nil
}

// Reject moves a pending_approval deployment to rejected.
func (e *Engine) Reject(ctx context.Context, id, actor string) error {
	if actor == "" {
		return errors.New("reject: actor is required")
	}
	d, err := e.store.GetDeployment(ctx, id)
	if err != nil {
		return fmt.Errorf("reject %s: %w", id, err)
	}
	if d.State != store.StatePendingApproval {
		return fmt.Errorf("reject %s: not pending approval (state is %s)", id, d.State)
	}
	t := store.Transition{From: d.State, Actor: actor, Message: "rejected", DecidedBy: actor}
	if err := e.store.Transition(ctx, id, store.StateRejected, t); err != nil {
		return fmt.Errorf("reject %s: %w", id, err)
	}
	e.log.InfoContext(ctx, "deployment rejected", "deployment_id", id, "actor", actor)
	return nil
}
