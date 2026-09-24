package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/redact"
	"github.com/music-gang/nops/internal/store"
)

// parsedFile is one snapshot file Nomad could parse, classified by its meta.
type parsedFile struct {
	path string
	job  *api.Job
	cfg  meta.Config
}

// Detect runs one detection cycle: parse every file of the current gitwatch
// snapshot, sync hook jobs, and create, supersede or complete deployments for
// managed jobs. It never applies anything (see engine-apply).
//
// It returns an error only for a failure that must stop the whole cycle: a
// store failure (invariant 7: never act on Nomad with unpersisted state) or a
// redact failure (never store an unredacted diff). Every other problem is
// scoped to the one file or job it came from: it is logged and the cycle
// continues.
func (e *Engine) Detect(ctx context.Context) error {
	snap := e.snapshots.Snapshot()
	commit := snap.Commit

	parsed, cache := e.parseFiles(ctx, snap)
	e.replaceParseCache(cache)

	hooks, managed := e.classify(ctx, commit, parsed)
	e.syncHooks(ctx, commit, hooks)

	hookIDs := make(map[string]bool, len(hooks))
	for _, h := range hooks {
		hookIDs[*h.job.ID] = true
	}

	seen := make(map[string]bool, len(managed))
	observations := make(map[string]Observation, len(managed))
	for _, mf := range managed {
		jobID := *mf.job.ID
		seen[jobID] = true // classified as managed: never "removed from repo" this cycle

		obs, ok, err := e.reconcileJob(ctx, commit, mf, hookIDs)
		if err != nil {
			return err
		}
		if ok {
			observations[jobID] = obs
		}
	}
	e.replaceObservations(observations)

	return e.supersedeRemoved(ctx, seen)
}

// parseFiles parses every file of the snapshot through Nomad, reusing the
// previous cycle's cache entry when the (content, vars) pair is unchanged.
// The returned cache replaces the engine's: an entry for a file no longer in
// the snapshot is dropped, so the cache never grows across cycles.
func (e *Engine) parseFiles(ctx context.Context, snap gitwatch.Snapshot) ([]parsedFile, map[string]parseEntry) {
	prev := e.snapshotParseCache()
	next := make(map[string]parseEntry, len(snap.Files))
	parsed := make([]parsedFile, 0, len(snap.Files))

	for _, f := range snap.Files {
		key := parseCacheKey(f.Content, f.Vars)
		entry, ok := e.cachedParse(prev, key)
		if !ok {
			job, err := e.nomad.ParseHCL(ctx, f.Content, f.Vars)
			entry = parseEntry{job: job, err: err}
		}
		next[key] = entry

		switch {
		case entry.err != nil:
			e.log.ErrorContext(ctx, "parse job file", "file", f.Path, "commit", snap.Commit, "error", entry.err)
			continue
		case entry.job.ID == nil || *entry.job.ID == "":
			e.log.ErrorContext(ctx, "parsed job has no ID", "file", f.Path, "commit", snap.Commit)
			continue
		}
		if ns := entry.job.Namespace; ns != nil && *ns != "" && *ns != e.namespace {
			e.log.ErrorContext(ctx, "job declares a namespace nops does not manage", "file", f.Path,
				"job", *entry.job.ID, "declared_namespace", *ns, "namespace", e.namespace)
			continue
		}
		parsed = append(parsed, parsedFile{path: f.Path, job: entry.job, cfg: meta.Parse(entry.job.Meta)})
	}
	return parsed, next
}

// classify groups parsed files by job ID: two files parsing to the same ID
// are both ignored, with an ERROR (the conservative reading, since nops
// cannot tell which one is meant). Every issue found by meta.Parse is logged
// here, whatever the file turns out to be. A job with nops_role = "hook"
// takes precedence over nops_managed on the same file.
func (e *Engine) classify(ctx context.Context, commit string, parsed []parsedFile) (hooks, managed []parsedFile) {
	byID := make(map[string][]parsedFile, len(parsed))
	for _, pf := range parsed {
		id := *pf.job.ID
		byID[id] = append(byID[id], pf)
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		group := byID[id]
		if len(group) > 1 {
			paths := make([]string, len(group))
			for i, pf := range group {
				paths[i] = pf.path
			}
			e.log.ErrorContext(ctx, "two files parse to the same job ID: both ignored",
				"job", id, "files", paths, "commit", commit)
			continue
		}
		pf := group[0]
		for _, iss := range pf.cfg.Issues {
			logIssue(ctx, e.log, id, pf.path, commit, iss)
		}
		switch {
		case pf.cfg.IsHook:
			hooks = append(hooks, pf)
		case pf.cfg.Managed:
			managed = append(managed, pf)
		}
	}
	return hooks, managed
}

