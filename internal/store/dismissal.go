package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrAlreadyDismissed is returned by DismissOrphan when the job is
	// already dismissed.
	ErrAlreadyDismissed = errors.New("job is already dismissed")
	// ErrNotDismissed is returned by UndismissOrphan when the job is not
	// dismissed.
	ErrNotDismissed = errors.New("job is not dismissed")
	// ErrNotLatestCompleted is returned by DismissOrphan when the deployment
	// is not the job's latest completed one.
	ErrNotLatestCompleted = errors.New("the deployment is not the job's latest completed one")
)

// Dismissal is a person's word that an orphan is not nops's business any
// more (docs/architecture.md#orphan-jobs): who said it and when, for the job's
// latest completed deployment at that moment. A new deployment of the job that
// completes deletes it.
type Dismissal struct {
	Namespace    string
	JobID        string
	DeploymentID string
	DismissedBy  string
	DismissedAt  time.Time
}

// DismissOrphan records that actor dismissed the job, whose latest completed
// deployment is deploymentID. It returns ErrNotLatestCompleted if it is not
// (the page it was asked from is older than a deployment), and
// ErrAlreadyDismissed if the job is already dismissed. It does not touch Nomad.
func (s *Store) DismissOrphan(ctx context.Context, namespace, jobID, deploymentID, actor string) error {
	if actor == "" {
		return errors.New("dismiss orphan: actor is required")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO orphan_dismissals (namespace, job_id, deployment_id, dismissed_by, dismissed_at)
		SELECT namespace, job_id, completed_id, ?, ? FROM latest_deployments
		WHERE namespace = ? AND job_id = ? AND completed_id = ?`,
		actor, s.ts(), namespace, jobID, deploymentID)
	if isUniqueViolation(err) {
		return fmt.Errorf("dismiss %s/%s: %w", namespace, jobID, ErrAlreadyDismissed)
	}
	if err != nil {
		return fmt.Errorf("dismiss %s/%s: %w", namespace, jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("dismiss %s/%s: %w", namespace, jobID, err)
	}
	if n == 0 {
		return fmt.Errorf("dismiss %s/%s: %w", namespace, jobID, ErrNotLatestCompleted)
	}
	return nil
}

// UndismissOrphan deletes the job's dismissal, and returns ErrNotDismissed if
// there was none.
func (s *Store) UndismissOrphan(ctx context.Context, namespace, jobID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM orphan_dismissals WHERE namespace = ? AND job_id = ?`, namespace, jobID)
	if err != nil {
		return fmt.Errorf("undismiss %s/%s: %w", namespace, jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("undismiss %s/%s: %w", namespace, jobID, err)
	}
	if n == 0 {
		return fmt.Errorf("undismiss %s/%s: %w", namespace, jobID, ErrNotDismissed)
	}
	return nil
}

// DismissalOf returns the job's dismissal, or ErrNotFound if there is none.
func (s *Store) DismissalOf(ctx context.Context, namespace, jobID string) (Dismissal, error) {
	d := Dismissal{Namespace: namespace, JobID: jobID}
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT deployment_id, dismissed_by, dismissed_at FROM orphan_dismissals
		WHERE namespace = ? AND job_id = ?`, namespace, jobID).Scan(&d.DeploymentID, &d.DismissedBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Dismissal{}, fmt.Errorf("dismissal of %s/%s: %w", namespace, jobID, ErrNotFound)
	}
	if err != nil {
		return Dismissal{}, fmt.Errorf("dismissal of %s/%s: %w", namespace, jobID, err)
	}
	if d.DismissedAt, err = parseTime(at); err != nil {
		return Dismissal{}, fmt.Errorf("parse dismissal time %q: %w", at, err)
	}
	return d, nil
}
