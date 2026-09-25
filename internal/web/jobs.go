package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// jobHistoryLimit is how many deployments a job's page lists.
const jobHistoryLimit = 50

// -- sync state of a job ---------------------------------------------------

// syncKey is where a managed job stands against git, in one word. It is what
// the Jobs page filters on and colors by; the order below is the precedence
// (the first that applies wins) and the order of the filter.
type syncKey string

const (
	syncInvalid   syncKey = "invalid"   // its meta has an error: nops ignores its policy and hooks
	syncBlocked   syncKey = "blocked"   // a failed or rejected deployment holds its drift back
	syncOrphan    syncKey = "orphan"    // deployed by nops, gone from git, still running in Nomad
	syncPending   syncKey = "pending"   // a deployment waits for a human decision
	syncDeploying syncKey = "deploying" // a deployment is on its way (detected, hooks, apply)
	syncDrift     syncKey = "drift"     // the cluster differs from git and nothing is being done
	syncInSync    syncKey = "sync"      // the cluster is what git says
)

var syncOrder = []syncKey{syncInvalid, syncBlocked, syncOrphan, syncPending, syncDeploying, syncDrift, syncInSync}

// valid reports whether k is one of the sync states.
func (k syncKey) valid() bool {
	for _, o := range syncOrder {
		if o == k {
			return true
		}
	}
	return false
}

func (k syncKey) label() string {
	switch k {
	case syncInvalid:
		return "Invalid meta"
	case syncBlocked:
		return "Blocked"
	case syncOrphan:
		return "Not in git"
	case syncPending:
		return "Awaiting approval"
	case syncDeploying:
		return "Deploying"
	case syncDrift:
		return "Drift"
	default:
		return "In sync"
	}
}

// class is the palette color of the state (the same five as a deployment's).
func (k syncKey) class() string {
	switch k {
	case syncInvalid, syncBlocked:
		return "state-failed"
	case syncPending, syncDrift, syncOrphan:
		return "state-pending"
	case syncDeploying:
		return "state-running"
	default:
		return "state-success"
	}
}

// jobSync decides the sync state of one observed job from what the last cycle
// saw and the job's latest deployment (nil if it never had one).
func jobSync(o engine.Observation, latest *store.Deployment) syncKey {
	for _, iss := range o.Issues {
		if iss.Severity == meta.SeverityError {
			return syncInvalid
		}
	}
	if o.BlockedBy != "" {
		return syncBlocked
	}
	if latest != nil {
		switch latest.State {
		case store.StatePendingApproval:
			return syncPending
		case store.StateDetected, store.StatePreHook, store.StateApplying, store.StatePostHook:
			return syncDeploying
		}
	}
	if o.Drift {
		return syncDrift
	}
	return syncInSync
}

// jobKey is how the maps of this file name a job.
func jobKey(namespace, jobID string) string { return namespace + "/" + jobID }

// latestByJob indexes the latest deployment of every job.
func (s *server) latestByJob(r *http.Request) (map[string]*store.Deployment, error) {
	list, err := s.store.LatestPerJob(r.Context())
	if err != nil {
		return nil, err
	}
	m := make(map[string]*store.Deployment, len(list))
	for _, d := range list {
		m[jobKey(d.Namespace, d.JobID)] = d
	}
	return m, nil
}

// -- GET /jobs -------------------------------------------------------------

// jobRow is one line of the Jobs page.
type jobRow struct {
	Namespace, JobID, Title, Path string
	Policy                        meta.Policy
	Sync                          syncKey
	SyncLabel, SyncClass          string
	BlockedReason                 string
	File                          string
	Observed                      timeView
	Last                          *deploymentCard // nil: the job never had a deployment
	Warnings                      int             // meta issues that are not errors
}

func (s *server) jobRow(o engine.Observation, latest *store.Deployment) jobRow {
	k := jobSync(o, latest)
	row := jobRow{
		Namespace: o.Namespace, JobID: o.JobID, Title: o.Namespace + "/" + o.JobID, Path: jobPath(o.Namespace, o.JobID),
		Policy: o.Policy, Sync: k, SyncLabel: k.label(), SyncClass: k.class(),
		BlockedReason: o.BlockedReason, File: o.FilePath, Observed: s.when(o.ObservedAt),
	}
	if latest != nil {
		c := s.card(latest)
		row.Last = &c
	}
	for _, iss := range o.Issues {
		if iss.Severity != meta.SeverityError {
			row.Warnings++
		}
	}
	return row
}

// orphanRow is the Jobs line of an orphan. It has no file and no observation:
// what it has is what nops deployed, and what Nomad says about it.
func (s *server) orphanRow(o engine.Orphan, latest *store.Deployment) jobRow {
	row := jobRow{
		Namespace: o.Namespace, JobID: o.JobID, Title: o.Namespace + "/" + o.JobID, Path: jobPath(o.Namespace, o.JobID),
		Policy: meta.Policy(o.Policy), Sync: syncOrphan, SyncLabel: syncOrphan.label(), SyncClass: syncOrphan.class(),
		BlockedReason: orphanReason, Observed: s.when(o.ObservedAt),
	}
	if latest != nil {
		c := s.card(latest)
		row.Last = &c
	}
	return row
}

