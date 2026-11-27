package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/music-gang/nops/internal/store"
)

// cycle is how a detection cycle acts on what it decides. Detect's (liveCycle)
// writes the store, notifies, logs and keeps what it saw for the pages. Plan's
// (planCycle) changes nothing and records what Detect would have done. The
// decisions are one code for both, so a plan is what the next cycle would do.
type cycle interface {
	// log is where the cycle logs what it finds in the files and in Nomad.
	log() *slog.Logger
	// reconciles says whether the cycle plans the managed job of mf and
	// reconciles its deployment; hooks are the hooks of its namespace. One it
	// does not is still seen as managed.
	reconciles(mf parsedFile, hooks map[string]parsedFile) bool

	// The writes, each as Engine's of the same name.
	transition(ctx context.Context, log *slog.Logger, d *store.Deployment, to store.State, message string) error
	moveCASIndex(ctx context.Context, log *slog.Logger, d *store.Deployment, to uint64) error
	// create stores d, already in the state it is born in, which message
	// explains. A deployment that appeared concurrently is not an error.
	create(ctx context.Context, log *slog.Logger, d *store.Deployment, message string) error
	dropWindowLift(ctx context.Context, l store.WindowLift, reason string) error
	// tidy is what the cycle does once its deployments are settled: deregister
	// the hook revisions nothing needs and drop the specs past their retention.
	tidy(ctx context.Context) error

	// The deployments a cycle looks at for the jobs it did not find: the
	// active ones it may supersede, and the completed ones of namespace ns
	// that may make an orphan.
	waiting(ctx context.Context) ([]*store.Deployment, error)
	orphanCandidates(ctx context.Context, ns string) ([]*store.Deployment, error)

	// What the cycle found, as it finds it.
	parsed(cache map[string]parseEntry)
	pausedAll(all *Hold)
	observed(obs map[jobKey]Observation)
	orphansFound(orphans []Orphan)
	// failed is a file that makes no job, or a job the cycle leaves out or
	// could not look at (two files for one job, a hook with invalid meta, a
	// failure of Nops or of Nomad scoped to it), and why. The cycle has logged
	// it already.
	failed(f FileError)
}

// FileError is a file that makes no job, or a job a cycle leaves out or could
// not look at, and why.
type FileError struct {
	// Repository and Path are the file; empty for a job no file declares any
	// more.
	Repository, Path string
	// Namespace and JobID are the job, empty when the file makes none.
	// Namespace is empty too when Nops does not manage it: Error names it.
	Namespace, JobID string
	Error            string
}

// liveCycle is the detection cycle of Detect.
type liveCycle struct{ e *Engine }

func (c liveCycle) log() *slog.Logger { return c.e.log }

func (liveCycle) reconciles(parsedFile, map[string]parsedFile) bool { return true }

func (c liveCycle) transition(ctx context.Context, log *slog.Logger, d *store.Deployment, to store.State, message string) error {
	return c.e.transition(ctx, log, d, to, message)
}

func (c liveCycle) moveCASIndex(ctx context.Context, log *slog.Logger, d *store.Deployment, to uint64) error {
	return c.e.moveCASIndex(ctx, log, d, to)
}

func (c liveCycle) create(ctx context.Context, log *slog.Logger, d *store.Deployment, message string) error {
	born := d.State
	if err := c.e.store.CreateDeployment(ctx, d); err != nil {
		if errors.Is(err, store.ErrActiveDeployment) {
			log.WarnContext(ctx, "active deployment appeared concurrently: skipped this cycle", "error", err)
			return nil
		}
		return fmt.Errorf("create deployment for %s: %w", d.JobID, err)
	}
	if born != store.StateDetected {
		c.e.announce(ctx, log, d, store.StateDetected, born, message)
	}
	return nil
}

func (c liveCycle) dropWindowLift(ctx context.Context, l store.WindowLift, reason string) error {
	return c.e.dropWindowLift(ctx, l, reason)
}

func (c liveCycle) tidy(ctx context.Context) error {
	if err := c.e.gcHookRevisions(ctx); err != nil {
		return err
	}
	return c.e.dropEndedSpecs(ctx)
}

func (c liveCycle) waiting(ctx context.Context) ([]*store.Deployment, error) {
	return c.e.store.ListActive(ctx)
}

func (c liveCycle) orphanCandidates(ctx context.Context, ns string) ([]*store.Deployment, error) {
	return c.e.store.OrphanCandidates(ctx, ns)
}

func (c liveCycle) parsed(cache map[string]parseEntry)  { c.e.replaceParseCache(cache) }
func (c liveCycle) pausedAll(all *Hold)                 { c.e.setAll(all) }
func (c liveCycle) observed(obs map[jobKey]Observation) { c.e.replaceObservations(obs) }
func (c liveCycle) orphansFound(orphans []Orphan)       { c.e.setOrphans(orphans) }
func (liveCycle) failed(FileError)                      {}
