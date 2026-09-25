package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// A hook revision is a hook job at one exact spec, registered in Nomad under
// its own ID, `<hook-id>-<first 8 hex of the spec hash>`. A deployment freezes
// the revisions of its hooks when it is created (see
// docs/design/engine-detection.md#hook-revisions), so what runs is what was
// approved, and two deployments that share a hook never overwrite each other's.

// revisionRe matches the ID of a hook revision, which GC uses to tell it from
// a hook job registered by hand.
var revisionRe = regexp.MustCompile(`^.+-[0-9a-f]{8}$`)

// revisionID is the Nomad job ID of a hook at the spec that hashes to hash.
func revisionID(hookID, hash string) string { return hookID + "-" + hash[:8] }

// freezeHooks builds the hooks a job's deployment freezes, from the hook jobs
// of the snapshot, and the hash that identifies the deployment: the target's
// spec hash combined with the hooks it runs, so a change to a hook is a
// change to what was approved. A declared hook that is not in the snapshot
// contributes to the hash with no spec, so adding the file changes it too, and
// is named in missing (`pre-hook "id"`), the first one found.
//
// With no hooks the hash is the target's own.
func freezeHooks(cfg meta.Config, hooks map[string]parsedFile, targetHash string) (frozen []store.DeploymentHook, hash, missing string, err error) {
	declared := []struct {
		phase string
		hook  *meta.Hook
	}{{"pre", cfg.PreHook}, {"post", cfg.PostHook}}

	var b strings.Builder
	b.WriteString(targetHash)
	any := false
	for _, dh := range declared {
		if dh.hook == nil {
			continue
		}
		any = true
		hf, ok := hooks[dh.hook.JobID]
		if !ok {
			if missing == "" {
				missing = fmt.Sprintf("%s-hook %q", dh.phase, dh.hook.JobID)
			}
			fmt.Fprintf(&b, "\n%s:%s:missing", dh.phase, dh.hook.JobID)
			continue
		}
		specHash, err := specHash(hf.job)
		if err != nil {
			return nil, "", "", fmt.Errorf("hash hook %s: %w", dh.hook.JobID, err)
		}
		spec, err := json.Marshal(hf.job)
		if err != nil {
			return nil, "", "", fmt.Errorf("marshal hook %s: %w", dh.hook.JobID, err)
		}
		frozen = append(frozen, store.DeploymentHook{
			Phase: dh.phase, HookID: dh.hook.JobID, Revision: revisionID(dh.hook.JobID, specHash),
			SpecHash: specHash, JobSpec: string(spec),
		})
		fmt.Fprintf(&b, "\n%s:%s:%s", dh.phase, dh.hook.JobID, specHash)
	}
	if !any {
		return nil, targetHash, "", nil
	}
	return frozen, hashOf(b.String()), missing, nil
}

// revisionJob is the job to register for a frozen hook: its spec with the ID
// of the revision. Name stays the hook's own, so what an operator reads in
// Nomad is still the hook.
func revisionJob(h store.DeploymentHook) (*api.Job, error) {
	var job api.Job
	if err := json.Unmarshal([]byte(h.JobSpec), &job); err != nil {
		return nil, fmt.Errorf("decode spec of hook %s: %w", h.HookID, err)
	}
	if job.Name == nil {
		name := h.HookID
		job.Name = &name
	}
	id := h.Revision
	job.ID = &id
	return &job, nil
}

// registerRevision makes sure the revision of a frozen hook is registered and
// running, right before the hook is dispatched: nothing of a hook is in Nomad
// before its deployment is approved. Plan then CAS like every write (a
// revision that is already there and identical needs none, and one that GC
// stopped shows as a difference, so it is registered again at its own index).
// Two deployments registering the same revision at once are safe: the loser
// gets a CAS conflict and, retried, finds nothing to do.
func (e *Engine) registerRevision(ctx context.Context, log *slog.Logger, h store.DeploymentHook) error {
	job, err := revisionJob(h)
	if err != nil {
		return err
	}
	var index uint64
	live, err := e.nomad.Job(ctx, h.Revision)
	switch {
	case errors.Is(err, nomadx.ErrJobNotFound):
	case err != nil:
		return err
	default:
		index = derefUint64(live.JobModifyIndex)
	}
	plan, err := e.nomad.Plan(ctx, job)
	if err != nil {
		return err
	}
	if plan.Diff == nil || plan.Diff.Type == "None" {
		return nil
	}
	if _, err := e.nomad.RegisterCAS(ctx, job, index, false); err != nil {
		return err
	}
	log.InfoContext(ctx, "hook revision registered", "hook", h.HookID, "revision", h.Revision)
	return nil
}

// gcHookRevisions deregisters, without purging, the hook revisions no
// non-terminal deployment needs. It only touches jobs that are hooks
// (nops_role = "hook"), whose ID has the shape of a revision and that are not
// children of another job; a hook registered by hand under its plain ID is
// never one. A Nomad failure is an ERROR, retried by the next cycle; a store
// failure is returned.
func (e *Engine) gcHookRevisions(ctx context.Context) error {
	stubs, err := e.nomad.ListJobs(ctx)
	if err != nil {
		e.log.ErrorContext(ctx, "hook revision GC: list jobs", "error", err)
		return nil
	}
	var candidates []string
	for _, s := range stubs {
		if s.ParentID == "" && !s.Stop && revisionRe.MatchString(s.ID) && meta.Parse(s.Meta).IsHook {
			candidates = append(candidates, s.ID)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// After the listing: a deployment created since only needs a revision that
	// is registered again by its own step if this run stops it.
	used, err := e.store.HookRevisionsInUse(ctx, e.namespace)
	if err != nil {
		return err
	}
	inUse := make(map[string]bool, len(used))
	for _, r := range used {
		inUse[r] = true
	}
	for _, id := range candidates {
		if inUse[id] {
			continue
		}
		if err := e.nomad.StopJob(ctx, id); err != nil {
			e.log.ErrorContext(ctx, "hook revision GC: deregister", "job", id, "namespace", e.namespace, "error", err)
			continue
		}
		e.log.InfoContext(ctx, "hook revision deregistered", "job", id, "namespace", e.namespace)
	}
	return nil
}
