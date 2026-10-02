package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// WindowLift is a person's request to deploy a job held by its sync window
// now (docs/policies.md, "Sync window"): it lifts the window for the one spec
// they saw, once. It waits until detection uses or drops it.
type WindowLift struct {
	Namespace   string
	JobID       string
	SpecHash    string
	RequestedBy string
	RequestedAt time.Time
}

// RequestWindowLift records that actor asked to deploy the job outside its
// sync window, for the spec with specHash. The job has at most one request: a
// new one replaces the one waiting. It does not touch Nomad.
func (s *Store) RequestWindowLift(ctx context.Context, namespace, jobID, specHash, actor string) error {
	if actor == "" {
		return errors.New("request window lift: actor is required")
	}
	if specHash == "" {
		return errors.New("request window lift: spec hash is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO window_lifts (namespace, job_id, spec_hash, requested_by, requested_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (namespace, job_id) DO UPDATE SET
			spec_hash = excluded.spec_hash, requested_by = excluded.requested_by, requested_at = excluded.requested_at`,
		namespace, jobID, specHash, actor, s.ts())
	if err != nil {
		return fmt.Errorf("request window lift %s/%s: %w", namespace, jobID, err)
	}
	return nil
}

// WindowLifts returns the requests waiting, oldest first.
func (s *Store) WindowLifts(ctx context.Context) ([]WindowLift, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT namespace, job_id, spec_hash, requested_by, requested_at
		FROM window_lifts ORDER BY requested_at, namespace, job_id`)
	if err != nil {
		return nil, fmt.Errorf("list window lifts: %w", err)
	}
	defer rows.Close()
	var out []WindowLift
	for rows.Next() {
		var l WindowLift
		var at string
		if err := rows.Scan(&l.Namespace, &l.JobID, &l.SpecHash, &l.RequestedBy, &at); err != nil {
			return nil, fmt.Errorf("scan window lift: %w", err)
		}
		if l.RequestedAt, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("parse window lift time %q: %w", at, err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list window lifts: %w", err)
	}
	return out, nil
}

// DropWindowLift deletes the job's request and reports whether there was one:
// a request the deployment it led to already used up is gone, and false says so.
func (s *Store) DropWindowLift(ctx context.Context, namespace, jobID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM window_lifts WHERE namespace = ? AND job_id = ?`, namespace, jobID)
	if err != nil {
		return false, fmt.Errorf("drop window lift %s/%s: %w", namespace, jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("drop window lift %s/%s: %w", namespace, jobID, err)
	}
	return n > 0, nil
}
