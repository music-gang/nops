package web

import (
	"errors"
	"net/http"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// -- GET / : pending deployments, PR-style -------------------------------

type indexData struct {
	baseData
	Pending []deploymentCard // pending_approval: needs a human
	Active  []deploymentCard // detected, pre_hook, applying, post_hook: already moving
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	active, err := s.store.ListActive(r.Context())
	if err != nil {
		s.serverError(w, r, "list active deployments", err)
		return
	}
	data := indexData{baseData: s.base(r, "pending")}
	for _, d := range active {
		card := toCard(d)
		if d.State == store.StatePendingApproval {
			data.Pending = append(data.Pending, card)
		} else {
			data.Active = append(data.Active, card)
		}
	}
	s.render(w, r, "index", data)
}

// -- GET /history : past deployments -------------------------------------

type historyData struct {
	baseData
	Deployments []deploymentCard
}

func (s *server) history(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListHistory(r.Context(), historyLimit)
	if err != nil {
		s.serverError(w, r, "list history", err)
		return
	}
	data := historyData{baseData: s.base(r, "history")}
	for _, d := range list {
		data.Deployments = append(data.Deployments, toCard(d))
	}
	s.render(w, r, "history", data)
}

// -- GET /drift : drift of every managed job, "none" policy included ----

type observationView struct {
	JobID, Namespace, FilePath string
	Policy                     meta.Policy
	Drift                      bool
	Diff                       *api.JobDiff
	Issues                     []meta.Issue
	ObservedAt                 string
	BlockedBy                  string
	BlockedReason              string
}

type driftData struct {
	baseData
	Observations []observationView
}

func (s *server) drift(w http.ResponseWriter, r *http.Request) {
	obs := s.engine.Observations()
	data := driftData{baseData: s.base(r, "drift")}
	for _, o := range obs {
		diff, err := parseDiff(o.PlanDiff)
		if err != nil {
			s.serverError(w, r, "parse drift diff", err)
			return
		}
		data.Observations = append(data.Observations, observationView{
			JobID:         o.JobID,
			Namespace:     o.Namespace,
			FilePath:      o.FilePath,
			Policy:        o.Policy,
			Drift:         o.Drift,
			Diff:          diff,
			Issues:        o.Issues,
			ObservedAt:    formatTime(o.ObservedAt),
			BlockedBy:     o.BlockedBy,
			BlockedReason: o.BlockedReason,
		})
	}
	s.render(w, r, "drift", data)
}

// -- GET /deployments/{id} : diff, decision, timeline --------------------

type eventView struct {
	Time       string
	From, To   store.State
	Actor, Msg string
}

type hookRunView struct {
	Phase, JobID string
	State        store.HookState
	Error        string
	StartedAt    string
	FinishedAt   string // "" if not finished
}

type deploymentDetailData struct {
	baseData
	Deployment deploymentCard
	Diff       *api.JobDiff
	Events     []eventView
	HookRuns   []hookRunView
	CanDecide  bool   // state is pending_approval: show Approve/Reject
	Notice     string // set after a stale-approval conflict
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
		Deployment: toCard(d),
		Diff:       diff,
		CanDecide:  d.State == store.StatePendingApproval,
		Notice:     notice,
	}
	for _, e := range events {
		data.Events = append(data.Events, eventView{
			Time: formatTime(e.Time), From: e.From, To: e.To, Actor: e.Actor, Msg: e.Message,
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
		hv := hookRunView{Phase: run.Phase, JobID: run.HookJobID, State: run.State, Error: run.Error, StartedAt: formatTime(run.StartedAt)}
		if !run.FinishedAt.IsZero() {
			hv.FinishedAt = formatTime(run.FinishedAt)
		}
		data.HookRuns = append(data.HookRuns, hv)
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
// a deployment is non-terminal: the state badge and the events timeline. It
// stops polling itself once the deployment is terminal (isTerminal in the
// template drops the hx-trigger/hx-get on the next swap).
func (s *server) deploymentStatus(w http.ResponseWriter, r *http.Request) {
	data, ok := s.deploymentView(w, r, r.PathValue("id"), "")
	if !ok {
		return
	}
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