func logIssue(ctx context.Context, log *slog.Logger, jobID, path, commit string, iss meta.Issue) {
	args := []any{"job", jobID, "file", path, "commit", commit, "key", iss.Key, "message", iss.Message}
	if iss.Severity == meta.SeverityError {
		log.ErrorContext(ctx, "invalid meta key", args...)
		return
	}
	log.WarnContext(ctx, "meta key issue", args...)
}

// syncHooks registers or updates every hook job found in the snapshot, plan
// then CAS, whatever the policy of the job it serves: it must be inert and
// ready in Nomad before any deployment can dispatch it. A failure here is an
// ERROR, retried at the next cycle; it never fails a deployment (the hook
// runner validates the job again at dispatch, see internal/hooks).
func (e *Engine) syncHooks(ctx context.Context, commit string, hooks []parsedFile) {
	for _, hf := range hooks {
		id := *hf.job.ID
		log := e.log.With("job", id, "namespace", e.namespace, "commit", commit)

		var index uint64
		live, err := e.nomad.Job(ctx, id)
		switch {
		case errors.Is(err, nomadx.ErrJobNotFound):
		case err != nil:
			log.ErrorContext(ctx, "hook sync: get live job", "error", err)
			continue
		default:
			index = derefUint64(live.JobModifyIndex)
		}

		plan, err := e.nomad.Plan(ctx, hf.job)
		if err != nil {
			log.ErrorContext(ctx, "hook sync: plan job", "error", err)
			continue
		}
		if plan.Diff == nil || plan.Diff.Type == "None" {
			continue
		}
		if _, err := e.nomad.RegisterCAS(ctx, hf.job, index, false); err != nil {
			log.ErrorContext(ctx, "hook sync: register", "error", err)
			continue
		}
		log.InfoContext(ctx, "hook job synced")
	}
}

// reconcileJob computes the drift of one managed job and reconciles its
// deployment. ok is false when the job was skipped because of a Nomad
// failure scoped to it (already logged): the caller keeps no observation for
// it this cycle rather than showing stale data.
func (e *Engine) reconcileJob(ctx context.Context, commit string, mf parsedFile, hookIDs map[string]bool) (obs Observation, ok bool, err error) {
	jobID := *mf.job.ID
	log := e.log.With("job", jobID, "namespace", e.namespace, "commit", commit)

	hash, err := specHash(mf.job)
	if err != nil {
		log.ErrorContext(ctx, "compute spec hash", "error", err)
		return Observation{}, false, nil
	}

	var (
		live      *api.Job
		liveIndex uint64
	)
	live, err = e.nomad.Job(ctx, jobID)
	switch {
	case errors.Is(err, nomadx.ErrJobNotFound):
		live = nil
	case err != nil:
		log.ErrorContext(ctx, "get live job", "error", err)
		return Observation{}, false, nil
	default:
		liveIndex = derefUint64(live.JobModifyIndex)
	}

	planJob := mf.job
	if live != nil {
		planJob, err = substituteScalingCounts(mf.job, live)
		if err != nil {
			log.ErrorContext(ctx, "copy live scaling counts", "error", err)
			return Observation{}, false, nil
		}
	}

	plan, err := e.nomad.Plan(ctx, planJob)
	if err != nil {
		log.ErrorContext(ctx, "plan job", "error", err)
		return Observation{}, false, nil
	}
	drift := plan.Diff != nil && plan.Diff.Type != "None"
	redacted, err := redact.Diff(plan.Diff)
	if err != nil {
		return Observation{}, false, fmt.Errorf("redact plan diff of %s: %w", jobID, err)
	}

	obs = Observation{
		JobID: jobID, Namespace: e.namespace, FilePath: mf.path,
		Policy: mf.cfg.Policy, Issues: mf.cfg.Issues, ObservedAt: e.now(),
	}
	if drift {
		obs.Drift, obs.PlanDiff = true, string(redacted)
	}

	blockedBy, blockedReason, err := e.reconcileDeployment(ctx, log, jobID, commit, mf.cfg, hash, liveIndex, drift, redacted, planJob, hookIDs)
	if err != nil {
		return obs, true, err
	}
	obs.BlockedBy, obs.BlockedReason = blockedBy, blockedReason
	return obs, true, nil
}

