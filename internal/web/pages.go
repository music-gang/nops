package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// -- GET /history : every deployment, by day ---------------------------------

type activityDay struct {
	Label string
	Rows  []deploymentCard
}

type activityData struct {
	baseData
	Days    []activityDay
	Filters []filterLink
	Filter  string // "" for all
	Total   int
	Limited bool // older deployments exist that this page does not list
}

// activityFilters are the choices of the Activity page: "active" is every
// deployment that has not finished, the rest are the terminal states.
var activityFilters = []struct{ key, label string }{
	{"active", "In progress"},
	{string(store.StateCompleted), "Completed"},
	{string(store.StateFailed), "Failed"},
	{string(store.StateRejected), "Rejected"},
	{string(store.StateSuperseded), "Superseded"},
}

// activityKey is the filter a deployment belongs to.
func activityKey(st store.State) string {
	if st.IsActive() {
		return "active"
	}
	return string(st)
}

func (s *server) history(w http.ResponseWriter, r *http.Request) {
	active, err := s.store.ListActive(r.Context())
	if err != nil {
		s.serverError(w, r, "list active deployments", err)
		return
	}
	past, err := s.store.ListHistory(r.Context(), historyLimit)
	if err != nil {
		s.serverError(w, r, "list history", err)
		return
	}
	all := append(append([]*store.Deployment{}, active...), past...)
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID > all[j].ID
	})

	filter := r.URL.Query().Get("state")
	valid := false
	for _, f := range activityFilters {
		valid = valid || f.key == filter
	}
	if !valid {
		filter = ""
	}

	data := activityData{baseData: s.base(r, "history"), Filter: filter, Total: len(all), Limited: len(past) == historyLimit}
	counts := map[string]int{}
	today := s.now().UTC().Truncate(24 * time.Hour)
	for _, d := range all {
		counts[activityKey(d.State)]++
		if filter != "" && activityKey(d.State) != filter {
			continue
		}
		label := dayLabel(today, d.CreatedAt)
		if n := len(data.Days); n == 0 || data.Days[n-1].Label != label {
			data.Days = append(data.Days, activityDay{Label: label})
		}
		day := &data.Days[len(data.Days)-1]
		day.Rows = append(day.Rows, s.card(d))
	}
	data.Filters = append(data.Filters, filterLink{Label: "All", Count: len(all), Active: filter == ""})
	for _, f := range activityFilters {
		if counts[f.key] > 0 || f.key == filter {
			data.Filters = append(data.Filters, filterLink{Key: f.key, Label: f.label, Count: counts[f.key], Active: f.key == filter})
		}
	}
	s.render(w, r, "activity", data)
}

// dayLabel names the UTC day of t as seen from today (UTC midnight).
func dayLabel(today, t time.Time) string {
	day := t.UTC().Truncate(24 * time.Hour)
	switch {
	case day.Equal(today):
		return "Today"
	case day.Equal(today.Add(-24 * time.Hour)):
		return "Yesterday"
	default:
		return day.Format("Mon, 2 Jan 2006")
	}
}

// -- GET /drift : moved to /jobs -------------------------------------------

// drift keeps the old address working: the drift of every managed job is the
// Jobs page now.
func (s *server) drift(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/jobs", http.StatusMovedPermanently)
}

// -- GET /deployments/{id} : review, decision, hooks, timeline --------------

type eventView struct {
	Time     timeView
	From, To store.State
	Actor    string
	Msg      string
	Retry    bool // from and to are the same terminal state: a retry was asked
}

type hookRunView struct {
	Phase, JobID string
	State        store.HookState
	Error        string
	StartedAt    timeView
	FinishedAt   timeView // zero if not finished
}

// planStep is one thing Approve sets in motion, in order.
type planStep struct {
	Kind string // "pre", "register", "health" or "post"
	Job  string // the hook's job ID, for "pre" and "post"
	// Revision is the first 8 hex of the hook's spec hash: which version of the
	// hook runs.
	Revision string
	Timeout  string
	Text     string // for "register"
}

// blockingView says a deployment is what holds its job's drift back, and how
// to lift it.
type blockingView struct {
	Reason    string
	RetryPath string
}

type deploymentDetailData struct {
	baseData
	Deployment deploymentCard
	Blocking   *blockingView // set while this deployment blocks its job
	Diff       *api.JobDiff
	Summary    diffSummary
	Steps      []planStep // what Approve will do; only while it can be approved
	Events     []eventView
	HookRuns   []hookRunView
	CanDecide  bool   // state is pending_approval: show Approve/Reject
	Notice     string // set after a stale-approval conflict
	OOB        bool   // the status fragment: the side column swaps out of band
}

