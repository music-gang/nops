package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/music-gang/nops/internal/nomadx"
)

// findOrphans looks for the jobs nops deployed that are no longer in the
// repository and still run in Nomad (docs/archive/engine-detection.md#orphan-jobs).
//
// present holds the key of every job parsed from the snapshot, whatever its
// classification: a job still in the repository without nops_managed means
// "hands off", not "removed", and two files claiming one ID are both still
// there. Only the jobs nops has a completed deployment for are candidates, and
// each is looked up in Nomad, in its own namespace: gone (purged) or stopped
// or dead is not an orphan. A job dismissed for that deployment
// (DismissOrphan) is no candidate, so it is not looked up. Each managed
// namespace is checked. It never changes anything in Nomad.
//
// A Nomad failure on one candidate is an ERROR and skips that job; a store
// failure is returned (never swallow a SQLite error).
func (dc *decider) findOrphans(ctx context.Context, c cycle, present map[jobKey]bool) ([]Orphan, error) {
	var orphans []Orphan
	for _, ns := range dc.namespaces {
		deployed, err := c.orphanCandidates(ctx, ns)
		if err != nil {
			return nil, fmt.Errorf("list deployed jobs of namespace %s: %w", ns, err)
		}
		for _, d := range deployed {
			if present[jobKey{ns, d.JobID}] {
				continue
			}
			live, err := dc.nomad.Job(ctx, ns, d.JobID)
			switch {
			case errors.Is(err, nomadx.ErrJobNotFound):
				continue // purged: nothing left to report
			case err != nil:
				c.log().ErrorContext(ctx, "orphan check: get live job", "job", d.JobID, "namespace", ns, "error", err)
				c.failed(FileError{Namespace: ns, JobID: d.JobID, Error: "orphan check: get live job: " + err.Error()})
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
				NomadStatus: status, ObservedAt: dc.now(),
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