// orphanReason is what an orphan is, in a line: shown under its name on Jobs
// and as the detail of its row on the Overview.
const orphanReason = "removed from git, still running in Nomad"

// filterLink is one choice of a page's filter: a link with the number of
// rows it would show.
type filterLink struct {
	Key, Label string
	Count      int
	Active     bool
}

type jobsData struct {
	baseData
	Rows    []jobRow
	Filters []filterLink
	Filter  string // "" for all
	Total   int
}

func (s *server) jobs(w http.ResponseWriter, r *http.Request) {
	obs := s.engine.Observations()
	latest, err := s.latestByJob(r)
	if err != nil {
		s.serverError(w, r, "list latest deployments", err)
		return
	}
	// A state that is not one of ours (an old link) shows everything, as if
	// there were no filter.
	filter := syncKey(r.URL.Query().Get("state"))
	if !filter.valid() {
		filter = ""
	}

	orphans := s.engine.Orphans()
	data := jobsData{baseData: s.base(r, "jobs"), Total: len(obs) + len(orphans), Filter: string(filter)}
	counts := map[syncKey]int{}
	rows := make([]jobRow, 0, data.Total)
	for _, o := range obs {
		rows = append(rows, s.jobRow(o, latest[jobKey(o.Namespace, o.JobID)]))
	}
	for _, o := range orphans {
		rows = append(rows, s.orphanRow(o, latest[jobKey(o.Namespace, o.JobID)]))
	}
	// Observations are sorted by job ID and so are orphans: one list again.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Title < rows[j].Title })
	for _, row := range rows {
		counts[row.Sync]++
		if filter == "" || filter == row.Sync {
			data.Rows = append(data.Rows, row)
		}
	}
	data.Filters = append(data.Filters, filterLink{Label: "All", Count: data.Total, Active: filter == ""})
	for _, k := range syncOrder {
		if counts[k] > 0 || k == filter {
			data.Filters = append(data.Filters, filterLink{Key: string(k), Label: k.label(), Count: counts[k], Active: k == filter})
		}
	}
	s.render(w, r, "jobs", data)
}

// -- GET /jobs/{namespace}/{job} --------------------------------------------

// hookView is a hook a job declares, as the pages name it.
type hookView struct {
	JobID   string
	Timeout string
}

func hookOf(h *meta.Hook) *hookView {
	if h == nil {
		return nil
	}
	return &hookView{JobID: h.JobID, Timeout: duration(h.Timeout)}
}

// orphanView is what the page of an orphan tells the operator to do. nops does
// not do it for them.
type orphanView struct {
	NomadStatus string
	StopCommand string
}

type jobData struct {
	baseData
	Namespace, JobID, Title string
	InRepo                  bool // the last cycle observed it; false: only its deployments remain
	Policy                  meta.Policy
	Sync                    syncKey
	SyncLabel, SyncClass    string
	File                    string
	Observed                timeView
	PreHook, PostHook       *hookView
	Orphan                  *orphanView // set when the job is gone from git but still runs in Nomad
	Blocked                 bool
	BlockedReason           string
	BlockedBy               string
	RetryPath               string // where the "Retry" button posts, set when Blocked
	Drift                   bool
	Diff                    *api.JobDiff
	Summary                 diffSummary
	Issues                  []meta.Issue
	Deployments             []deploymentCard
}

func (s *server) job(w http.ResponseWriter, r *http.Request) {
	ns, id := r.PathValue("namespace"), r.PathValue("job")

	var obs *engine.Observation
	for _, o := range s.engine.Observations() {
		if o.Namespace == ns && o.JobID == id {
			o := o
			obs = &o
			break
		}
	}
	var orphan *engine.Orphan
	for _, o := range s.engine.Orphans() {
		if o.Namespace == ns && o.JobID == id {
			o := o
			orphan = &o
			break
		}
	}
	deps, err := s.store.ListByJob(r.Context(), ns, id, jobHistoryLimit)
	if err != nil {
		s.serverError(w, r, "list deployments of a job", err)
		return
	}
	if obs == nil && orphan == nil && len(deps) == 0 {
		s.notFoundMessage(w, r, "This job does not exist.")
		return
	}

	data := jobData{baseData: s.base(r, "jobs"), Namespace: ns, JobID: id, Title: ns + "/" + id}
	for _, d := range deps {
		data.Deployments = append(data.Deployments, s.card(d))
	}
	if orphan != nil {
		data.Orphan = &orphanView{NomadStatus: orphan.NomadStatus, StopCommand: "nomad job stop -namespace " + orphan.Namespace + " " + orphan.JobID}
		data.Sync, data.SyncLabel, data.SyncClass = syncOrphan, syncOrphan.label(), syncOrphan.class()
		data.Policy = meta.Policy(orphan.Policy)
	}
	if obs != nil {
		var latest *store.Deployment
		if len(deps) > 0 {
			latest = deps[0]
		}
		diff, err := parseDiff(obs.PlanDiff)
		if err != nil {
			s.serverError(w, r, "parse drift diff", err)
			return
		}
		k := jobSync(*obs, latest)
		data.InRepo, data.Policy = true, obs.Policy
		data.Sync, data.SyncLabel, data.SyncClass = k, k.label(), k.class()
		data.File, data.Observed = obs.FilePath, s.when(obs.ObservedAt)
		data.PreHook, data.PostHook = hookOf(obs.PreHook), hookOf(obs.PostHook)
		data.Blocked, data.BlockedBy, data.BlockedReason = obs.BlockedBy != "", obs.BlockedBy, obs.BlockedReason
		if data.Blocked {
			data.RetryPath = jobPath(ns, id) + "/retry"
		}
		data.Drift, data.Diff, data.Summary, data.Issues = obs.Drift, diff, summarize(diff), obs.Issues
	}
	s.render(w, r, "job", data)
}

