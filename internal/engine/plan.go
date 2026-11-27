package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// Plan is what a detection cycle would do if a repository held other job
// files: the answer to "what would Nops do with this change" before it is
// merged (docs/cli.md#planning-a-change).
type Plan struct {
	// Jobs are the managed jobs of the repository, and those of the other
	// repositories that run a hook it holds or held, sorted by namespace, then
	// ID.
	Jobs []PlannedJob
	// Errors are the files of the repository that make no job, and the jobs
	// the cycle leaves out or could not look at.
	Errors []FileError
	// Unplanned are the managed jobs of the repository in a namespace the
	// caller may not plan: not planned, and named only.
	Unplanned []FileError
	// Removed are the jobs the cycle would read as removed, sorted by
	// namespace, then ID: those the repository manages now and would not any
	// more, and those a file of the repository that does not parse now keeps
	// from being read as removed.
	Removed []RemovedJob
	// RemovedUnchecked says a file of a repository does not parse, which keeps
	// a cycle from reading any job as removed: Removed is then empty.
	RemovedUnchecked bool
}

// PlannedJob is a managed job as the cycle would leave it.
type PlannedJob struct {
	Observation
	// Latest is the state the job's latest deployment would be in: the one the
	// cycle creates or moves, or else the one there is; "" with none.
	Latest store.State
}

// RemovedJob is a job the cycle would read as removed.
type RemovedJob struct {
	Namespace, JobID string
	// Superseded is the deployment of the job, waiting to start or for
	// approval, that the cycle would supersede; "" with none.
	Superseded string
	// Orphan says the job would be an orphan: Nops deployed it, and it runs.
	Orphan bool
}

// ErrUnknownRepository is returned by Plan for a repository Nops does not read.
var ErrUnknownRepository = errors.New("no repository has this name")

// Plan runs a detection cycle in which snap is the snapshot of its
// repository, the others staying at their last one, and returns what it would
// do. It writes nothing, to Nomad or to the store, and keeps nothing in
// memory: the pages go on showing the last real cycle. allow says whether the
// caller may plan a namespace; a job of one it may not is never planned.
//
// It plans in Nomad the jobs of snap, and those of the other repositories
// that run a hook snap holds or the repository held, and looks up the jobs it
// would remove. Like a real cycle, it also parses every file of the other
// repositories the parse cache does not hold (one that did not parse last
// time), and reads the pauses and the Deploy now requests. When a file of the
// repository does not parse now, any job may be one the cycle reads as
// removed, and the plan looks at all of them, as Detect does.
//
// Plans run one at a time; a second one waits for the first, or for ctx.
func (e *Engine) Plan(ctx context.Context, snap gitwatch.Snapshot, allow func(namespace string) bool) (Plan, error) {
	select {
	case e.planSem <- struct{}{}:
		defer func() { <-e.planSem }()
	case <-ctx.Done():
		return Plan{}, ctx.Err()
	}

	snaps := e.snapshots.Snapshots()
	at := slices.IndexFunc(snaps, func(s gitwatch.Snapshot) bool { return s.Repository == snap.Repository })
	if at < 0 {
		return Plan{}, fmt.Errorf("%w: %q", ErrUnknownRepository, snap.Repository)
	}
	dc := e.decider()
	c := newPlanCycle(dc, e.store, snap.Repository, allow)
	c.readBefore(ctx, snaps[at])
	snaps = slices.Clone(snaps)
	snaps[at] = snap

	st, err := dc.run(ctx, c, snaps)
	if err != nil {
		return Plan{}, err
	}

	p := Plan{Unplanned: c.unplanned}
	for _, f := range c.errors {
		// A job with no file is one the plan looked up for removal.
		if f.Repository == snap.Repository || f.Repository == "" || c.planned[jobKey{f.Namespace, f.JobID}] {
			p.Errors = append(p.Errors, f)
		}
	}
	for key, o := range c.obs {
		latest, err := c.latest(ctx, key)
		if err != nil {
			return Plan{}, err
		}
		p.Jobs = append(p.Jobs, PlannedJob{Observation: o, Latest: latest})
	}
	sort.Slice(p.Jobs, func(i, j int) bool {
		return lessKey(jobKey{p.Jobs[i].Namespace, p.Jobs[i].JobID}, jobKey{p.Jobs[j].Namespace, p.Jobs[j].JobID})
	})

	if st.Unparsed > 0 {
		p.RemovedUnchecked = true
		return p, nil
	}
	orphansNow := map[jobKey]bool{}
	for _, o := range e.Orphans() {
		orphansNow[jobKey{o.Namespace, o.JobID}] = true
	}
	removed := map[jobKey]bool{}
	for key := range c.before {
		removed[key] = true
	}
	for key := range c.superseded {
		removed[key] = true
	}
	for key := range c.orphans {
		if !orphansNow[key] {
			removed[key] = true
		}
	}
	for key := range removed {
		if c.seen(key) {
			continue
		}
		p.Removed = append(p.Removed, RemovedJob{Namespace: key.namespace, JobID: key.id, Superseded: c.superseded[key], Orphan: c.orphans[key]})
	}
	sort.Slice(p.Removed, func(i, j int) bool {
		return lessKey(jobKey{p.Removed[i].Namespace, p.Removed[i].JobID}, jobKey{p.Removed[j].Namespace, p.Removed[j].JobID})
	})
	return p, nil
}

