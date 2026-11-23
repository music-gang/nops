package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/music-gang/nops/internal/store"
)

// ErrNotOrphan is returned by DismissOrphan when the job is not an orphan as
// of the last detection cycle, or its last deployment is not the one asked:
// the page it was asked from is older than that.
var ErrNotOrphan = errors.New("the job is not an orphan as of the last detection cycle")

// DismissOrphan records that a person removed the job from git on purpose
// and lets it run unmanaged: nops stops reporting it until a new deployment of
// it completes (docs/architecture.md#orphan-jobs). deploymentID is the last
// deployment the person saw. It does not touch Nomad. The dismissal is
// persisted first (invariant 7), then the job leaves the orphans, and the
// detection loop is asked for a cycle, so one already running can't bring it
// back.
//
// It returns ErrNotOrphan if the job is not an orphan with that last
// deployment, store.ErrAlreadyDismissed if it is already dismissed and
// store.ErrNotFound for a job of a namespace nops does not manage.
func (e *Engine) DismissOrphan(ctx context.Context, namespace, jobID, deploymentID, actor string) error {
	if actor == "" {
		return errors.New("dismiss orphan: actor is required")
	}
	if !e.managedNS[namespace] {
		return fmt.Errorf("dismiss %s/%s: %w", namespace, jobID, store.ErrNotFound)
	}
	if !e.isOrphan(namespace, jobID, deploymentID) {
		return fmt.Errorf("dismiss %s/%s: %w", namespace, jobID, ErrNotOrphan)
	}
	err := e.store.DismissOrphan(ctx, namespace, jobID, deploymentID, actor)
	switch {
	case errors.Is(err, store.ErrNotLatestCompleted):
		return fmt.Errorf("dismiss %s/%s: %w", namespace, jobID, ErrNotOrphan)
	case err != nil:
		return err
	}
	e.log.InfoContext(ctx, "orphan dismissed", "job", jobID, "namespace", namespace, "deployment", deploymentID, "actor", actor)
	e.dropOrphan(namespace, jobID)
	e.kickDetection()
	return nil
}

// UndismissOrphan deletes the job's dismissal: the detection cycle, asked for
// now, reports it again if it is still an orphan. It returns
// store.ErrNotDismissed if there is no dismissal, and store.ErrNotFound for a
// job of a namespace nops does not manage.
func (e *Engine) UndismissOrphan(ctx context.Context, namespace, jobID, actor string) error {
	if actor == "" {
		return errors.New("undismiss orphan: actor is required")
	}
	if !e.managedNS[namespace] {
		return fmt.Errorf("undismiss %s/%s: %w", namespace, jobID, store.ErrNotFound)
	}
	if err := e.store.UndismissOrphan(ctx, namespace, jobID); err != nil {
		return err
	}
	e.log.InfoContext(ctx, "orphan dismissal undone", "job", jobID, "namespace", namespace, "actor", actor)
	e.kickDetection()
	return nil
}

// isOrphan reports whether the last detection cycle found the job an orphan
// whose last deployment is deploymentID.
func (e *Engine) isOrphan(namespace, jobID, deploymentID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, o := range e.orphans {
		if o.Namespace == namespace && o.JobID == jobID {
			return o.LastDeploymentID == deploymentID
		}
	}
	return false
}

// dropOrphan takes the job out of the orphans and their count, ahead of the
// next cycle, which recomputes them from the store anyway.
func (e *Engine) dropOrphan(namespace, jobID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := make([]Orphan, 0, len(e.orphans))
	for _, o := range e.orphans {
		if o.Namespace != namespace || o.JobID != jobID {
			next = append(next, o)
		}
	}
	e.orphans = next
	e.status.Orphans = len(next)
}
