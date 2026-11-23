package engine

import (
	"context"
	"fmt"
)

// dropEndedSpecs drops the specs of the deployments that ended longer than
// specRetention ago, except the latest completed one of each job (see
// docs/architecture.md#detection): nothing reads them once a deployment has
// ended, since a retry reads the job from git again. With no retention it
// does nothing. A store failure is returned.
func (e *Engine) dropEndedSpecs(ctx context.Context) error {
	if e.specRetention <= 0 {
		return nil
	}
	n, err := e.store.DropEndedSpecs(ctx, e.now().Add(-e.specRetention))
	if n > 0 {
		e.log.InfoContext(ctx, "specs of ended deployments dropped", "deployments", n, "retention", e.specRetention)
	}
	if err != nil {
		return fmt.Errorf("drop the specs of ended deployments: %w", err)
	}
	return nil
}
