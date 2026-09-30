package web

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/nomadx"
)

// Nomad is what the dashboard reads from Nomad for its panel: the live job, its
// allocations and its latest Nomad deployment. *nomadx.Client implements it.
// The panel is read-only and shows what Nomad reports; the one write the
// dashboard makes there, promoting canaries, goes through Engine.Promote.
type Nomad interface {
	Job(ctx context.Context, ns, id string) (*api.Job, error)
	Allocations(ctx context.Context, ns, jobID string) ([]nomadx.Alloc, error)
	LatestDeployment(ctx context.Context, ns, jobID string) (*api.Deployment, error)
}

var _ Nomad = (*nomadx.Client)(nil)

const (
	// panelTTL is how long the panel of a job is reused. The pages poll every
	// 3 to 5 seconds, so a tab costs one set of Nomad calls per poll and any
	// number of tabs on the same job cost that same one.
	panelTTL = 4 * time.Second
	// panelTimeout bounds the Nomad calls of one panel: a Nomad that does not
	// answer must not hold a page (or the cache) for long.
	panelTimeout = 3 * time.Second
)

// nomadView is what Nomad says about a job, as the Nomad panel shows it. Every
// number is Nomad's own or a plain count of its allocations by the status Nomad
// gave them: nothing is decided here.
type nomadView struct {
	// Err is set when Nomad did not answer; the rest is then empty.
	Err string
	// Missing is set when the job does not exist in Nomad (not registered yet,
	// or purged).
	Missing bool

	Status, Type string
	Version      uint64
	Groups       []groupView
	// Deployment is the job's latest Nomad deployment, nil if it has none (a
	// batch job, or no update block).
	Deployment *nomadDeploymentView

	// AppliedIndex is the index of the apply of the nops deployment the panel is
	// shown for; 0 on a job's page.
	AppliedIndex uint64

	// DeploymentsURL is the job's Deployments tab in the Nomad UI, "" without a
	// -nomad-ui-url.
	DeploymentsURL string
}

// Tracked reports whether the Nomad deployment is the one that tracks the apply
// of the nops deployment the panel is shown for.
func (v *nomadView) Tracked() bool { return v.Deployment.Tracks(v.AppliedIndex) }

// Another reports whether the panel is shown for a nops deployment, and the
// job's latest Nomad deployment is not the one tracking its apply.
func (v *nomadView) Another() bool {
	return v.AppliedIndex != 0 && v.Deployment != nil && !v.Tracked()
}

// groupView is one task group: the count the live job asks for against the
// allocations of the live version.
type groupView struct {
	Name                                     string
	Desired                                  int
	Running, Pending, Complete, Failed, Lost int
	Healthy                                  int // allocations the Nomad deployment marked healthy
	Canaries                                 int // canary allocations
}

// nomadDeploymentView is a Nomad deployment.
type nomadDeploymentView struct {
	ID, Status, Description string
	JobModifyIndex          uint64
	Groups                  []deploymentGroupView
}

// deploymentGroupView is what a Nomad deployment says of one task group.
type deploymentGroupView struct {
	Name                       string
	Healthy, Unhealthy, Placed int
	Desired                    int
	DesiredCanaries            int
	PlacedCanaries             int
	Promoted                   bool
	// ProgressBy is the deadline of the group's progress: zero when Nomad has
	// none (the deployment waits for a promotion, or is finished). Progress is
	// its rendering, filled for the request that shows it.
	ProgressBy time.Time
	Progress   timeView
}

// ShortID is the first 8 characters of the Nomad deployment's ID, as Nomad's own
// CLI shows it.
func (d *nomadDeploymentView) ShortID() string {
	if len(d.ID) > 8 {
		return d.ID[:8]
	}
	return d.ID
}

// Tracks reports whether the Nomad deployment is the one that tracks the apply
// registered at appliedIndex, the one a nops deployment waits on.
func (d *nomadDeploymentView) Tracks(appliedIndex uint64) bool {
	return d != nil && appliedIndex != 0 && d.JobModifyIndex == appliedIndex
}

// panelCache keeps the last panel of each job for panelTTL, errors included: a
// Nomad that is down is asked once per TTL, not once per poll per tab.
type panelCache struct {
	mu      sync.Mutex
	entries map[string]panelEntry
}

type panelEntry struct {
	at   time.Time
	view nomadView
}