func lessKey(a, b jobKey) bool {
	if a.namespace != b.namespace {
		return a.namespace < b.namespace
	}
	return a.id < b.id
}

// planCycle is the cycle of a plan: it records what Detect would do, and
// does none of it. Like the decider, it reads through views that cannot write.
type planCycle struct {
	dc    *decider
	store planStore
	// repo is the repository planned.
	repo  string
	allow func(namespace string) bool

	// before is the managed jobs of the repository now, and hooksBefore its
	// hooks: what the plan may find removed, and the hooks whose jobs it plans.
	before      map[jobKey]bool
	hooksBefore map[jobKey]bool
	// all says a file of the repository does not parse now: any job the cycle
	// does not see may be one it reads as removed.
	all bool

	obs        map[jobKey]Observation
	created    map[jobKey]store.State // the state of the deployment the cycle creates
	moved      map[jobKey]store.State // the state it moves the job's active deployment to
	superseded map[jobKey]string      // the deployment it supersedes
	orphans    map[jobKey]bool
	errors     []FileError
	unplanned  []FileError
	managed    map[jobKey]bool // every managed job of the cycle, planned or not
	planned    map[jobKey]bool // the jobs it plans
}

// planStore is what a plan reads from the store besides what the decider does:
// the deployments of the jobs it may find removed.
type planStore interface {
	decideStore
	ListActive(ctx context.Context) ([]*store.Deployment, error)
	OrphanCandidates(ctx context.Context, namespace string) ([]*store.Deployment, error)
	OrphanCandidate(ctx context.Context, namespace, jobID string) (*store.Deployment, error)
}

func newPlanCycle(dc *decider, st planStore, repo string, allow func(string) bool) *planCycle {
	return &planCycle{
		dc: dc, store: st, repo: repo, allow: allow, before: map[jobKey]bool{}, hooksBefore: map[jobKey]bool{},
		created: map[jobKey]store.State{}, moved: map[jobKey]store.State{}, superseded: map[jobKey]string{},
		orphans: map[jobKey]bool{}, managed: map[jobKey]bool{}, planned: map[jobKey]bool{},
	}
}

// readBefore reads the managed jobs and the hooks of the repository's
// snapshot now, through the parse cache. It reports nothing: the last cycle
// did.
func (c *planCycle) readBefore(ctx context.Context, snap gitwatch.Snapshot) {
	parsed, _, unparsed, _ := c.dc.parseFiles(ctx, newPlanCycle(c.dc, c.store, c.repo, c.allow), []gitwatch.Snapshot{snap})
	c.all = unparsed > 0
	count := map[jobKey]int{}
	for _, pf := range parsed {
		count[keyOf(pf.job)]++
	}
	for _, pf := range parsed {
		key := keyOf(pf.job)
		switch {
		case pf.cfg.IsHook:
			c.hooksBefore[key] = true
		case count[key] == 1 && pf.cfg.Managed:
			c.before[key] = true
		}
	}
}

// discard is the logger of a plan: what it finds goes in its answer, and a
// change that is not merged is nothing to log about.
var discard = slog.New(slog.DiscardHandler)

func (c *planCycle) log() *slog.Logger { return discard }

