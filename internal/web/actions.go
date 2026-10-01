package web

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/store"
)

// retry is POST /deployments/{id}/retry: try a failed or rejected deployment
// again, without a new commit (Engine.Retry). It never applies anything: the
// deployment it creates follows the job's policy, so under "approval" it still
// waits for a human decision. Engine.Retry has run the detection cycle that
// creates it by the time it answers, so the page the person lands on already
// shows it.
//
// The form's "back" names the page to return to (see backTo): nothing the
// browser sends reaches the Location header as a path or URL. Without one the
// person goes to the deployment that retries this one.
func (s *server) retry(w http.ResponseWriter, r *http.Request) {
	actor, ok := UserFrom(r.Context())
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	next, err := s.engine.Retry(r.Context(), id, actor)
	switch {
	case err == nil:
		http.Redirect(w, r, s.afterRetry(r, id, next), http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, engine.ErrNotRetryable), errors.Is(err, store.ErrAlreadyRetried):
		s.conflict(w, r, "Nothing to retry", "This deployment can no longer be retried: it was already retried, a newer deployment replaced it, or git has another spec for the job now.")
	default:
		s.serverError(w, r, "retry deployment", err)
	}
}

// afterRetry is where a retry sends the person: the page the form named, or the
// deployment that retries the one they asked about.
func (s *server) afterRetry(r *http.Request, id, next string) string {
	back := r.FormValue("back")
	if back == "" {
		return s.basePath + "/deployments/" + url.PathEscape(next)
	}
	var ns, job string
	if d, err := s.store.GetDeployment(r.Context(), id); err == nil {
		ns, job = d.Namespace, d.JobID
	}
	return s.backTo(back, ns, job)
}

// pause is POST /jobs/{namespace}/{job}/pause: hold a job so nops starts no new
// deployment for it until it is resumed (Engine.Pause). It only ever subtracts:
// it creates and approves nothing. The form's optional "reason" is kept with
// who paused and when.
func (s *server) pause(w http.ResponseWriter, r *http.Request) {
	actor, ok := UserFrom(r.Context())
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	ns, job := r.PathValue("namespace"), r.PathValue("job")
	err := s.engine.Pause(r.Context(), ns, job, actor, r.FormValue("reason"))
	switch {
	case err == nil:
		http.Redirect(w, r, s.backTo(r.FormValue("back"), ns, job), http.StatusSeeOther)
	case errors.Is(err, engine.ErrNotPausable):
		s.notFoundMessage(w, r, "This job is not in the repository as nops last read it: there is nothing to pause.")
	case errors.Is(err, store.ErrAlreadyPaused):
		s.conflict(w, r, "Already paused", "This job is already paused: someone paused it since the page was rendered.")
	case errors.Is(err, engine.ErrPauseNoteTooLong):
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "error", errorData{
			baseData: s.base(r, ""),
			Status:   http.StatusBadRequest,
			Title:    "Reason too long",
			Message:  "The reason for a pause can be 500 bytes at most. Go back and shorten it.",
		})
	default:
		s.serverError(w, r, "pause job", err)
	}
}

// resume is POST /jobs/{namespace}/{job}/resume: lift a pause (Engine.Resume).
// The next detection cycle treats the job as any other.
func (s *server) resume(w http.ResponseWriter, r *http.Request) {
	actor, ok := UserFrom(r.Context())
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	ns, job := r.PathValue("namespace"), r.PathValue("job")
	err := s.engine.Resume(r.Context(), ns, job, actor)
	switch {
	case err == nil:
		http.Redirect(w, r, s.backTo(r.FormValue("back"), ns, job), http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This job does not exist.")
	case errors.Is(err, store.ErrNotPaused):
		s.conflict(w, r, "Not paused", "This job is not paused: it was already resumed since the page was rendered.")
	default:
		s.serverError(w, r, "resume job", err)
	}
}

// conflict renders the 409 page of an action that found the job in another
// state than the page it was sent from said.
func (s *server) conflict(w http.ResponseWriter, r *http.Request, title, message string) {
	w.WriteHeader(http.StatusConflict)
	s.render(w, r, "error", errorData{
		baseData: s.base(r, ""),
		Status:   http.StatusConflict,
		Title:    title,
		Message:  message,
	})
}

// promote is POST /deployments/{id}/promote: promote the canaries of the Nomad
// deployment an applying deployment waits on (Engine.Promote). Like Approve it
// is an authenticated human action; unlike it, it applies nothing new: it
// releases what the job's own update block asked to hold for a person.
func (s *server) promote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor, ok := UserFrom(r.Context())
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	err := s.engine.Promote(r.Context(), id, actor)
	switch {
	case err == nil:
		s.dropPanelOf(r, id)
		http.Redirect(w, r, s.basePath+"/deployments/"+id, http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, engine.ErrNotWaitingForPromotion):
		w.WriteHeader(http.StatusConflict)
		s.render(w, r, "error", errorData{
			baseData: s.base(r, ""),
			Status:   http.StatusConflict,
			Title:    "Nothing to promote",
			Message:  "This deployment is not waiting for a canary promotion any more: the canaries were already promoted, or the Nomad deployment moved on.",
		})
	default:
		s.serverError(w, r, "promote canaries", err)
	}
}

// dropPanelOf forgets the cached Nomad panel of a deployment's job, so the page
// a write sends back to reads what Nomad says after it, not up to panelTTL
// before. If the deployment cannot be read, the panel is only that old.
func (s *server) dropPanelOf(r *http.Request, id string) {
	if s.nomad == nil {
		return
	}
	d, err := s.store.GetDeployment(r.Context(), id)
	if err != nil {
		s.log.ErrorContext(r.Context(), "read the deployment to refresh its Nomad panel", "deployment_id", id, "error", err)
		return
	}
	s.panels.mu.Lock()
	defer s.panels.mu.Unlock()
	delete(s.panels.entries, jobKey(d.Namespace, d.JobID))
}

// fetchNow is POST /fetch: ask the git watcher for a poll right away, the
// same trigger the git webhook pulls, instead of waiting for the next tick.
// It never blocks and says nothing about the outcome: the poll is
// asynchronous, and its result shows up as the head and the status of git.
// Like retry it always goes back to "/".
func (s *server) fetchNow(w http.ResponseWriter, r *http.Request) {
	s.trigger()
	http.Redirect(w, r, s.basePath+"/", http.StatusSeeOther)
}

// backTo is the page a write returns to, chosen from a fixed list by the name
// the form sends ("jobs", "activity", "job"; anything else is the Overview).
// A name is not a path: every result is a constant or built here from what
// nops itself knows, so there is no redirect to validate (decision log,
// 2026-09-24).
func (s *server) backTo(name, namespace, jobID string) string {
	switch name {
	case "jobs":
		return s.basePath + "/jobs"
	case "activity":
		return s.basePath + "/history"
	case "job":
		// The job's own page, from the engine's copy of its name, not the
		// request's.
		for _, o := range s.engine.Observations() {
			if o.Namespace == namespace && o.JobID == jobID {
				return s.jobPath(o.Namespace, o.JobID)
			}
		}
	}
	return s.basePath + "/"
}
