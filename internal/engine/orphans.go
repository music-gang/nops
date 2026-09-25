package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/music-gang/nops/internal/nomadx"
)

// findOrphans looks for the jobs nops deployed that are no longer in the
// repository and still run in Nomad (docs/design/engine-detection.md#orphan-jobs).
//
// present holds the ID of every job parsed from the snapshot, whatever its
// classification: a job still in the repository without nops_managed means
// "hands off", not "removed", and two files claiming one ID are both still
// there. Only the jobs nops has a completed deployment for are candidates, and
// each is looked up in Nomad: gone (purged) or stopped or dead is not an
// orphan. It never changes anything in Nomad.
//
// A Nomad failure on one candidate is an ERROR and skips that job; a store
// failure is returned (never swallow a SQLite error).
func (e *Engine) findOrphans(ctx context.Context, present map[string]bool) ([]Orphan, error) {
	deployed, err := e.store.LatestCompletedPerJob(ctx, e.namespace)
	if err != nil {
		return nil, fmt.Errorf("list deployed jobs: %w", err)
	}

	var orphans []Orphan
	for _, d := range deployed {
		if present[d.JobID] {
			continue
		}
		live, err := e.nomad.Job(ctx, d.JobID)
		switch {
		case errors.Is(err, nomadx.ErrJobNotFound):
			continue // purged: nothing left to report
		case err != nil:
			e.log.ErrorContext(ctx, "orphan check: get live job", "job", d.JobID, "namespace", e.namespace, "error", err)
			continue
		}
		if live.Stop != nil && *live.Stop {
			continue // stopped by someone: what the alert asks for
		}
		status := ""
		if live.Status != nil {
			status = *live.Status
		}
		if status == "dead" {
			continue // nothing runs and, with no schedule, nothing will
		}
		orphans = append(orphans, Orphan{
			JobID: d.JobID, Namespace: e.namespace, Policy: d.Policy, LastDeploymentID: d.ID,
			NomadStatus: status, ObservedAt: e.now(),
		})
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].JobID < orphans[j].JobID })
	return orphans, nil
}
