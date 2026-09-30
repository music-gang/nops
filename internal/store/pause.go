package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrAlreadyPaused is returned by Pause when the job is already paused.
	ErrAlreadyPaused = errors.New("job is already paused")
	// ErrNotPaused is returned by Resume when the job is not paused.
	ErrNotPaused = errors.New("job is not paused")
)

// Pause is one pause of a job: who paused it, when and why, and, once it is
// over, who resumed it and when. A pause that is still in force has a zero
// ResumedAt.
type Pause struct {
	Namespace string
	JobID     string
	PausedBy  string
	PausedAt  time.Time
	// Reason is what the person said when pausing; it may be empty.
	Reason    string
	ResumedBy string
	ResumedAt time.Time // zero while the pause is in force
}

// PauseJob records that actor paused the job: nops starts no new deployment for
// it until ResumeJob (docs/deployment-lifecycle.md). It does not touch Nomad, and a
// deployment already running is not its business. It returns ErrAlreadyPaused
// if the job is already paused, the per-job one-pause rule being the database's
// (a partial unique index).
func (s *Store) PauseJob(ctx context.Context, namespace, jobID, actor, reason string) error {
	if actor == "" {
		return errors.New("pause job: actor is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_pauses (namespace, job_id, paused_by, paused_at, reason)
		VALUES (?, ?, ?, ?, ?)`, namespace, jobID, actor, s.ts(), reason)
	if isUniqueViolation(err) {
		return fmt.Errorf("pause %s/%s: %w", namespace, jobID, ErrAlreadyPaused)
	}
	if err != nil {
		return fmt.Errorf("pause %s/%s: %w", namespace, jobID, err)
	}
	return nil
}

// ResumeJob records that actor lifted the job's pause, and returns ErrNotPaused
// if there was none. The row stays, as the record of the pause.
func (s *Store) ResumeJob(ctx context.Context, namespace, jobID, actor string) error {
	if actor == "" {
		return errors.New("resume job: actor is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE job_pauses SET resumed_by = ?, resumed_at = ?
		WHERE namespace = ? AND job_id = ? AND resumed_at IS NULL`, actor, s.ts(), namespace, jobID)
	if err != nil {
		return fmt.Errorf("resume %s/%s: %w", namespace, jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resume %s/%s: %w", namespace, jobID, err)
	}
	if n == 0 {
		return fmt.Errorf("resume %s/%s: %w", namespace, jobID, ErrNotPaused)
	}
	return nil
}

// ActivePauses returns the pauses in force, oldest first.
func (s *Store) ActivePauses(ctx context.Context) ([]Pause, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT namespace, job_id, paused_by, paused_at, reason
		FROM job_pauses WHERE resumed_at IS NULL ORDER BY paused_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list active pauses: %w", err)
	}
	defer rows.Close()
	var out []Pause
	for rows.Next() {
		var p Pause
		var at string
		if err := rows.Scan(&p.Namespace, &p.JobID, &p.PausedBy, &at, &p.Reason); err != nil {
			return nil, fmt.Errorf("scan pause: %w", err)
		}
		if p.PausedAt, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("parse pause time %q: %w", at, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active pauses: %w", err)
	}
	return out, nil
}

// PauseOf returns the pause in force on one job, or ErrNotFound if there is none.
func (s *Store) PauseOf(ctx context.Context, namespace, jobID string) (Pause, error) {
	p := Pause{Namespace: namespace, JobID: jobID}
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT paused_by, paused_at, reason FROM job_pauses
		WHERE namespace = ? AND job_id = ? AND resumed_at IS NULL`, namespace, jobID).Scan(&p.PausedBy, &at, &p.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return Pause{}, fmt.Errorf("pause of %s/%s: %w", namespace, jobID, ErrNotFound)
	}
	if err != nil {
		return Pause{}, fmt.Errorf("pause of %s/%s: %w", namespace, jobID, err)
	}
	if p.PausedAt, err = parseTime(at); err != nil {
		return Pause{}, fmt.Errorf("parse pause time %q: %w", at, err)
	}
	return p, nil
}