// planSteps reads what approving d will run: the hooks it froze at detection
// (which one, and at which revision) with the timeouts its spec declares, and
// the register and health steps between them. The spec itself is never
// rendered.
func planSteps(d *store.Deployment, hooks []store.DeploymentHook) ([]planStep, error) {
	var job api.Job
	if err := json.Unmarshal([]byte(d.JobSpec), &job); err != nil {
		return nil, fmt.Errorf("read the spec of deployment %s: %w", d.ID, err)
	}
	cfg := meta.Parse(job.Meta)

	hookStep := func(kind string, h store.DeploymentHook, declared *meta.Hook) planStep {
		st := planStep{Kind: kind, Job: h.HookID, Revision: strings.TrimPrefix(h.Revision, h.HookID+"-")}
		if declared != nil {
			st.Timeout = duration(declared.Timeout)
		}
		return st
	}
	var steps []planStep
	for _, h := range hooks {
		if h.Phase == "pre" {
			steps = append(steps, hookStep("pre", h, cfg.PreHook))
		}
	}
	register := "Create the job in Nomad (it is not registered yet)."
	if d.CASIndex != 0 {
		register = fmt.Sprintf("Update the job in Nomad, only if it has not changed since (index %d).", d.CASIndex)
	}
	steps = append(steps, planStep{Kind: "register", Text: register}, planStep{Kind: "health"})
	for _, h := range hooks {
		if h.Phase == "post" {
			steps = append(steps, hookStep("post", h, cfg.PostHook))
		}
	}
	return steps, nil
}

// deploymentView assembles everything /deployments/{id} and its status
// fragment need, or writes the error response itself and returns ok=false.
func (s *server) deploymentView(w http.ResponseWriter, r *http.Request, id, notice string) (deploymentDetailData, bool) {
	d, ok := s.getDeployment(w, r, id)
	if !ok {
		return deploymentDetailData{}, false
	}
	diff, err := parseDiff(d.PlanDiff)
	if err != nil {
		s.serverError(w, r, "parse deployment diff", err)
		return deploymentDetailData{}, false
	}
	events, err := s.store.Events(r.Context(), id)
	if err != nil {
		s.serverError(w, r, "list events", err)
		return deploymentDetailData{}, false
	}
	data := deploymentDetailData{
		baseData:   s.base(r, ""),
		Deployment: s.card(d),
		Diff:       diff,
		Summary:    summarize(diff),
		CanDecide:  d.State == store.StatePendingApproval,
		Notice:     notice,
	}
	for _, o := range s.engine.Observations() {
		if o.BlockedBy == d.ID {
			data.Blocking = &blockingView{Reason: o.BlockedReason, RetryPath: jobPath(o.Namespace, o.JobID) + "/retry"}
			break
		}
	}
	if data.CanDecide {
		frozen, err := s.store.DeploymentHooks(r.Context(), id)
		if err != nil {
			s.serverError(w, r, "list frozen hooks", err)
			return deploymentDetailData{}, false
		}
		if data.Steps, err = planSteps(d, frozen); err != nil {
			s.serverError(w, r, "plan steps", err)
			return deploymentDetailData{}, false
		}
	}
	for _, e := range events {
		data.Events = append(data.Events, eventView{
			Time: s.when(e.Time), From: e.From, To: e.To, Actor: e.Actor, Msg: e.Message, Retry: e.From == e.To,
		})
	}
	for _, phase := range []string{"pre", "post"} {
		run, err := s.store.GetHookRun(r.Context(), id, phase)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			s.serverError(w, r, "get hook run", err)
			return deploymentDetailData{}, false
		}
		data.HookRuns = append(data.HookRuns, hookRunView{
			Phase: run.Phase, JobID: run.HookJobID, State: run.State, Error: run.Error,
			StartedAt: s.when(run.StartedAt), FinishedAt: s.when(run.FinishedAt),
		})
	}
	return data, true
}

func (s *server) deployment(w http.ResponseWriter, r *http.Request) {
	data, ok := s.deploymentView(w, r, r.PathValue("id"), "")
	if !ok {
		return
	}
	s.render(w, r, "deployment", data)
}

// deploymentStatus is the htmx fragment polled by the deployment page while
// a deployment is non-terminal: the head (state, decision) and, out of band,
// the side column (details, hooks, timeline); the diff, which does not
// change, is not sent again. It stops polling itself once the deployment is
// terminal (isTerminal in the template drops the hx-trigger/hx-get on the next
// swap).
func (s *server) deploymentStatus(w http.ResponseWriter, r *http.Request) {
	data, ok := s.deploymentView(w, r, r.PathValue("id"), "")
	if !ok {
		return
	}
	data.OOB = true
	s.render(w, r, "status_fragment", data)
}

// -- POST /deployments/{id}/approve and /reject --------------------------

// decide runs an approve or reject call and redirects back to the
// deployment page. approve is nil for a reject.
func (s *server) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	id := r.PathValue("id")
	actor, ok := UserFrom(r.Context())
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	var err error
	if approve {
		err = s.engine.Approve(ctx, id, r.FormValue("spec_hash"), actor)
	} else {
		err = s.engine.Reject(ctx, id, actor)
	}
	switch {
	case err == nil:
		http.Redirect(w, r, "/deployments/"+id, http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, engine.ErrStaleApproval):
		data, ok := s.deploymentView(w, r, id, "The spec changed since this page loaded: review the new diff before deciding.")
		if !ok {
			return
		}
		w.WriteHeader(http.StatusConflict)
		s.render(w, r, "deployment", data)
	default:
		action := "approve deployment"
		if !approve {
			action = "reject deployment"
		}
		s.serverError(w, r, action, err)
	}
}

func (s *server) approve(w http.ResponseWriter, r *http.Request) { s.decide(w, r, true) }
func (s *server) reject(w http.ResponseWriter, r *http.Request)  { s.decide(w, r, false) }
