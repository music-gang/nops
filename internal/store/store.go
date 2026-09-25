// Package store persists deployments, hook runs and the audit log in SQLite.
//
// Every state change of a deployment goes through Store.Transition, which
// updates the row and appends an event in one transaction. There is no other
// way to change Deployment.State.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var (
	// ErrActiveDeployment is returned by CreateDeployment when the job already
	// has a deployment in an active state (the per-job lock).
	ErrActiveDeployment = errors.New("job already has an active deployment")
	// ErrNotFound is returned when a row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrStateConflict is returned by Transition when the deployment is not in
	// the expected state (someone else moved it first).
	ErrStateConflict = errors.New("deployment is not in the expected state")
	// ErrInvalidTransition is returned by Transition for a move the state
	// machine does not allow.
	ErrInvalidTransition = errors.New("invalid state transition")
	// ErrAlreadyRetried is returned by MarkRetried when the deployment was
	// already retried.
	ErrAlreadyRetried = errors.New("deployment was already retried")
)

// State is the state of a deployment.
type State string

const (
	StateDetected        State = "detected"
	StatePendingApproval State = "pending_approval"
	StatePreHook         State = "pre_hook"
	StateApplying        State = "applying"
	StatePostHook        State = "post_hook"
	StateCompleted       State = "completed"
	StateFailed          State = "failed"
	StateRejected        State = "rejected"
	StateSuperseded      State = "superseded"
)

// IsActive reports whether the state holds the per-job lock.
func (s State) IsActive() bool {
	switch s {
	case StateDetected, StatePendingApproval, StatePreHook, StateApplying, StatePostHook:
		return true
	}
	return false
}

// allowed lists the legal transitions. Terminal states have no exits.
var allowed = map[State][]State{
	StateDetected:        {StatePendingApproval, StatePreHook, StateApplying, StateCompleted, StateFailed, StateSuperseded},
	StatePendingApproval: {StatePreHook, StateApplying, StateRejected, StateSuperseded, StateCompleted},
	StatePreHook:         {StateApplying, StateFailed},
	StateApplying:        {StatePostHook, StateCompleted, StateFailed},
	StatePostHook:        {StateCompleted, StateFailed},
}

func canTransition(from, to State) bool {
	for _, s := range allowed[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Policy of a deployment. PolicyNone never creates a deployment.
type Policy string

const (
	PolicyAuto     Policy = "auto"
	PolicyApproval Policy = "approval"
)

// Deployment is one attempt to bring a job to a given spec.
type Deployment struct {
	ID        string
	JobID     string
	Namespace string
	CommitSHA string
	// CommitSubject and CommitAuthor describe CommitSHA (first line of the
	// message, author name); empty for a deployment created before they
	// were recorded.
	CommitSubject string
	CommitAuthor  string
	SpecHash      string
	JobSpec       string // JSON of the parsed job to register
	PlanDiff      string // JSON of the redacted plan diff; may be empty
	Policy        Policy
	State         State
	CASIndex      uint64
	AppliedIndex  uint64
	EvalID        string
	Error         string
	DecidedBy     string
	DecidedAt     time.Time // zero if not decided
	// RetriedBy and RetriedAt are set by MarkRetried on a failed or rejected
	// deployment a human asked to retry: detection stops treating it as the
	// reason a job's drift is blocked.
	RetriedBy string
	RetriedAt time.Time // zero if not retried
	CreatedAt time.Time
	UpdatedAt time.Time
}

// HookState is the state of a hook run.
type HookState string

const (
	HookDispatching HookState = "dispatching"
	HookRunning     HookState = "running"
	HookSucceeded   HookState = "succeeded"
	HookFailed      HookState = "failed"
	HookTimedOut    HookState = "timed_out"
)

// HookRun is the execution of a pre or post hook for a deployment.
type HookRun struct {
	ID               string
	DeploymentID     string
	Phase            string // "pre" or "post"
	HookJobID        string
	IdempotencyToken string
	DispatchedJobID  string
	State            HookState
	Timeout          time.Duration
	Error            string
	StartedAt        time.Time
	FinishedAt       time.Time // zero if not finished
}

// Event is one row of the audit log.
type Event struct {
	ID           int64
	DeploymentID string
	Time         time.Time
	From, To     State
	Actor        string
	Message      string
}

// Transition describes a state change. Optional fields are only written when
// non-zero.
type Transition struct {
	// From is the state the deployment must currently be in (required).
	From State
	// Actor is who caused the change: a user name or "nops" (required).
	Actor   string
	Message string

	Error        string
	AppliedIndex uint64
	EvalID       string
	// DecidedBy records a human decision (approve/reject); it also sets decided_at.
	DecidedBy string
}

// Store is a SQLite-backed store. It is safe for concurrent use.
type Store struct {
	db    *sql.DB
	now   func() time.Time
	newID func() string
}

// Option configures Open.
type Option func(*Store)

// WithClock injects the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// dbFileMode is the permission the database file (and its WAL/SHM sidecars)
// are kept at: job_spec holds the full job spec, unredacted, so it can carry
// the same secrets as the plan diff (see docs/design/engine-detection.md).
const dbFileMode = 0o600

// Open opens (creating if needed) the database at path and applies
// migrations. The file, and its WAL/SHM sidecars once they exist, are kept at
// dbFileMode: job_spec is not redacted.
func Open(path string, opts ...Option) (*Store, error) {
	if err := ensureFileMode(path, dbFileMode); err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// One connection: writes are serialized anyway and it avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	s := &Store{
		db:    db,
		now:   time.Now,
		newID: func() string { return ulid.Make().String() },
	}
	for _, o := range opts {
		o(s)
	}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	// WAL/SHM are only created on the first write (by migrate); chmod them now
	// that they exist. Best-effort: a failure here does not fail Open.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Chmod(path+suffix, dbFileMode)
	}
	return s, nil
}

