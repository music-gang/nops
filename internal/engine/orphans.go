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
// present holds the key of every job parsed from the snapshot, whatever its
// classification: a job still in the repository without nops_managed means
// "hands off", not "removed", and two files claiming one ID are both still
// there. Only the jobs nops has a completed deployment for are candidates, and
// each is looked up in Nomad, in its own namespace: gone (purged) or stopped
// or dead is not an orphan. Each managed namespace is checked. It never changes
// anything in Nomad.
//
// A Nomad failure on one candidate is an ERROR and skips that job; a store
// failure is returned (never swallow a SQLite error).
func (e *Engine) findOrphans(ctx context.Context, present map[jobKey]bool) ([]Orphan, error) {
	var orphans []Orphan
	for _, ns := range e.namespaces {
		deployed, err := e.store.LatestCompletedPerJob(ctx, ns)
		if err != nil {
			return nil, fmt.Errorf("list deployed jobs of namespace %s: %w", ns, err)
		}
		for _, d := range deployed {
			if present[jobKey{ns, d.JobID}] {
				continue
			}
			live, err := e.nomad.Job(ctx, ns, d.JobID)
			switch {
			case errors.Is(err, nomadx.ErrJobNotFound):
				continue // purged: nothing left to report
			case err != nil:
				e.log.ErrorContext(ctx, "orphan check: get live job", "job", d.JobID, "namespace", ns, "error", err)
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
				JobID: d.JobID, Namespace: ns, Policy: d.Policy, LastDeploymentID: d.ID,
				NomadStatus: status, ObservedAt: e.now(),
			})
		}
	}
	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].Namespace != orphans[j].Namespace {
			return orphans[i].Namespace < orphans[j].Namespace
		}
		return orphans[i].JobID < orphans[j].JobID
	})
	return orphans, nil
}
