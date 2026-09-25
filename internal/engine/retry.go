package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/music-gang/nops/internal/store"
)

// ErrNotBlocked is returned by Retry when the job is not currently blocked by
// its latest deployment: there is nothing to retry (it may have been unblocked
// by a new commit or a retry since the page was rendered).
var ErrNotBlocked = errors.New("job is not blocked by its latest deployment")

// Retry lets a human unblock a job whose drift is suppressed by the anti-loop
// rule (a failed or rejected deployment for the same spec, see
// docs/design/engine-apply.md, decisions 6 and 7) without a new commit.
//
// It does not create a deployment and does not touch Nomad: it marks the
// blocking deployment as retried (persisted first, invariant 7), so that the
// rule stops counting it, and asks the detection loop for a cycle now. That
// cycle creates a new deployment like any other, which follows the policy:
// under "approval" it waits for a human decision (invariant 3).
//
// It returns ErrNotBlocked if the job is not blocked by its latest deployment
// and store.ErrAlreadyRetried if that deployment was already retried.
func (e *Engine) Retry(ctx context.Context, namespace, jobID, actor string) error {
	if actor == "" {
		return errors.New("retry: actor is required")
	}
	if namespace != e.namespace {
		return fmt.Errorf("retry %s/%s: %w", namespace, jobID, store.ErrNotFound)
	}

	obs, ok := e.observation(jobID)
	if !ok || obs.BlockedBy == "" {
		return fmt.Errorf("retry %s/%s: %w", namespace, jobID, ErrNotBlocked)
	}
	// The observation is as old as the last cycle: a deployment created since
	// then means it no longer describes the job.
	latest, err := e.store.LatestDeployment(ctx, namespace, jobID)
	if err != nil {
		return fmt.Errorf("retry %s/%s: %w", namespace, jobID, err)
	}
	if latest.ID != obs.BlockedBy {
		return fmt.Errorf("retry %s/%s: %w", namespace, jobID, ErrNotBlocked)
	}

	if err := e.store.MarkRetried(ctx, latest.ID, actor); err != nil {
		return fmt.Errorf("retry %s/%s: %w", namespace, jobID, err)
	}
	e.log.InfoContext(ctx, "deployment retried", "deployment_id", latest.ID, "job", jobID, "actor", actor)

	e.kickDetection()
	return nil
}

// observation returns the last cycle's observation of one job.
func (e *Engine) observation(jobID string) (Observation, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	o, ok := e.observations[jobID]
	return o, ok
}