// ensureFileMode makes sure path exists at mode, creating an empty file if
// needed and fixing the mode of one that already exists (a pre-existing file
// may predate this rule, or have been created with a looser umask).
func ensureFileMode(path string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, mode)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) ts() string { return s.now().UTC().Format(time.RFC3339Nano) }

func (s *Store) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for _, name := range names {
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad version prefix: %w", name, err)
		}
		if v <= current {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migration %s: begin: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		// PRAGMA does not accept bound parameters; v comes from an embedded file name.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: set version: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: commit: %w", name, err)
		}
		current = v
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// CreateDeployment inserts d in state detected and logs the first event.
// ID, State, CreatedAt and UpdatedAt are set by the store. It returns
// ErrActiveDeployment if the job already holds the per-job lock.
func (s *Store) CreateDeployment(ctx context.Context, d *Deployment) error {
	if d.JobID == "" || d.Namespace == "" || d.SpecHash == "" || d.JobSpec == "" {
		return errors.New("create deployment: job_id, namespace, spec_hash and job_spec are required")
	}
	if d.Policy != PolicyAuto && d.Policy != PolicyApproval {
		return fmt.Errorf("create deployment: invalid policy %q", d.Policy)
	}
	id := s.newID()
	now := s.ts()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `INSERT INTO deployments
		(id, job_id, namespace, commit_sha, commit_subject, commit_author, spec_hash, job_spec, plan_diff,
		 policy, state, cas_index, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`,
		id, d.JobID, d.Namespace, d.CommitSHA, d.CommitSubject, d.CommitAuthor, d.SpecHash, d.JobSpec, d.PlanDiff,
		string(d.Policy), string(StateDetected), int64(d.CASIndex), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %s/%s", ErrActiveDeployment, d.Namespace, d.JobID)
		}
		return fmt.Errorf("create deployment: %w", err)
	}
	if err := insertEvent(ctx, tx, id, now, "", StateDetected, "nops", fmt.Sprintf("detected at commit %s", d.CommitSHA)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create deployment: commit: %w", err)
	}

	t, _ := parseTime(now)
	d.ID, d.State, d.CreatedAt, d.UpdatedAt = id, StateDetected, t, t
	return nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, depID, ts string, from, to State, actor, msg string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO events (deployment_id, ts, from_state, to_state, actor, message) VALUES (?, ?, ?, ?, ?, ?)`,
		depID, ts, string(from), string(to), actor, msg)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// Transition moves a deployment to state `to`, atomically with its event.
// It returns ErrInvalidTransition if the state machine forbids from→to, and
// ErrStateConflict if the deployment is no longer in t.From.
func (s *Store) Transition(ctx context.Context, id string, to State, t Transition) error {
	if t.From == "" || t.Actor == "" {
		return errors.New("transition: From and Actor are required")
	}
	if !canTransition(t.From, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.From, to)
	}
	now := s.ts()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("transition %s: %w", id, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE deployments SET
			state = ?,
			updated_at = ?,
			error = COALESCE(NULLIF(?, ''), error),
			applied_index = COALESCE(NULLIF(?, 0), applied_index),
			eval_id = COALESCE(NULLIF(?, ''), eval_id),
			decided_by = COALESCE(NULLIF(?, ''), decided_by),
			decided_at = CASE WHEN ? <> '' THEN ? ELSE decided_at END
		WHERE id = ? AND state = ?`,
		string(to), now, t.Error, int64(t.AppliedIndex), t.EvalID, t.DecidedBy, t.DecidedBy, now,
		id, string(t.From))
	if err != nil {
		return fmt.Errorf("transition %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var cur string
		err := tx.QueryRowContext(ctx, `SELECT state FROM deployments WHERE id = ?`, id).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("transition %s: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("transition %s: %w", id, err)
		}
		return fmt.Errorf("transition %s: %w (expected %s, is %s)", id, ErrStateConflict, t.From, cur)
	}

	msg := t.Message
	if msg == "" {
		msg = t.Error
	}
	if err := insertEvent(ctx, tx, id, now, t.From, to, t.Actor, msg); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("transition %s: commit: %w", id, err)
	}
	return nil
}