// reconciles plans the jobs of the repository, and those of the others that
// run a hook it holds or held: a change to a hook changes them. A job of a
// namespace the caller may not plan is never planned; one of the repository
// is listed as unplanned.
func (c *planCycle) reconciles(mf parsedFile, hooks map[string]parsedFile) bool {
	key := keyOf(mf.job)
	c.managed[key] = true
	ours := mf.commit.repo == c.repo
	if !ours && !c.runsOurHook(key.namespace, mf, hooks) {
		return false
	}
	if !c.allow(key.namespace) {
		if ours {
			c.unplanned = append(c.unplanned, FileError{Repository: mf.commit.repo, Path: mf.path, Namespace: key.namespace, JobID: key.id})
		}
		return false
	}
	c.planned[key] = true
	return true
}

// runsOurHook says whether the job runs a hook of the repository, as it is
// in the plan or as it is now.
func (c *planCycle) runsOurHook(ns string, mf parsedFile, hooks map[string]parsedFile) bool {
	for _, id := range slices.Concat(mf.cfg.PreHooks, mf.cfg.PostHooks) {
		if h, ok := hooks[id]; ok && h.commit.repo == c.repo || c.hooksBefore[jobKey{ns, id}] {
			return true
		}
	}
	return false
}

func (c *planCycle) seen(key jobKey) bool { return c.managed[key] }

func (c *planCycle) transition(_ context.Context, _ *slog.Logger, d *store.Deployment, to store.State, _ string) error {
	key := jobKey{d.Namespace, d.JobID}
	c.moved[key] = to
	if to == store.StateSuperseded {
		c.superseded[key] = d.ID
	}
	return nil
}

func (c *planCycle) moveCASIndex(_ context.Context, _ *slog.Logger, d *store.Deployment, to uint64) error {
	d.CASIndex = to
	return nil
}

func (c *planCycle) create(_ context.Context, _ *slog.Logger, d *store.Deployment, _ string) error {
	c.created[jobKey{d.Namespace, d.JobID}] = d.State
	return nil
}

func (c *planCycle) dropWindowLift(context.Context, store.WindowLift, string) error { return nil }
func (c *planCycle) tidy(context.Context) error                                     { return nil }

// gone is the jobs of the repository now that the cycle no longer sees.
func (c *planCycle) gone() []jobKey {
	var out []jobKey
	for key := range c.before {
		if !c.seen(key) {
			out = append(out, key)
		}
	}
	return out
}

// waiting is the active deployments of the jobs the cycle may read as
// removed: those of the repository it no longer sees, or every one when a
// file of the repository does not parse now.
func (c *planCycle) waiting(ctx context.Context) ([]*store.Deployment, error) {
	if c.all {
		return c.store.ListActive(ctx)
	}
	var out []*store.Deployment
	for _, key := range c.gone() {
		d, err := c.store.ActiveDeployment(ctx, key.namespace, key.id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, fmt.Errorf("active deployment of %s: %w", key.id, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// orphanCandidates is the candidates of namespace ns among the same jobs. A
// namespace the caller may not plan has none: nothing of it is looked up in
// Nomad.
func (c *planCycle) orphanCandidates(ctx context.Context, ns string) ([]*store.Deployment, error) {
	if !c.allow(ns) {
		return nil, nil
	}
	if c.all {
		return c.store.OrphanCandidates(ctx, ns)
	}
	var out []*store.Deployment
	for _, key := range c.gone() {
		if key.namespace != ns {
			continue
		}
		d, err := c.store.OrphanCandidate(ctx, ns, key.id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, fmt.Errorf("orphan candidate %s: %w", key.id, err)
		}
		out = append(out, d)
	}
	return out, nil
}

func (c *planCycle) parsed(map[string]parseEntry)        {}
func (c *planCycle) pausedAll(*Hold)                     {}
func (c *planCycle) observed(obs map[jobKey]Observation) { c.obs = obs }

func (c *planCycle) orphansFound(orphans []Orphan) {
	for _, o := range orphans {
		c.orphans[jobKey{o.Namespace, o.JobID}] = true
	}
}

func (c *planCycle) failed(f FileError) { c.errors = append(c.errors, f) }

// latest is the state the job's latest deployment would be in after the cycle.
func (c *planCycle) latest(ctx context.Context, key jobKey) (store.State, error) {
	if s, ok := c.created[key]; ok {
		return s, nil
	}
	if s, ok := c.moved[key]; ok {
		return s, nil
	}
	d, err := c.store.LatestDeployment(ctx, key.namespace, key.id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("latest deployment of %s: %w", key.id, err)
	}
	return d.State, nil
}
