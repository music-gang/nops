package gitwatch

import (
	"context"
	"sync"
)

// Repo is one repository of a Group as the dashboard and the metrics show it.
type Repo struct {
	Name string
	// URL is the repository's URL without credentials.
	URL      string
	Snapshot Snapshot
	Status   Status
}

// Group is the repositories Nops reads, each with its own Watcher: each one is
// cloned and polled on its own, and a failed poll of one leaves the others'
// snapshots alone. Build it with NewGroup.
type Group struct {
	watchers []*Watcher
	changed  chan struct{}
}

// NewGroup builds a Group of watchers, in the order the snapshots and the
// repositories are listed. The watchers share one Changed channel: a new
// commit in any of them signals it. Call Start before Run.
func NewGroup(ws ...*Watcher) *Group {
	g := &Group{watchers: ws, changed: make(chan struct{}, 1)}
	for _, w := range ws {
		w.changed = g.changed
	}
	return g
}

// Start does the initial clone of every repository. An error is fatal, as for
// a single Watcher.
func (g *Group) Start(ctx context.Context) error {
	for _, w := range g.watchers {
		if err := w.Start(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Run polls every repository on its own until ctx is done, and returns once
// every poll loop has.
func (g *Group) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, w := range g.watchers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Run(ctx)
		}()
	}
	wg.Wait()
}

// Trigger asks every repository for a poll: one whose head did not move costs
// only a git ls-remote.
func (g *Group) Trigger() {
	for _, w := range g.watchers {
		w.Trigger()
	}
}

// Snapshots returns the last good snapshot of every repository.
func (g *Group) Snapshots() []Snapshot {
	out := make([]Snapshot, len(g.watchers))
	for i, w := range g.watchers {
		out[i] = w.Snapshot()
	}
	return out
}

// Changed is signalled after a poll of any repository picks up a new commit.
func (g *Group) Changed() <-chan struct{} {
	return g.changed
}

// Repositories returns every repository with its last good snapshot and how
// its last poll went.
func (g *Group) Repositories() []Repo {
	out := make([]Repo, len(g.watchers))
	for i, w := range g.watchers {
		out[i] = Repo{Name: w.opts.Name, URL: StripCredentials(w.opts.URL), Snapshot: w.Snapshot(), Status: w.Status()}
	}
	return out
}

// CommitURL returns the web address of commit sha of the repository named
// repo, as the package's CommitURL does: "" for a repository it does not have.
func (g *Group) CommitURL(repo, sha string) string {
	for _, w := range g.watchers {
		if w.opts.Name == repo {
			return CommitURL(w.opts.URL, sha)
		}
	}
	return ""
}
