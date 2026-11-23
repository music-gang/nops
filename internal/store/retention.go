package store

import (
	"context"
	"fmt"
	"time"
)

// dropSpecsBatch is how many deployments one transaction of DropEndedSpecs
// drops the specs of, so a long backlog never holds the write lock for long.
var dropSpecsBatch = 500

// dropSpecsSQL drops the specs of one batch of the deployments that ended
// before a time, oldest first, less the latest completed one of each job.
// Arguments: now, before, batch size. INDEXED BY because the planner would
// otherwise pick deployments_by_state, which reads every ended deployment.
const dropSpecsSQL = `UPDATE deployments SET job_spec = '', job_source = NULL, job_vars = NULL, specs_dropped_at = ?
	WHERE id IN (SELECT d.id FROM deployments d INDEXED BY deployments_specs_to_drop
		WHERE d.specs_dropped_at IS NULL AND d.state IN ('completed', 'failed', 'rejected', 'superseded')
		  AND d.updated_at < ?
		  AND NOT EXISTS (SELECT 1 FROM latest_deployments l
			WHERE l.namespace = d.namespace AND l.job_id = d.job_id AND l.completed_id = d.id)
		ORDER BY d.updated_at LIMIT ?)
	RETURNING id`

// DropEndedSpecs empties the spec, the job file, the var-file and the hooks'
// specs of every deployment that ended before before, except the latest
// completed one of each job, and returns how many it dropped. The rest of the
// deployment stays: its plan diff, its events and its hook runs. It works in
// batches, each in its own transaction.
func (s *Store) DropEndedSpecs(ctx context.Context, before time.Time) (int, error) {
	total := 0
	for {
		n, err := s.dropSpecsBatch(ctx, formatTime(before))
		total += n
		if err != nil {
			return total, err
		}
		if n < dropSpecsBatch {
			return total, nil
		}
	}
}

func (s *Store) dropSpecsBatch(ctx context.Context, before string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("drop ended specs: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, dropSpecsSQL, s.ts(), before, dropSpecsBatch)
	if err != nil {
		return 0, fmt.Errorf("drop ended specs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("drop ended specs: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("drop ended specs: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("drop ended specs: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE deployment_hooks SET job_spec = '' WHERE deployment_id = ?`, id); err != nil {
			return 0, fmt.Errorf("drop ended specs of the hooks of %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("drop ended specs: commit: %w", err)
	}
	return len(ids), nil
}
