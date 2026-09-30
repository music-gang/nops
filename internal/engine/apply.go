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

// ErrNotInRepo is returned by Approve when the deployment's job is not among the
// managed jobs of the last detection cycle: it was removed from git, or its file
// does not parse (or has not been read yet, right after a start). Approving it
// would register a spec git no longer asks for.
var ErrNotInRepo = errors.New("the job is not in the repository as of the last detection cycle")

// RunApply runs the apply loop: every EngineInterval it picks up every
// non-terminal deployment and advances it by one step, one goroutine per
// deployment, until ctx is done. See docs/archive/engine-apply.md.
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
		if !e.managedNS[d.Namespace] || !e.startApply(d.ID) {
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
	log := e.jobLog(d).With("deployment_id", d.ID)
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

// jobLog is the logger of a deployment's job. It has no deployment_id:
// transition adds it to its own lines, so a logger that already carried it would
// put the key twice in the line.
func (e *Engine) jobLog(d *store.Deployment) *slog.Logger {
	return e.log.With("job", d.JobID, "namespace", d.Namespace)
}

// applyTransition wraps transition for apply's own steps: a store failure or
// an illegal/conflicting move is logged here (with log, the step's, which has
// the deployment_id) and does not stop the caller. A deployment that ends here
// (completed or failed) asks for a detection cycle: what the dashboard says
// about the job (in sync, blocked) comes from the last cycle, which is from
// before the apply.
func (e *Engine) applyTransition(ctx context.Context, log *slog.Logger, d *store.Deployment, to store.State, message string) {
	if err := e.transition(ctx, e.jobLog(d), d, to, message); err != nil {
		log.ErrorContext(ctx, "transition", "to", to, "error", err)
		return
	}
	if to == store.StateCompleted || to == store.StateFailed {
		e.kickDetection()
	}
}

// parseJobSpec decodes the exact job nops will register, as stored at
// detection time (docs/archive/engine-detection.md#job_spec-keeps-the-full-unredacted-spec).
//
// The job goes where its deployment says: the deployment's namespace is the
// one its CAS index was read in, and Plan and RegisterCAS act on the job's own.
func parseJobSpec(d *store.Deployment) (*api.Job, error) {
	var job api.Job
	if err := json.Unmarshal([]byte(d.JobSpec), &job); err != nil {
		return nil, fmt.Errorf("decode job spec of %s: %w", d.ID, err)
	}
	job.Namespace = &d.Namespace
	return &job, nil
}

// nextAfterDecision picks pre_hook or applying for a deployment about to
// start applying: pre_hook if the target spec declares any nops_pre_hook, else
// applying straight away. Used both for an auto deployment leaving detected
// and for Approve leaving pending_approval.
func nextAfterDecision(job *api.Job) store.State {
	if len(meta.Parse(job.Meta).PreHooks) > 0 {
		return store.StatePreHook
	}
	return store.StateApplying
}

func (e *Engine) stepDetected(ctx context.Context, log *slog.Logger, d *store.Deployment) {
	if d.Policy != store.PolicyAuto {
		// Invariant 3: only Approve moves a deployment under approval on.
		// Detection creates one pending_approval, so this is a deployment an
		// older nops left `detected` (it moved it in a second write): detection
		// puts it right (see reconcileDeployment), and is asked to do it now.
		log.DebugContext(ctx, "detected deployment under approval left to detection")
		e.kickDetection()
		return
	}
	job, err := parseJobSpec(d)
	if err != nil {
		log.ErrorContext(ctx, "decode job spec", "error", err)
		return
	}
	// A hold gates the start: a deployment that was `detected` before the job was
	// paused, or before its sync window closed, is not advanced. Detection puts
	// it aside (see reconcileDeployment), and is asked to do it now. The window
	// is the one the deployment froze, with its spec. A store error is not "not
	// held": the deployment waits for the next cycle rather than start on a
	// guess (invariant 7).
	pause, err := e.pauseOf(ctx, d.Namespace, d.JobID)
	if err != nil {
		log.ErrorContext(ctx, "read the job's pause", "error", err)
		return
	}
	if hold := holdFrom(pause, e.windowStatus(meta.Parse(job.Meta))); hold != nil {
		log.DebugContext(ctx, "detected deployment of a held job left to detection", "reason", hold.Reason)
		e.kickDetection()
		return
	}
	e.applyTransition(ctx, log, d, nextAfterDecision(job), "")
}

// stepHook runs the deployment's hooks for phase ("pre" or "post"), one after
// the other in the order they are declared, and moves it on once all of them
// have succeeded; the first one to fail or time out stops the phase and fails
// the deployment. The hooks are the ones the deployment froze at detection:
// each one's revision is registered here, right before it is dispatched, from
// that spec (nothing of a hook is in Nomad before the deployment is approved,
// and what runs is what was approved). An error from registering a revision
// or from Hooks.Run (Nomad or SQLite) is left for the next cycle to retry, and
// a registration that keeps failing for the hook's timeout fails the
// deployment, as an unreachable hook would.
//
// A hook whose run already finished returns its stored result without being
// dispatched again, so a step that starts over, after an error or a restart,
// carries on at the first hook that has not finished.
func (e *Engine) stepHook(ctx context.Context, log *slog.Logger, d *store.Deployment, phase string) {
	job, err := parseJobSpec(d)
	if err != nil {
		log.ErrorContext(ctx, "decode job spec", "error", err)
		return
	}
	frozen, err := e.store.DeploymentHooks(ctx, d.ID)
	if err != nil {
		log.ErrorContext(ctx, "read frozen hooks", "phase", phase, "error", err)
		return
	}
	var phaseHooks []store.DeploymentHook
	for _, h := range frozen {
		if h.Phase == phase {
			phaseHooks = append(phaseHooks, h)
		}
	}
	if len(phaseHooks) == 0 {
		// Detection freezes every hook a deployment declares and only routes
		// here for one that does: no row means a deployment made before hooks
		// were frozen. Nothing to run and nothing to wait for: fail it, and
		// the job's next deployment starts from a clean state.
		e.applyTransition(ctx, log, d, store.StateFailed,
			fmt.Sprintf("%s-hook: the deployment has no frozen hook (created by an older nops): push a new commit or retry it", phase))
		return
	}

	for i, hook := range phaseHooks {
		timeout := frozenTimeout(hook)
		if err := e.registerRevision(ctx, log, d.Namespace, hook); err != nil {
			log.ErrorContext(ctx, "register hook revision", "phase", phase, "hook", hook.HookID, "revision", hook.Revision, "error", err)
			if e.now().Sub(e.hookStart(ctx, log, d, phase, phaseHooks, i)) >= timeout {
				e.applyTransition(ctx, log, d, store.StateFailed, hookFailure(phase,
					fmt.Sprintf("%s: could not register it within %s: %v", hook.HookID, timeout, err)))
			}
			return
		}

		res, err := e.hooks.Run(ctx, hooks.Request{
			DeploymentID: d.ID, Namespace: d.Namespace, Phase: phase, Position: hook.Position, HookJobID: hook.Revision,
			Commit: d.CommitSHA, Timeout: timeout, Target: job,
		})
		if err != nil {
			log.ErrorContext(ctx, "run hook", "phase", phase, "hook", hook.HookID, "error", err)
			return
		}
		switch res.State {
		case store.HookSucceeded:
			continue
		case store.HookFailed, store.HookTimedOut:
			e.applyTransition(ctx, log, d, store.StateFailed, hookFailure(phase,
				fmt.Sprintf("%s %s: %s", hook.HookID, res.State, res.Error)))
			return
		}
		// running/dispatching: Run only returns once terminal or on error, so
		// this case does not occur; stop here and look again next cycle.
		return
	}

	next := store.StateApplying
	if phase == "post" {
		next = store.StateCompleted
	}
	e.applyTransition(ctx, log, d, next, "")
}

// hookStart is when the hook at index i of the phase became the one being
// worked on: when the previous one finished, or when the deployment entered
// the phase for the first.
func (e *Engine) hookStart(ctx context.Context, log *slog.Logger, d *store.Deployment, phase string, phaseHooks []store.DeploymentHook, i int) time.Time {
	if i == 0 {
		return d.UpdatedAt
	}
	prev, err := e.store.GetHookRun(ctx, d.ID, phase, phaseHooks[i-1].Position)
	if err != nil || prev.FinishedAt.IsZero() {
		if err != nil {
			log.ErrorContext(ctx, "read the previous hook run", "phase", phase, "error", err)
		}
		return d.UpdatedAt
	}
	return prev.FinishedAt
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
// invariant 2 (CAS on the saved cas_index). A Nomad error before the register
// has gone through is retried next cycle, bounded by ApplyTimeout like the wait
// for health (see registerRetry).
func (e *Engine) stepRegister(ctx context.Context, log *slog.Logger, d *store.Deployment) {
	job, err := parseJobSpec(d)
	if err != nil {
		log.ErrorContext(ctx, "decode job spec", "error", err)
		return
	}

	live, err := e.nomad.Job(ctx, d.Namespace, d.JobID)
	var liveIndex uint64
	switch {
	case errors.Is(err, nomadx.ErrJobNotFound):
	case err != nil:
		log.ErrorContext(ctx, "get live job", "error", err)
		e.registerRetry(ctx, log, d, err)
		return
	default:
		liveIndex = derefUint64(live.JobModifyIndex)
	}

	var evalID string
	if liveIndex == d.CASIndex {
		plan, err := e.nomad.Plan(ctx, job)
		if err != nil {
			log.ErrorContext(ctx, "plan job", "error", err)
			e.registerRetry(ctx, log, d, err)
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
			e.registerRetry(ctx, log, d, err)
			return
		}
		evalID = res.EvalID
	} else {
		// The live index moved since detection: either the register already
		// happened (a crash between it succeeding and the next step) or a
		// real conflict. A plan of our spec against the live job tells them
		// apart (docs/deployment-lifecycle.md#recovery-after-a-crash).
		plan, err := e.nomad.Plan(ctx, job)
		if err != nil {
			log.ErrorContext(ctx, "plan job", "error", err)
			e.registerRetry(ctx, log, d, err)
			return
		}
		if plan.Diff != nil && plan.Diff.Type != "None" {
			e.applyTransition(ctx, log, d, store.StateFailed, "conflict: live job changed and our spec no longer applies")
			return
		}
		// Already applied: evalID stays empty, nothing more to register.
	}

	applied, err := e.nomad.Job(ctx, d.Namespace, d.JobID)
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

// registerRetry follows a Nomad error that stopped the register step before it
// went through: the deployment stays applying for the next cycle to try again,
// unless it has been applying for ApplyTimeout already. Some errors never heal
// by themselves (a namespace that is gone from Nomad, a token without
// submit-job, a spec Nomad refuses), and a deployment left applying would hold
// the job's only active slot (invariant 6) for good. The clock is the same as
// stepHealth's: the "-> applying" event, which a restart does not move.
func (e *Engine) registerRetry(ctx context.Context, log *slog.Logger, d *store.Deployment, cause error) {
	since, err := e.store.AppliedSince(ctx, d.ID)
	if err != nil {
		log.ErrorContext(ctx, "read applied timestamp", "error", err)
		return
	}
	if e.now().After(since.Add(e.applyTimeout)) {
		e.applyTransition(ctx, log, d, store.StateFailed,
			fmt.Sprintf("could not register within %s: %v", e.applyTimeout, cause))
	}
}

// stepHealth waits for the applied job version to become healthy, bounded by
// ApplyTimeout counted from the "-> applying" event, or from the promotion of
// the canaries if the Nomad deployment waited for one. While it waits for a
// person to promote them there is no bound: that is not a failure to become
// healthy (see docs/archive/engine-apply.md, decision 10).
func (e *Engine) stepHealth(ctx context.Context, log *slog.Logger, d *store.Deployment) {
	waiting := !d.PromotionWaitSince.IsZero() && d.PromotedAt.IsZero()
	if !waiting {
		since, err := e.store.AppliedSince(ctx, d.ID)
		if err != nil {
			log.ErrorContext(ctx, "read applied timestamp", "error", err)
			return
		}
		if !d.PromotedAt.IsZero() {
			since = d.PromotedAt
		}
		if e.now().After(since.Add(e.applyTimeout)) {
			e.applyTransition(ctx, log, d, store.StateFailed,
				fmt.Sprintf("apply did not become healthy within %s", e.applyTimeout))
			return
		}
	}

	verdict, reason, err := e.applyHealth(ctx, d)
	if err != nil {
		log.ErrorContext(ctx, "check apply health", "error", err)
		return // retried next cycle, still bounded by the timeout above unless waiting for a promotion
	}
	switch verdict {
	case healthFailed:
		e.applyTransition(ctx, log, d, store.StateFailed, "apply did not become healthy: "+reason)
	case healthHealthy:
		job, err := parseJobSpec(d)
		if err != nil {
			log.ErrorContext(ctx, "decode job spec", "error", err)
			return
		}
		next := store.StateCompleted
		if len(meta.Parse(job.Meta).PostHooks) > 0 {
			next = store.StatePostHook
		}
		e.applyTransition(ctx, log, d, next, "")
	case healthAwaitingPromotion:
		e.awaitPromotion(ctx, log, d)
	case healthPromoted:
		if waiting {
			e.promoted(ctx, log, d)
		}
	default:
		// still waiting: nothing to do this cycle.
	}
}

// awaitPromotion records, once, that the deployment waits for a person to
// promote its canaries in Nomad, and tells them. The notification follows the
// write (invariant 7) and is sent only by the call that wrote it, so a restart
// or another cycle does not send it again.
func (e *Engine) awaitPromotion(ctx context.Context, log *slog.Logger, d *store.Deployment) {
	wrote, err := e.store.MarkAwaitingPromotion(ctx, d.ID)
	if err != nil {
		log.ErrorContext(ctx, "record the wait for a canary promotion", "error", err)
		return
	}
	if !wrote {
		return
	}
	log.InfoContext(ctx, "deployment waiting for canary promotion in Nomad")
	fresh, err := e.store.GetDeployment(ctx, d.ID)
	if err != nil {
		log.ErrorContext(ctx, "reload deployment before notify", "error", err)
		return
	}
	go e.notifier.Notify(ctx, fresh)
}

// promoted records that the canaries a deployment waited for are promoted: the
// apply timeout counts again, from now, for the rest of the rollout.
func (e *Engine) promoted(ctx context.Context, log *slog.Logger, d *store.Deployment) {
	wrote, err := e.store.MarkPromoted(ctx, d.ID)
	if err != nil {
		log.ErrorContext(ctx, "record the promotion of the canaries", "error", err)
		return
	}
	if wrote {
		log.InfoContext(ctx, "canaries promoted in Nomad: the apply timeout counts again", "apply_timeout", e.applyTimeout)
	}
}

// health is what applyHealth makes of the job version nops applied.
type health int

const (
	healthWaiting health = iota // neither healthy nor failed: look again next cycle
	healthHealthy
	healthFailed
	// healthAwaitingPromotion: the Nomad deployment is running, its canaries are
	// placed and healthy, and it waits for a person to promote them.
	healthAwaitingPromotion
	// healthPromoted: the Nomad deployment is running and its canaries have been
	// promoted; the rest of the rollout is Nomad's.
	healthPromoted
)

// applyHealth decides whether the job version nops applied is healthy: via
// its Nomad deployment when there is one tracking exactly that apply, else
// via the allocations of that job version (a batch job, or an update stanza
// that produces no Nomad deployment). healthWaiting means neither healthy nor
// failed; reason says why for healthFailed.
func (e *Engine) applyHealth(ctx context.Context, d *store.Deployment) (health, string, error) {
	dep, err := e.nomad.LatestDeployment(ctx, d.Namespace, d.JobID)
	if err != nil {
		return healthWaiting, "", err
	}
	if dep != nil && dep.JobModifyIndex == d.AppliedIndex {
		switch dep.Status {
		case api.DeploymentStatusSuccessful:
			return healthHealthy, "", nil
		case api.DeploymentStatusFailed, api.DeploymentStatusCancelled:
			return healthFailed, fmt.Sprintf("nomad deployment %s: %s", dep.Status, dep.StatusDescription), nil
		case api.DeploymentStatusRunning:
			job, err := parseJobSpec(d)
			if err != nil {
				return healthWaiting, "", err
			}
			return canaryHealth(dep, job), "", nil
		default:
			return healthWaiting, "", nil
		}
	}

	live, err := e.nomad.Job(ctx, d.Namespace, d.JobID)
	if err != nil {
		return healthWaiting, "", err
	}
	// The allocations of the live version are ours only while the live job is
	// still the one nops registered: after an outside edit (for instance while
	// nops was down) they belong to someone else's spec.
	if liveIndex := derefUint64(live.JobModifyIndex); liveIndex != d.AppliedIndex {
		return healthFailed, fmt.Sprintf("job modified outside nops while waiting for health (live index %d, applied %d)",
			liveIndex, d.AppliedIndex), nil
	}
	if neverHasAllocations(live) {
		return healthHealthy, "", nil // registered at the applied index: nothing to wait for
	}
	version := derefUint64(live.Version)
	allocs, err := e.nomad.Allocations(ctx, d.Namespace, d.JobID)
	if err != nil {
		return healthWaiting, "", err
	}
	var ours []nomadx.Alloc
	for _, a := range allocs {
		if a.JobVersion == version {
			ours = append(ours, a)
		}
	}
	if len(ours) == 0 {
		return healthWaiting, "", nil // scheduling not done yet
	}
	for _, a := range ours {
		if a.ClientStatus == "failed" || a.ClientStatus == "lost" {
			return healthFailed, a.Failure, nil
		}
	}
	for _, a := range ours {
		if a.ClientStatus != "running" && a.ClientStatus != "complete" {
			return healthWaiting, "", nil // still starting
		}
	}
	return healthHealthy, "", nil
}

// canaryHealth is the verdict on a running Nomad deployment of the job nops
// registered, for the groups that have canaries a person has to promote (a
// group with auto_promote promotes them itself, and is Nomad's to finish):
// healthAwaitingPromotion once every one of them has all its canaries placed
// and healthy and none is promoted yet, healthPromoted once they are promoted,
// else healthWaiting (the canaries are still starting, or there are none).
func canaryHealth(dep *api.Deployment, job *api.Job) health {
	manual := 0
	promoted := 0
	ready := 0
	for _, g := range job.TaskGroups {
		if g == nil || g.Name == nil {
			continue
		}
		st := dep.TaskGroups[*g.Name]
		if st == nil || st.DesiredCanaries == 0 || autoPromotes(job, g) {
			continue
		}
		manual++
		switch {
		case st.Promoted:
			promoted++
		case len(st.PlacedCanaries) >= st.DesiredCanaries && st.HealthyAllocs >= st.DesiredCanaries:
			ready++
		}
	}
	switch {
	case manual == 0:
		return healthWaiting
	case promoted == manual:
		return healthPromoted
	case ready+promoted == manual:
		return healthAwaitingPromotion
	}
	return healthWaiting
}

// autoPromotes reports whether Nomad promotes the canaries of a group by
// itself. The job comes canonicalized from Nomad, which merges the job-level
// update into the group's, so the group's own is the one that counts; the
// job's is only a fallback for a spec that was not canonicalized.
func autoPromotes(job *api.Job, g *api.TaskGroup) bool {
	u := g.Update
	if u == nil {
		u = job.Update
	}
	return u != nil && u.AutoPromote != nil && *u.AutoPromote
}

// neverHasAllocations reports whether a job has no allocation of its own to
// wait for: a periodic job only spawns child jobs, a parameterized one waits to
// be dispatched, and a job whose groups all have count 0 places nothing. Once
// registered it is healthy: the allocations path would wait for one that never
// comes and fail the deployment at the apply timeout.
func neverHasAllocations(job *api.Job) bool {
	if job.IsPeriodic() || job.IsParameterized() {
		return true
	}
	if len(job.TaskGroups) == 0 {
		return false
	}
	for _, g := range job.TaskGroups {
		if g == nil || g.Count == nil || *g.Count > 0 {
			return false
		}
	}
	return true
}

// Approve moves a pending_approval deployment forward: pre_hook if the
// target spec declares nops_pre_hook, else applying. It refuses without
// touching Nomad or the store if specHash no longer matches the deployment's
// current one (invariant 3), or if its job is not among the managed jobs of the
// last detection cycle (ErrNotInRepo): a broken file elsewhere suspends the
// removal of a job's deployments, so the deployment of a job git no longer has
// can still be pending, and approving it would register that job. It also
// refuses (ErrPaused) while the job is paused: the deployment stays
// pending_approval and can be approved once the job is resumed.
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
	if !e.observed(d.Namespace, d.JobID) {
		return fmt.Errorf("approve %s: %w", id, ErrNotInRepo)
	}
	hold, err := e.pauseOf(ctx, d.Namespace, d.JobID)
	if err != nil {
		return fmt.Errorf("approve %s: %w", id, err)
	}
	if hold != nil {
		return fmt.Errorf("approve %s: %w: %s", id, ErrPaused, hold.Reason)
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

// ErrNotWaitingForPromotion is returned by Promote when the deployment is not
// waiting for its canaries to be promoted, as Nomad reports it now: it moved on,
// or someone else promoted them since the page was rendered.
var ErrNotWaitingForPromotion = errors.New("the deployment is not waiting for a canary promotion")

// Promote promotes the canaries of the Nomad deployment an applying deployment
// waits on (see docs/archive/engine-apply.md, decision 11). It is a human's
// action, not a register: it changes no spec, so plan and CAS do not apply. It
// refuses, without touching Nomad, unless the deployment is applying, was seen
// waiting for a promotion and is still waiting for it in Nomad now: the Nomad
// deployment tracks the applied index and canaryHealth says its canaries are
// healthy and not promoted. Who asked is on record (an event) before Nomad is
// asked (invariant 7); the apply loop then sees the promoted canaries and
// restarts the apply timeout as for any promotion.
func (e *Engine) Promote(ctx context.Context, id, actor string) error {
	if actor == "" {
		return errors.New("promote: actor is required")
	}
	d, err := e.store.GetDeployment(ctx, id)
	if err != nil {
		return fmt.Errorf("promote %s: %w", id, err)
	}
	if !e.managedNS[d.Namespace] {
		return fmt.Errorf("promote %s: %w", id, store.ErrNotFound)
	}
	if d.State != store.StateApplying || d.PromotionWaitSince.IsZero() || !d.PromotedAt.IsZero() {
		return fmt.Errorf("promote %s: %w", id, ErrNotWaitingForPromotion)
	}
	job, err := parseJobSpec(d)
	if err != nil {
		return fmt.Errorf("promote %s: %w", id, err)
	}
	dep, err := e.nomad.LatestDeployment(ctx, d.Namespace, d.JobID)
	if err != nil {
		return fmt.Errorf("promote %s: %w", id, err)
	}
	if dep == nil || dep.JobModifyIndex != d.AppliedIndex || dep.Status != api.DeploymentStatusRunning ||
		canaryHealth(dep, job) != healthAwaitingPromotion {
		return fmt.Errorf("promote %s: %w", id, ErrNotWaitingForPromotion)
	}

	if err := e.store.MarkPromotionRequested(ctx, id, actor); err != nil {
		return fmt.Errorf("promote %s: %w", id, err)
	}
	if err := e.nomad.PromoteDeployment(ctx, d.Namespace, dep.ID); err != nil {
		e.log.ErrorContext(ctx, "promote canaries", "deployment_id", id, "job", d.JobID, "namespace", d.Namespace,
			"nomad_deployment", dep.ID, "actor", actor, "error", err)
		return fmt.Errorf("promote %s: %w", id, err)
	}
	e.log.InfoContext(ctx, "canaries promoted", "deployment_id", id, "job", d.JobID, "namespace", d.Namespace,
		"nomad_deployment", dep.ID, "actor", actor)
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
	e.kickDetection() // the job is now blocked, which the last cycle does not know
	return nil
}