// nomadPanel returns what Nomad says about a job, or nil when the dashboard has
// no Nomad to ask. It never fails: what Nomad answers with an error is the
// view's Err, so a page renders with the panel saying it could not be read.
//
// appliedIndex is the job index a nops deployment applied, for the panel of that
// deployment (Tracked says whether the Nomad deployment is the one that tracks it),
// and 0 for the panel of a job.
func (s *server) nomadPanel(ctx context.Context, namespace, jobID string, appliedIndex uint64) *nomadView {
	if s.nomad == nil {
		return nil
	}
	key := jobKey(namespace, jobID)
	s.panels.mu.Lock()
	defer s.panels.mu.Unlock()
	e, ok := s.panels.entries[key]
	if !ok || s.now().Sub(e.at) >= panelTTL || s.now().Before(e.at) {
		ctx, cancel := context.WithTimeout(ctx, panelTimeout)
		defer cancel()
		e = panelEntry{at: s.now(), view: s.readNomad(ctx, namespace, jobID)}
		if s.panels.entries == nil {
			s.panels.entries = map[string]panelEntry{}
		}
		s.panels.entries[key] = e
	}
	v := e.view
	v.AppliedIndex = appliedIndex
	v.DeploymentsURL = s.nomadDeploymentsURL(namespace, jobID)
	// The progress deadline is shown relative to now: render it per request.
	if v.Deployment != nil {
		d := *v.Deployment
		d.Groups = append([]deploymentGroupView(nil), d.Groups...)
		for i := range d.Groups {
			d.Groups[i].Progress = s.until(d.Groups[i].ProgressBy)
		}
		v.Deployment = &d
	}
	return &v
}

// readNomad asks Nomad. A failure is logged at ERROR (docs/logs-and-notifications.md),
// once per cache entry, and returned as the view's Err.
func (s *server) readNomad(ctx context.Context, namespace, jobID string) nomadView {
	fail := func(what string, err error) nomadView {
		s.log.ErrorContext(ctx, "read Nomad for the panel", "namespace", namespace, "job", jobID, "what", what, "error", err)
		return nomadView{Err: fmt.Sprintf("Nomad did not answer (%s): %v", what, err)}
	}
	job, err := s.nomad.Job(ctx, namespace, jobID)
	if errors.Is(err, nomadx.ErrJobNotFound) {
		return nomadView{Missing: true}
	}
	if err != nil {
		return fail("job", err)
	}
	allocs, err := s.nomad.Allocations(ctx, namespace, jobID)
	if err != nil {
		return fail("allocations", err)
	}
	dep, err := s.nomad.LatestDeployment(ctx, namespace, jobID)
	if err != nil {
		return fail("deployment", err)
	}

	v := nomadView{Version: derefU64(job.Version)}
	if job.Status != nil {
		v.Status = *job.Status
	}
	if job.Type != nil {
		v.Type = *job.Type
	}
	var groups []*groupView // in the job's order
	byGroup := map[string]*groupView{}
	for _, g := range job.TaskGroups {
		if g == nil || g.Name == nil {
			continue
		}
		gv := &groupView{Name: *g.Name}
		if g.Count != nil {
			gv.Desired = *g.Count
		}
		groups = append(groups, gv)
		byGroup[gv.Name] = gv
	}
	for _, a := range allocs {
		// The allocations of the live version: the older ones are being replaced.
		if a.JobVersion != v.Version {
			continue
		}
		gv, ok := byGroup[a.TaskGroup]
		if !ok {
			continue
		}
		switch a.ClientStatus {
		case "running":
			gv.Running++
		case "pending":
			gv.Pending++
		case "complete":
			gv.Complete++
		case "failed":
			gv.Failed++
		case "lost":
			gv.Lost++
		}
		if a.Healthy != nil && *a.Healthy {
			gv.Healthy++
		}
		if a.Canary {
			gv.Canaries++
		}
	}
	for _, gv := range groups {
		v.Groups = append(v.Groups, *gv)
	}

	if dep != nil {
		dv := &nomadDeploymentView{ID: dep.ID, Status: dep.Status, Description: dep.StatusDescription, JobModifyIndex: dep.JobModifyIndex}
		names := make([]string, 0, len(dep.TaskGroups))
		for name := range dep.TaskGroups {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			st := dep.TaskGroups[name]
			if st == nil {
				continue
			}
			dv.Groups = append(dv.Groups, deploymentGroupView{
				Name: name, Healthy: st.HealthyAllocs, Unhealthy: st.UnhealthyAllocs, Placed: st.PlacedAllocs,
				Desired: st.DesiredTotal, DesiredCanaries: st.DesiredCanaries, PlacedCanaries: len(st.PlacedCanaries),
				Promoted: st.Promoted, ProgressBy: st.RequireProgressBy,
			})
		}
		v.Deployment = dv
	}
	return v
}

func derefU64(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}
