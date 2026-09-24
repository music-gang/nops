// Package engine detects drift between the git repository and the Nomad
// cluster, creates deployments accordingly (see
// docs/design/engine-detection.md), and drives them from approval to
// completed (see docs/design/engine-apply.md). Recovery after a restart is a
// later task: every step here is already written to resume from persisted
// state alone.
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
	}
}

// RunDetection runs one cycle immediately, then again on every new commit
// (Snapshots.Changed) and on every DriftInterval tick, until ctx is done.
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