// -- what needs attention (the Overview) ----------------------------------

// recentFailure is how long a failed deployment nobody dealt with stays in the
// list of what needs attention.
const recentFailure = 7 * 24 * time.Hour

// attentionItem is one line of "Needs attention": something a person has to
// look at, with why and where to act.
type attentionItem struct {
	Kind      string // "pending", "blocked", "failed" or "invalid"
	KindLabel string
	KindClass string // palette color, like a state's
	Title     string // namespace/job
	Path      string // where to act: a deployment or a job
	Detail    string
	When      timeView
	// RetryPath, when set, is where the "Retry" button of a blocked job posts.
	RetryPath string
}

// attention collects what needs a person, most urgent first: approvals (oldest
// waiting first), blocked jobs, failures nobody has retried, meta errors. A
// job under policy "none" that drifts is not here: leaving it is its policy.
func (s *server) attention(obs []engine.Observation, active []*store.Deployment, latest map[string]*store.Deployment) []attentionItem {
	var items []attentionItem

	for _, d := range active {
		if d.State != store.StatePendingApproval {
			continue
		}
		c := s.card(d)
		detail := c.CommitSubj
		if detail == "" {
			detail = "commit " + c.CommitShort
		}
		items = append(items, attentionItem{
			Kind: "pending", KindLabel: "Needs approval", KindClass: "state-pending", Title: c.Title, Path: c.Path, Detail: detail, When: c.CreatedAt,
		})
	}

	blocking := map[string]bool{}
	for _, o := range obs {
		if o.BlockedBy == "" {
			continue
		}
		blocking[o.BlockedBy] = true
		it := attentionItem{
			Kind: "blocked", KindLabel: "Blocked", KindClass: "state-failed", Title: o.Namespace + "/" + o.JobID,
			Path: jobPath(o.Namespace, o.JobID), Detail: o.BlockedReason, RetryPath: jobPath(o.Namespace, o.JobID) + "/retry",
		}
		if d := latest[jobKey(o.Namespace, o.JobID)]; d != nil {
			it.When = s.when(d.UpdatedAt)
		}
		items = append(items, it)
	}

	failed := make([]*store.Deployment, 0)
	for _, d := range latest {
		if d.State == store.StateFailed && d.RetriedAt.IsZero() && !blocking[d.ID] && s.now().Sub(d.UpdatedAt) < recentFailure {
			failed = append(failed, d)
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].UpdatedAt.After(failed[j].UpdatedAt) })
	for _, d := range failed {
		c := s.card(d)
		detail := c.Error
		if detail == "" {
			detail = "the deployment failed"
		}
		items = append(items, attentionItem{
			Kind: "failed", KindLabel: "Failed", KindClass: "state-failed", Title: c.Title, Path: c.Path, Detail: detail, When: s.when(d.UpdatedAt),
		})
	}

	for _, o := range obs {
		for _, iss := range o.Issues {
			if iss.Severity != meta.SeverityError {
				continue
			}
			items = append(items, attentionItem{
				Kind: "invalid", KindLabel: "Invalid meta", KindClass: "state-failed", Title: o.Namespace + "/" + o.JobID, Path: jobPath(o.Namespace, o.JobID),
				Detail: iss.Key + ": " + iss.Message, When: s.when(o.ObservedAt),
			})
			break // one line per job: its page lists them all
		}
	}

	// Least urgent: nothing is broken, a service nobody asked for keeps
	// running. The operator decides; nops never stops it.
	for _, o := range s.engine.Orphans() {
		items = append(items, attentionItem{
			Kind: "orphan", KindLabel: "Not in git", KindClass: "state-pending", Title: o.Namespace + "/" + o.JobID,
			Path: jobPath(o.Namespace, o.JobID), Detail: "Removed from git, still running in Nomad",
		})
	}
	return items
}