// reconcileDeployment applies the state-machine rules of
// docs/state-machine.md to one job: revalidate an existing detected/
// pending_approval deployment, then create a new one if there is still drift
// to apply. A deployment in pre_hook/applying/post_hook is left untouched:
// detection never interferes with an apply in progress. blockedBy is the ID
// of the deployment whose retry rule is suppressing a new deployment for
// this job's drift, or "" if none (see docs/design/engine-apply.md, decisions
// 6 and 7); it feeds Observation.BlockedBy/BlockedReason for the dashboard.
func (e *Engine) reconcileDeployment(ctx context.Context, log *slog.Logger, jobID, commit string, cfg meta.Config,
	hash string, liveIndex uint64, drift bool, redacted []byte, planJob *api.Job, hookIDs map[string]bool) (blockedBy, blockedReason string, err error) {

	active, err := e.store.ActiveDeployment(ctx, e.namespace, jobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		active = nil
	case err != nil:
		return "", "", fmt.Errorf("active deployment of %s: %w", jobID, err)
	}

	if active != nil {
		switch active.State {
		case store.StatePreHook, store.StateApplying, store.StatePostHook:
			return "", "", nil
		case store.StateDetected, store.StatePendingApproval:
			switch {
			case cfg.Policy == meta.PolicyNone:
				if err := e.transition(ctx, log, active, store.StateSuperseded, "policy changed to none"); err != nil {
					return "", "", err
				}
			case active.SpecHash != hash:
				if err := e.transition(ctx, log, active, store.StateSuperseded, fmt.Sprintf("newer spec at commit %s", commit)); err != nil {
					return "", "", err
				}
			case active.CASIndex != liveIndex:
				if err := e.transition(ctx, log, active, store.StateSuperseded, "job modified outside nops"); err != nil {
					return "", "", err
				}
			case !drift:
				if err := e.transition(ctx, log, active, store.StateCompleted, "already in sync"); err != nil {
					return "", "", err
				}
			default:
				return "", "", nil // still approvable / still pending: nothing to do
			}
		}
	}

	if cfg.Policy == meta.PolicyNone || !drift {
		return "", "", nil
	}

	latest, err := e.store.LatestDeployment(ctx, e.namespace, jobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return "", "", fmt.Errorf("latest deployment of %s: %w", jobID, err)
	default:
		if blockedBy, blockedReason = blockedRetry(latest, hash, liveIndex); blockedBy != "" {
			return blockedBy, blockedReason, nil
		}
	}

	specJSON, err := json.Marshal(planJob)
	if err != nil {
		return "", "", fmt.Errorf("marshal job spec of %s: %w", jobID, err)
	}
	d := &store.Deployment{
		JobID: jobID, Namespace: e.namespace, CommitSHA: commit, SpecHash: hash,
		JobSpec: string(specJSON), PlanDiff: string(redacted), Policy: storePolicy(cfg.Policy), CASIndex: liveIndex,
	}
	if err := e.store.CreateDeployment(ctx, d); err != nil {
		if errors.Is(err, store.ErrActiveDeployment) {
			log.WarnContext(ctx, "active deployment appeared concurrently: skipped this cycle", "error", err)
			return "", "", nil
		}
		return "", "", fmt.Errorf("create deployment for %s: %w", jobID, err)
	}

	if missing := missingHook(cfg, hookIDs); missing != "" {
		return "", "", e.transition(ctx, log, d, store.StateFailed, fmt.Sprintf("%s not found in repo at commit %s", missing, commit))
	}
	if cfg.Policy == meta.PolicyApproval {
		return "", "", e.transition(ctx, log, d, store.StatePendingApproval, "waiting for approval")
	}
	return "", "", nil
}

// blockedRetry decides whether a job's drift is blocked from a new
// deployment by its latest one, and why (docs/state-machine.md, "not
// retrying an unchanged failure", extended by
// docs/design/engine-apply.md, decision 6): a failed or rejected deployment
// with the same spec_hash blocks as long as nothing has changed — the live
// index too, unless the deployment reached the register (AppliedIndex != 0),
// in which case it blocks regardless of the live index, since Nomad itself
// (for example auto_revert) may be the one moving it.
func blockedRetry(latest *store.Deployment, hash string, liveIndex uint64) (blockedBy, reason string) {
	if latest.State != store.StateFailed && latest.State != store.StateRejected {
		return "", ""
	}
	if latest.SpecHash != hash {
		return "", ""
	}
	if latest.AppliedIndex != 0 {
		return latest.ID, fmt.Sprintf("deployment %s failed after applying this spec; a new commit is needed to retry", latest.ID)
	}
	if latest.CASIndex == liveIndex {
		return latest.ID, fmt.Sprintf("deployment %s failed on the same live job; nothing has changed since", latest.ID)
	}
	return "", ""
}

