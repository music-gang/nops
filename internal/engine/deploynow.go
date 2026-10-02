package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/music-gang/nops/internal/store"
)

// ErrNotDeployable is returned by DeployNow when the job can't be deployed
// outside its sync window now: its window is not what holds it, it has nothing
// to deploy, a failed deployment blocks it or one is active, or git has another
// spec than the one asked. The page it was asked from may be older than that.
var ErrNotDeployable = errors.New("the job cannot be deployed outside its sync window now")

// DeployNowable reports whether DeployNow would take the job o observes, whose
// latest deployment is latest (nil if it never had one): a closed sync window
// holds it, it drifts, nothing blocks it and no deployment is active. A pause
// is the hold of a paused job, so a paused job is never deployable.
func DeployNowable(o Observation, latest *store.Deployment) bool {
	if o.Hold == nil || o.Hold.Kind != HoldWindow || !o.Drift || o.BlockedBy != "" {
		return false
	}
	return latest == nil || !latest.State.IsActive()
}

// DeployNow lets a person deploy a job held by its sync window without waiting
// for it to open (see docs/policies.md, "Sync window"). It does not touch
// Nomad: it records the request for the spec with specHash, the one the person
// saw (persisted first, invariant 7), and runs a detection cycle, which lifts
// the window for that spec only and creates the deployment like any other: it
// plans again, follows the policy and takes the live job's index. A request the
// cycle can't use, because git moved on, a pause came or nothing is left to
// deploy, is dropped and logged. The cycle runs here, not at the loop's next
// tick, because the page the person is sent back to reads its outcome.
//
// It returns the ID of the deployment that was created, or "" when there is
// none yet (the cycle failed and is left to the loop: the request is kept, and
// the observation says so). It returns ErrNotDeployable if the job can't be
// deployed now, and store.ErrNotFound for a job of a namespace nops does not
// manage.
func (e *Engine) DeployNow(ctx context.Context, namespace, jobID, specHash, actor string) (string, error) {
	if actor == "" {
		return "", errors.New("deploy now: actor is required")
	}
	if !e.managedNS[namespace] {
		return "", fmt.Errorf("deploy now %s/%s: %w", namespace, jobID, store.ErrNotFound)
	}
	key := jobKey{namespace, jobID}
	obs, ok := e.observation(key)
	if !ok || obs.SpecHash != specHash {
		return "", fmt.Errorf("deploy now %s/%s: %w", namespace, jobID, ErrNotDeployable)
	}
	latest, err := e.store.LatestDeployment(ctx, namespace, jobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		latest = nil
	case err != nil:
		return "", fmt.Errorf("deploy now %s/%s: %w", namespace, jobID, err)
	}
	if !DeployNowable(obs, latest) {
		return "", fmt.Errorf("deploy now %s/%s: %w", namespace, jobID, ErrNotDeployable)
	}

	if err := e.store.RequestWindowLift(ctx, namespace, jobID, specHash, actor); err != nil {
		return "", fmt.Errorf("deploy now %s/%s: %w", namespace, jobID, err)
	}
	e.log.InfoContext(ctx, "deploy now requested", "job", jobID, "namespace", namespace, "actor", actor)
	// The page the person is sent back to reads the last cycle's observation:
	// say so in it now, in case the cycle below fails.
	e.setDeployNowBy(key, actor)

	// The person may close the tab: a cycle stopped halfway is no cycle.
	if err := e.Detect(context.WithoutCancel(ctx)); err != nil {
		e.log.ErrorContext(ctx, "detection cycle after a deploy now aborted", "job", jobID, "namespace", namespace, "error", err)
		e.kickDetection()
		return "", nil
	}
	created, err := e.store.LatestDeployment(ctx, namespace, jobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", nil
	case err != nil:
		e.log.ErrorContext(ctx, "read the deployment a deploy now created", "job", jobID, "namespace", namespace, "error", err)
		return "", nil
	}
	if (latest == nil || created.ID != latest.ID) && created.WindowLiftedBy == actor {
		return created.ID, nil
	}
	return "", nil
}

// setDeployNowBy puts the request on a job's last observation, ahead of the
// next cycle, which replaces the observation. A job with no observation has
// nothing to update.
func (e *Engine) setDeployNowBy(key jobKey, actor string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o, ok := e.observations[key]; ok {
		o.DeployNowBy = actor
		e.observations[key] = o
	}
}
