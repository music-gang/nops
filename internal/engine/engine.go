// Package engine detects drift between the git repository and the Nomad
// cluster, creates deployments accordingly (see
// docs/design/engine-detection.md), and drives them from approval to
// completed (see docs/design/engine-apply.md). Recovery after a restart is the
// first RunApply cycle: every step resumes from persisted state alone, so it
// picks up whatever a crash left in flight.
package engine

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// Nomad is what the engine needs from Nomad. *nomadx.Client implements it.
type Nomad interface {
	ParseHCL(ctx context.Context, hcl, vars string) (*api.Job, error)
	Job(ctx context.Context, id string) (*api.Job, error)
	Plan(ctx context.Context, job *api.Job) (*api.JobPlanResponse, error)
	RegisterCAS(ctx context.Context, job *api.Job, modifyIndex uint64, preserveCounts bool) (*nomadx.RegisterResult, error)
	// Allocations and LatestDeployment are used by apply to decide whether the
	// applied job version is healthy (see docs/design/engine-apply.md).
	Allocations(ctx context.Context, jobID string) ([]nomadx.Alloc, error)
	LatestDeployment(ctx context.Context, jobID string) (*api.Deployment, error)
}

// Store is what the engine needs from the store. *store.Store implements it.
type Store interface {
	CreateDeployment(ctx context.Context, d *store.Deployment) error
	Transition(ctx context.Context, id string, to store.State, t store.Transition) error
	GetDeployment(ctx context.Context, id string) (*store.Deployment, error)
	ActiveDeployment(ctx context.Context, namespace, jobID string) (*store.Deployment, error)
	ListActive(ctx context.Context) ([]*store.Deployment, error)
	LatestDeployment(ctx context.Context, namespace, jobID string) (*store.Deployment, error)
	// LatestCompletedPerJob is used by the orphan check (see
	// docs/design/engine-detection.md#orphan-jobs).
	LatestCompletedPerJob(ctx context.Context, namespace string) ([]*store.Deployment, error)
	// MarkRetried is used by Retry (see docs/state-machine.md).
	MarkRetried(ctx context.Context, id, actor string) error
	// SetApplied and AppliedSince are used by apply (see docs/design/engine-apply.md).
	SetApplied(ctx context.Context, id string, appliedIndex uint64, evalID string) error
	AppliedSince(ctx context.Context, deploymentID string) (time.Time, error)
}

// Hooks is what apply needs to run a deployment's hooks. *hooks.Runner
// implements it.
type Hooks interface {
	Run(ctx context.Context, req hooks.Request) (hooks.Result, error)
}

// Snapshots is what detection needs from gitwatch. *gitwatch.Watcher implements it.
type Snapshots interface {
	Snapshot() gitwatch.Snapshot
	Changed() <-chan struct{}
}

// Notifier tells people a deployment needs them. *notify.Notifier implements
// it. Notify never returns an error: a failed delivery is only ever a WARN.
type Notifier interface {
	Notify(ctx context.Context, d *store.Deployment)
}

// Observation is the last known drift of one managed job, kept only in
// memory: it is not persisted, since it can always be recomputed from git and
// Nomad. It exists so the dashboard can show drift for a job whose policy is
// "none" (no deployment is ever created for it).
type Observation struct {
	JobID     string
	Namespace string
	FilePath  string
	// Policy is the effective policy read from the job's meta, including "none".
	Policy meta.Policy
	// Drift is true when the live job differs from the one in git.
	Drift bool
	// PlanDiff is the redacted JSON diff, empty when there is no drift.
	PlanDiff string
	// PreHook and PostHook are the hooks the job's meta declares, nil if none:
	// what a deployment of this job would run.
	PreHook, PostHook *meta.Hook
	// Issues lists the meta validation problems found on this job, if any.
	Issues []meta.Issue
	// ObservedAt is when this cycle computed the observation.
	ObservedAt time.Time
	// BlockedBy is the ID of the deployment whose retry rule currently
	// suppresses a new deployment for this job's drift, or "" if none (see
	// docs/design/engine-apply.md, decisions 6 and 7).
	BlockedBy string
	// BlockedReason explains BlockedBy and what unblocks it. Empty when
	// BlockedBy is empty.
	BlockedReason string
}

// Orphan is a job nops has deployed that is no longer in the repository but is
// still there in Nomad and not stopped: something the operator has to decide
// about. nops only reports it, it never stops it (see
// docs/design/engine-detection.md#orphan-jobs). Like Observation it is kept
// only in memory and rebuilt every cycle.
type Orphan struct {
	JobID     string
	Namespace string
	// Policy is the policy of the job's last completed deployment: the file
	// that said what it should be is gone.
	Policy store.Policy
	// LastDeploymentID is that deployment.
	LastDeploymentID string
	// NomadStatus is the job's status in Nomad ("running" or "pending").
	NomadStatus string
	// ObservedAt is when this cycle looked at it in Nomad.
	ObservedAt time.Time
}

