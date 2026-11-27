package engine

import (
	"context"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/store"
)

// decider makes the decisions of a detection cycle, for Detect and for Plan
// alike. It reads the store and Nomad through views that cannot write: what a
// cycle changes, it changes through its cycle. So a plan, which runs the same
// decisions, cannot write, and a write added here does not compile.
type decider struct {
	store      decideStore
	nomad      decideNomad
	namespaces []string            // the managed namespaces, in the order configured
	managedNS  map[string]bool     // the same, for lookups
	repoJobs   map[string][]string // Options.RepositoryJobs
	syncLoc    *time.Location
	now        func() time.Time
	// parseCache is the cache the last cycle built, and orphans the orphans it
	// reported.
	parseCache func() map[string]parseEntry
	orphans    func() []Orphan
}

// decideStore is what the decisions read from the store.
type decideStore interface {
	ActivePauses(ctx context.Context) ([]store.Pause, error)
	AllPause(ctx context.Context) (store.Pause, error)
	WindowLifts(ctx context.Context) ([]store.WindowLift, error)
	ActiveDeployment(ctx context.Context, namespace, jobID string) (*store.Deployment, error)
	LatestDeployment(ctx context.Context, namespace, jobID string) (*store.Deployment, error)
}

// decideNomad is what the decisions read from Nomad: a plan is a dry run.
type decideNomad interface {
	ParseHCL(ctx context.Context, ns, hcl, vars string) (*api.Job, error)
	Job(ctx context.Context, ns, id string) (*api.Job, error)
	Plan(ctx context.Context, job *api.Job) (*api.JobPlanResponse, error)
}

// decider is the engine's decider, as the engine is set up now.
func (e *Engine) decider() *decider {
	return &decider{
		store: e.store, nomad: e.nomad, namespaces: e.namespaces, managedNS: e.managedNS, repoJobs: e.repoJobs,
		syncLoc: e.syncLoc, now: e.now, parseCache: e.snapshotParseCache, orphans: e.Orphans,
	}
}