// SetApplied records the applied index (and, when known, the eval ID) of a
// deployment while it stays applying, without changing its state: invariant
// 7 requires them persisted before nops starts waiting for health, and
// Store.Transition only writes when the state itself changes. evalID may be
// empty (a crash recovery that finds the register already done, without a
// fresh eval to record). It returns ErrStateConflict if the deployment is no
// longer applying, or ErrNotFound if it does not exist.
func (s *Store) SetApplied(ctx context.Context, id string, appliedIndex uint64, evalID string) error {
	if appliedIndex == 0 {
		return errors.New("set applied: appliedIndex is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE deployments SET
			applied_index = ?,
			eval_id = COALESCE(NULLIF(?, ''), eval_id),
			updated_at = ?
		WHERE id = ? AND state = ?`,
		int64(appliedIndex), evalID, s.ts(), id, string(StateApplying))
	if err != nil {
		return fmt.Errorf("set applied %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetDeployment(ctx, id); err != nil {
			return fmt.Errorf("set applied %s: %w", id, err)
		}
		return fmt.Errorf("set applied %s: %w", id, ErrStateConflict)
	}
	return nil
}

// AppliedSince returns the timestamp of a deployment's "-> applying" event:
// the apply timeout is counted from it (docs/state-machine.md), so a restart
// does not extend it. It returns ErrNotFound if the deployment never reached
// applying.
func (s *Store) AppliedSince(ctx context.Context, deploymentID string) (time.Time, error) {
	var ts string
	err := s.db.QueryRowContext(ctx, `SELECT ts FROM events
		WHERE deployment_id = ? AND to_state = ? ORDER BY id DESC LIMIT 1`,
		deploymentID, string(StateApplying)).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("applied since %s: %w", deploymentID, ErrNotFound)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("applied since %s: %w", deploymentID, err)
	}
	return parseTime(ts)
}

const deploymentCols = `id, job_id, namespace, commit_sha, commit_subject, commit_author, spec_hash, job_spec,
	plan_diff, policy, state, cas_index, applied_index, eval_id, error, decided_by, decided_at,
	retried_by, retried_at, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanDeployment(r scanner) (*Deployment, error) {
	var (
		d                                   Deployment
		plan, eval, errMsg, by, decidedAt   sql.NullString
		subject, author, retriedBy, retried sql.NullString
		applied                             sql.NullInt64
		cas                                 int64
		policy, state, createdAt, updatedAt string
	)
	if err := r.Scan(&d.ID, &d.JobID, &d.Namespace, &d.CommitSHA, &subject, &author, &d.SpecHash, &d.JobSpec, &plan,
		&policy, &state, &cas, &applied, &eval, &errMsg, &by, &decidedAt, &retriedBy, &retried,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	d.PlanDiff, d.EvalID, d.Error, d.DecidedBy = plan.String, eval.String, errMsg.String, by.String
	d.CommitSubject, d.CommitAuthor, d.RetriedBy = subject.String, author.String, retriedBy.String
	d.Policy, d.State, d.CASIndex, d.AppliedIndex = Policy(policy), State(state), uint64(cas), uint64(applied.Int64)
	var err error
	if d.DecidedAt, err = parseTime(decidedAt.String); err != nil {
		return nil, err
	}
	if d.RetriedAt, err = parseTime(retried.String); err != nil {
		return nil, err
	}
	if d.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if d.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// GetDeployment returns a deployment by ID, or ErrNotFound.
func (s *Store) GetDeployment(ctx context.Context, id string) (*Deployment, error) {
	d, err := scanDeployment(s.db.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("deployment %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment %s: %w", id, err)
	}
	return d, nil
}

// ActiveDeployment returns the active deployment of a job, or ErrNotFound.
func (s *Store) ActiveDeployment(ctx context.Context, namespace, jobID string) (*Deployment, error) {
	d, err := scanDeployment(s.db.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE namespace = ? AND job_id = ? AND state IN ('detected','pending_approval','pre_hook','applying','post_hook')`,
		namespace, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("active deployment %s/%s: %w", namespace, jobID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("active deployment %s/%s: %w", namespace, jobID, err)
	}
	return d, nil
}

// LatestDeployment returns the most recently created deployment of a job,
// whatever its state, or ErrNotFound if the job never had one. It is how the
// engine tells a fresh failure from one it already knows about (see
// docs/design/engine-detection.md).
func (s *Store) LatestDeployment(ctx context.Context, namespace, jobID string) (*Deployment, error) {
	d, err := scanDeployment(s.db.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE namespace = ? AND job_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		namespace, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("latest deployment %s/%s: %w", namespace, jobID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("latest deployment %s/%s: %w", namespace, jobID, err)
	}
	return d, nil
}

func (s *Store) queryDeployments(ctx context.Context, q string, args ...any) ([]*Deployment, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query deployments: %w", err)
	}
	defer rows.Close()
	var out []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListActive returns all deployments holding a per-job lock, oldest first.
func (s *Store) ListActive(ctx context.Context) ([]*Deployment, error) {
	return s.queryDeployments(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE state IN ('detected','pending_approval','pre_hook','applying','post_hook')
		ORDER BY created_at, id`)
}

// ListByState returns the deployments in a state, oldest first.
func (s *Store) ListByState(ctx context.Context, st State) ([]*Deployment, error) {
	return s.queryDeployments(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE state = ? ORDER BY created_at, id`, string(st))
}

// ListByJob returns the deployments of one job, whatever their state, newest
// first.
func (s *Store) ListByJob(ctx context.Context, namespace, jobID string, limit int) ([]*Deployment, error) {
	return s.queryDeployments(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE namespace = ? AND job_id = ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, namespace, jobID, limit)
}

// LatestPerJob returns the most recent deployment of every job that ever had
// one, whatever its state, ordered by namespace and job ID.
func (s *Store) LatestPerJob(ctx context.Context) ([]*Deployment, error) {
	return s.queryDeployments(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE id = (SELECT x.id FROM deployments x
			WHERE x.namespace = deployments.namespace AND x.job_id = deployments.job_id
			ORDER BY x.created_at DESC, x.id DESC LIMIT 1)
		ORDER BY namespace, job_id`)
}

// LatestCompletedPerJob returns, for every job of a namespace that ever had a
// `completed` deployment, the most recent of those, ordered by job ID. It is
// what nops has put into production: the jobs it is answerable for, and the
// only ones it looks for after they leave the repository.
func (s *Store) LatestCompletedPerJob(ctx context.Context, namespace string) ([]*Deployment, error) {
	return s.queryDeployments(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE namespace = ? AND state = 'completed'
		  AND id = (SELECT x.id FROM deployments x
			WHERE x.namespace = deployments.namespace AND x.job_id = deployments.job_id AND x.state = 'completed'
			ORDER BY x.created_at DESC, x.id DESC LIMIT 1)
		ORDER BY job_id`, namespace)
}

// MarkRetried records that actor asked to retry a failed or rejected
// deployment, and logs an event (from and to are both the deployment's state:
// nothing moves, it is the audit trail of the decision). A deployment in any
// other state is ErrStateConflict, one already retried is ErrAlreadyRetried,
// a missing one is ErrNotFound. It does not touch Nomad: it only stops the
// deployment from blocking its job's next one (docs/state-machine.md).
func (s *Store) MarkRetried(ctx context.Context, id, actor string) error {
	if actor == "" {
		return errors.New("mark retried: actor is required")
	}
	now := s.ts()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mark retried %s: %w", id, err)
	}
	defer tx.Rollback()

	var state string
	err = tx.QueryRowContext(ctx, `UPDATE deployments SET retried_by = ?, retried_at = ?, updated_at = ?
		WHERE id = ? AND state IN ('failed','rejected') AND retried_at IS NULL
		RETURNING state`, actor, now, now, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		var cur string
		var retried sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT state, retried_at FROM deployments WHERE id = ?`, id).Scan(&cur, &retried)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("mark retried %s: %w", id, ErrNotFound)
		case err != nil:
			return fmt.Errorf("mark retried %s: %w", id, err)
		case retried.Valid:
			return fmt.Errorf("mark retried %s: %w", id, ErrAlreadyRetried)
		}
		return fmt.Errorf("mark retried %s: %w (is %s, want failed or rejected)", id, ErrStateConflict, cur)
	}
	if err != nil {
		return fmt.Errorf("mark retried %s: %w", id, err)
	}
	if err := insertEvent(ctx, tx, id, now, State(state), State(state), actor, "retry requested"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark retried %s: commit: %w", id, err)
	}
	return nil
}

// ListHistory returns terminal deployments, newest first.
func (s *Store) ListHistory(ctx context.Context, limit int) ([]*Deployment, error) {
	return s.queryDeployments(ctx, `SELECT `+deploymentCols+` FROM deployments
		WHERE state IN ('completed','failed','rejected','superseded')
		ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
}

// Events returns the audit log of a deployment in order.
func (s *Store) Events(ctx context.Context, deploymentID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, deployment_id, ts, from_state, to_state, actor, message FROM events WHERE deployment_id = ? ORDER BY id`,
		deploymentID)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var (
			e        Event
			ts       string
			from, to string
		)
		if err := rows.Scan(&e.ID, &e.DeploymentID, &ts, &from, &to, &e.Actor, &e.Message); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		var perr error
		if e.Time, perr = parseTime(ts); perr != nil {
			return nil, perr
		}
		e.From, e.To = State(from), State(to)
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnsureHookRun creates the hook run for (deploymentID, phase) in state
// dispatching, or returns the existing one. It is the idempotent entry point
// used before dispatching, so a restarted controller finds the earlier run.
// created is true if the row was just inserted.
func (s *Store) EnsureHookRun(ctx context.Context, deploymentID, phase, hookJobID string, timeout time.Duration) (run *HookRun, created bool, err error) {
	if phase != "pre" && phase != "post" {
		return nil, false, fmt.Errorf("ensure hook run: invalid phase %q", phase)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO hook_runs
		(id, deployment_id, phase, hook_job_id, idempotency_token, state, timeout_s, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (deployment_id, phase) DO NOTHING`,
		s.newID(), deploymentID, phase, hookJobID, deploymentID+":"+phase, string(HookDispatching),
		int64(timeout/time.Second), s.ts())
	if err != nil {
		return nil, false, fmt.Errorf("ensure hook run %s/%s: %w", deploymentID, phase, err)
	}
	n, _ := res.RowsAffected()
	run, err = s.GetHookRun(ctx, deploymentID, phase)
	if err != nil {
		return nil, false, err
	}
	return run, n == 1, nil
}

// GetHookRun returns the hook run of a deployment phase, or ErrNotFound.
func (s *Store) GetHookRun(ctx context.Context, deploymentID, phase string) (*HookRun, error) {
	var (
		h                  HookRun
		dispatched, errMsg sql.NullString
		finished           sql.NullString
		state, started     string
		timeoutS           int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, deployment_id, phase, hook_job_id, idempotency_token,
			dispatched_job_id, state, timeout_s, error, started_at, finished_at
		FROM hook_runs WHERE deployment_id = ? AND phase = ?`, deploymentID, phase).
		Scan(&h.ID, &h.DeploymentID, &h.Phase, &h.HookJobID, &h.IdempotencyToken,
			&dispatched, &state, &timeoutS, &errMsg, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("hook run %s/%s: %w", deploymentID, phase, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get hook run %s/%s: %w", deploymentID, phase, err)
	}
	h.DispatchedJobID, h.Error, h.State = dispatched.String, errMsg.String, HookState(state)
	h.Timeout = time.Duration(timeoutS) * time.Second
	if h.StartedAt, err = parseTime(started); err != nil {
		return nil, err
	}
	if h.FinishedAt, err = parseTime(finished.String); err != nil {
		return nil, err
	}
	return &h, nil
}

// HookUpdate changes a hook run. Optional fields are written only when non-zero.
type HookUpdate struct {
	State           HookState // required
	DispatchedJobID string
	Error           string
}

// UpdateHookRun records progress of a hook run (dispatched child job, final
// outcome). Terminal states set finished_at.
func (s *Store) UpdateHookRun(ctx context.Context, runID string, u HookUpdate) error {
	if u.State == "" {
		return errors.New("update hook run: State is required")
	}
	finished := ""
	switch u.State {
	case HookSucceeded, HookFailed, HookTimedOut:
		finished = s.ts()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE hook_runs SET
			state = ?,
			dispatched_job_id = COALESCE(NULLIF(?, ''), dispatched_job_id),
			error = COALESCE(NULLIF(?, ''), error),
			finished_at = COALESCE(NULLIF(?, ''), finished_at)
		WHERE id = ?`, string(u.State), u.DispatchedJobID, u.Error, finished, runID)
	if err != nil {
		return fmt.Errorf("update hook run %s: %w", runID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("update hook run %s: %w", runID, ErrNotFound)
	}
	return nil
}