// Status is how the last detection cycle went, kept only in memory for the
// dashboard: whether nops is doing its job is not something an empty list of
// deployments can say.
type Status struct {
	// At is when the last cycle ended; zero before the first one.
	At time.Time
	// Duration is how long that cycle took.
	Duration time.Duration
	// Error is the failure that aborted the cycle (a store or redact error),
	// or "" if it ran to the end.
	Error string
	// Managed is how many managed jobs the cycle found. Skipped is how many of
	// them it could not plan because of a Nomad failure scoped to the job
	// (they have no observation this cycle). Unparsed is how many files Nomad
	// could not parse (or that name a namespace nops does not manage).
	Managed, Skipped, Unparsed int
	// Orphans is how many orphan jobs are reported. OrphanCheckSkipped is true
	// when the cycle did not look for them because a file did not parse (a
	// file that does not parse looks exactly like one that was removed); the
	// orphans of the last complete check are then kept as they were.
	Orphans            int
	OrphanCheckSkipped bool
}

// Engine runs the detection cycle (parse, plan, create and supersede
// deployments, sync hook jobs, keep the in-memory drift observations) and the
// apply loop (advance non-terminal deployments to completed).
type Engine struct {
	store          Store
	nomad          Nomad
	snapshots      Snapshots
	notifier       Notifier
	hooks          Hooks
	log            *slog.Logger
	namespace      string
	driftInterval  time.Duration
	engineInterval time.Duration
	applyTimeout   time.Duration
	now            func() time.Time

	mu           sync.RWMutex
	parseCache   map[string]parseEntry
	observations map[string]Observation
	orphans      []Orphan
	status       Status

	// kick asks the detection loop for a cycle now (Retry).
	kick chan struct{}

	applyMu  sync.Mutex
	inFlight map[string]struct{}
}

// Options configures New.
type Options struct {
	Store     Store
	Nomad     Nomad
	Snapshots Snapshots
	Notifier  Notifier
	// Hooks runs a deployment's pre/post hooks (used by apply).
	Hooks Hooks
	// Namespace is the single Nomad namespace nops manages (config.Config.NomadNamespace).
	Namespace string
	// DriftInterval is how often a detection cycle runs when there is no new commit.
	DriftInterval time.Duration
	// EngineInterval is how often the apply loop advances non-terminal deployments.
	EngineInterval time.Duration
	// ApplyTimeout is how long an apply may wait for the Nomad deployment (or
	// the allocations) to become healthy, counted from the "-> applying" event.
	ApplyTimeout time.Duration
	Log          *slog.Logger
}

// New creates an Engine. Call RunDetection and RunApply to start the two
// loops, or Detect directly for a single detection cycle (recovery and tests).
func New(o Options) *Engine {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		store:          o.Store,
		nomad:          o.Nomad,
		snapshots:      o.Snapshots,
		notifier:       o.Notifier,
		hooks:          o.Hooks,
		log:            log,
		namespace:      o.Namespace,
		driftInterval:  o.DriftInterval,
		engineInterval: o.EngineInterval,
		applyTimeout:   o.ApplyTimeout,
		now:            time.Now,
		parseCache:     map[string]parseEntry{},
		observations:   map[string]Observation{},
		inFlight:       map[string]struct{}{},
		kick:           make(chan struct{}, 1),
	}
}

// RunDetection runs one cycle immediately, then again on every new commit
// (Snapshots.Changed), on every DriftInterval tick and when Retry asks for
// one, until ctx is done.
func (e *Engine) RunDetection(ctx context.Context) {
	e.runOnce(ctx)
	ticker := time.NewTicker(e.driftInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.runOnce(ctx)
		case <-e.snapshots.Changed():
			e.runOnce(ctx)
		case <-e.kick:
			e.runOnce(ctx)
		}
	}
}

func (e *Engine) runOnce(ctx context.Context) {
	if err := e.Detect(ctx); err != nil {
		e.log.ErrorContext(ctx, "detection cycle aborted", "error", err)
	}
}

// Observations returns the last cycle's drift, one entry per managed job,
// sorted by job ID. It is rebuilt from scratch on every cycle, so a job that
// leaves the repository disappears from it on the next call.
func (e *Engine) Observations() []Observation {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Observation, 0, len(e.observations))
	for _, o := range e.observations {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out
}

// Status returns how the last detection cycle went.
func (e *Engine) Status() Status {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.status
}

func (e *Engine) setStatus(st Status) {
	e.mu.Lock()
	e.status = st
	e.mu.Unlock()
}

// Orphans returns the jobs nops deployed that are gone from the repository but
// still run in Nomad, sorted by job ID. See Orphan.
func (e *Engine) Orphans() []Orphan {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]Orphan(nil), e.orphans...)
}

func (e *Engine) setOrphans(next []Orphan) {
	e.mu.Lock()
	e.orphans = next
	e.mu.Unlock()
}