// supersedeRemoved supersedes every detected/pending_approval deployment
// whose job is no longer in the repo's snapshot. A deployment already being
// applied (pre_hook/applying/post_hook) is left alone.
func (e *Engine) supersedeRemoved(ctx context.Context, seen map[string]bool) error {
	active, err := e.store.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("list active deployments: %w", err)
	}
	for _, d := range active {
		if d.Namespace != e.namespace || seen[d.JobID] {
			continue
		}
		if d.State != store.StateDetected && d.State != store.StatePendingApproval {
			continue
		}
		log := e.log.With("job", d.JobID, "namespace", e.namespace)
		if err := e.transition(ctx, log, d, store.StateSuperseded, "job removed from repo"); err != nil {
			return err
		}
	}
	return nil
}

// transition moves d to `to` and, for the two states that need a human
// (pending_approval and failed), notifies after the transition is persisted
// (invariant 7). A conflict (someone else moved the deployment first, or an
// illegal move) is logged and skipped rather than aborting the cycle.
func (e *Engine) transition(ctx context.Context, log *slog.Logger, d *store.Deployment, to store.State, message string) error {
	t := store.Transition{From: d.State, Actor: "nops", Message: message}
	if to == store.StateFailed {
		t.Error = message
	}
	if err := e.store.Transition(ctx, d.ID, to, t); err != nil {
		if errors.Is(err, store.ErrStateConflict) || errors.Is(err, store.ErrInvalidTransition) {
			log.WarnContext(ctx, "transition skipped", "deployment_id", d.ID, "to", to, "error", err)
			return nil
		}
		return fmt.Errorf("transition %s to %s: %w", d.ID, to, err)
	}
	log.InfoContext(ctx, "deployment "+string(to), "deployment_id", d.ID, "message", message)

	if to != store.StatePendingApproval && to != store.StateFailed {
		return nil
	}
	fresh, err := e.store.GetDeployment(ctx, d.ID)
	if err != nil {
		log.ErrorContext(ctx, "reload deployment before notify", "deployment_id", d.ID, "error", err)
		return nil
	}
	go e.notifier.Notify(ctx, fresh)
	return nil
}

// missingHook reports the first declared hook that is not among the hook
// jobs found in the repo's snapshot, or "" if both are.
func missingHook(cfg meta.Config, hookIDs map[string]bool) string {
	if cfg.PreHook != nil && !hookIDs[cfg.PreHook.JobID] {
		return fmt.Sprintf("pre-hook %q", cfg.PreHook.JobID)
	}
	if cfg.PostHook != nil && !hookIDs[cfg.PostHook.JobID] {
		return fmt.Sprintf("post-hook %q", cfg.PostHook.JobID)
	}
	return ""
}

func storePolicy(p meta.Policy) store.Policy {
	if p == meta.PolicyAuto {
		return store.PolicyAuto
	}
	return store.PolicyApproval
}

// specHash identifies the content of a parsed job, computed before any
// live-cluster adjustment (substituteScalingCounts), so a live count change
// alone never supersedes a pending approval.
func specHash(job *api.Job) (string, error) {
	b, err := json.Marshal(job)
	if err != nil {
		return "", err
	}
	return hashOf(string(b)), nil
}

// substituteScalingCounts returns job unchanged if none of its task groups
// with a scaling policy have a live counterpart; otherwise it returns a deep
// copy with the live count substituted in, so a plan against it does not
// show a Count difference the autoscaler owns (nops does not fight it: see
// docs/philosophy.md). job itself, which may be a cached parse result, is
// never mutated.
func substituteScalingCounts(job, live *api.Job) (*api.Job, error) {
	liveCounts := make(map[string]int, len(live.TaskGroups))
	for _, g := range live.TaskGroups {
		if g != nil && g.Name != nil && g.Count != nil {
			liveCounts[*g.Name] = *g.Count
		}
	}
	needsCopy := false
	for _, g := range job.TaskGroups {
		if g == nil || g.Scaling == nil || g.Name == nil {
			continue
		}
		if _, ok := liveCounts[*g.Name]; ok {
			needsCopy = true
			break
		}
	}
	if !needsCopy {
		return job, nil
	}
	cp, err := deepCopyJob(job)
	if err != nil {
		return nil, err
	}
	for _, g := range cp.TaskGroups {
		if g == nil || g.Scaling == nil || g.Name == nil {
			continue
		}
		if c, ok := liveCounts[*g.Name]; ok {
			count := c
			g.Count = &count
		}
	}
	return cp, nil
}

func deepCopyJob(job *api.Job) (*api.Job, error) {
	b, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	var cp api.Job
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

func derefUint64(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}
