package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// ErrNotRetryable is returned by Retry when the deployment cannot be retried
// now: it is not the latest of its job, its spec is not the one in git, the
// job is not in the repository or has policy none, or there is nothing to run
// again (it did not reach the register and the job has no drift). The page it
// was asked from may be older than that.
var ErrNotRetryable = errors.New("the deployment cannot be retried")

// Retryable reports whether Retry would take d, a deployment of a job whose
// latest deployment is latest (nil if unknown): it is failed or rejected, not
// retried yet, the latest of its job, and still the head, that is the job's
// spec in git now is its spec (anything else would be a rollback). The job must
// still be one nops deploys, and there must be something to run: drift, or a
// deployment that failed after its register.
func (e *Engine) Retryable(d, latest *store.Deployment) bool {
	if latest == nil || latest.ID != d.ID || !d.RetriedAt.IsZero() ||
		(d.State != store.StateFailed && d.State != store.StateRejected) {
		return false
	}
	obs, ok := e.observation(jobKey{d.Namespace, d.JobID})
	if !ok || obs.Policy == meta.PolicyNone || obs.SpecHash != d.SpecHash {
		return false
	}
	return obs.Drift || d.AppliedIndex != 0
}

// Retry lets a human try a failed or rejected deployment again, without a new
// commit (see docs/deployment-lifecycle.md, "not retrying an unchanged
// failure"). It does not touch Nomad: it marks the deployment retried
// (persisted first, invariant 7), so that the anti-loop rule stops counting it,
// and runs a detection cycle, which creates the deployment that retries it like
// any other: it follows the policy, so under "approval" it waits for a human
// decision (invariant 3). The cycle runs here, not at the loop's next tick,
// because the page the person is sent back to reads its outcome: the successor
// and the end of the block.
//
// It returns the ID of the deployment that retries id, or id itself when there
// is none (the job is held, or the cycle failed and is left to the loop). It
// returns ErrNotRetryable if the deployment cannot be retried now and
// store.ErrAlreadyRetried if it already was.
func (e *Engine) Retry(ctx context.Context, id, actor string) (string, error) {
	if actor == "" {
		return "", errors.New("retry: actor is required")
	}
	d, err := e.store.GetDeployment(ctx, id)
	if err != nil {
		return "", fmt.Errorf("retry %s: %w", id, err)
	}
	if !e.managedNS[d.Namespace] {
		return "", fmt.Errorf("retry %s: %w", id, store.ErrNotFound)
	}
	if !d.RetriedAt.IsZero() {
		return "", fmt.Errorf("retry %s: %w", id, store.ErrAlreadyRetried)
	}
	latest, err := e.store.LatestDeployment(ctx, d.Namespace, d.JobID)
	if err != nil {
		return "", fmt.Errorf("retry %s: %w", id, err)
	}
	if !e.Retryable(d, latest) {
		return "", fmt.Errorf("retry %s: %w", id, ErrNotRetryable)
	}

	if err := e.store.MarkRetried(ctx, id, actor); err != nil {
		return "", fmt.Errorf("retry %s: %w", id, err)
	}
	e.log.InfoContext(ctx, "deployment retried", "deployment_id", id, "job", d.JobID, "namespace", d.Namespace, "actor", actor)

	// The person may close the tab: a cycle stopped halfway is no cycle.
	if err := e.Detect(context.WithoutCancel(ctx)); err != nil {
		e.log.ErrorContext(ctx, "detection cycle after a retry aborted", "deployment_id", id, "error", err)
		e.kickDetection()
		return id, nil
	}
	next, err := e.store.RetryOf(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return id, nil
	case err != nil:
		e.log.ErrorContext(ctx, "read the deployment that retries", "deployment_id", id, "error", err)
		return id, nil
	}
	return next.ID, nil
}

// observation returns the last cycle's observation of one job.
func (e *Engine) observation(key jobKey) (Observation, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	o, ok := e.observations[key]
	return o, ok
}
