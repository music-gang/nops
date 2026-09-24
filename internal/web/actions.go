package web

import (
	"errors"
	"net/http"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/store"
)

// retry is POST /jobs/{namespace}/{job}/retry: unblock a job whose drift is
// suppressed by a failed or rejected deployment, without a new commit
// (Engine.Retry). It never applies anything: the next deployment follows the
// job's policy, so under "approval" it still waits for a human decision.
//
// The form's "back" names the page to return to (see backTo): nothing the
// browser sends reaches the Location header as a path or URL.
func (s *server) retry(w http.ResponseWriter, r *http.Request) {
	actor, ok := UserFrom(r.Context())
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	ns, job := r.PathValue("namespace"), r.PathValue("job")
	err := s.engine.Retry(r.Context(), ns, job, actor)
	switch {
	case err == nil:
		http.Redirect(w, r, s.backTo(r.FormValue("back"), ns, job), http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This job does not exist.")
	case errors.Is(err, engine.ErrNotBlocked), errors.Is(err, store.ErrAlreadyRetried):
		w.WriteHeader(http.StatusConflict)
		s.render(w, r, "error", errorData{
			baseData: s.base(r, ""),
			Status:   http.StatusConflict,
			Title:    "Nothing to retry",
			Message:  "This job is no longer blocked: it was already retried, or a newer deployment replaced the failed one.",
		})
	default:
		s.serverError(w, r, "retry job", err)
	}
}

// fetchNow is POST /fetch: ask the git watcher for a poll right away, the
// same trigger the git webhook pulls, instead of waiting for the next tick.
// It never blocks and says nothing about the outcome: the poll is
// asynchronous, and its result shows up as the head and the status of git.
// Like retry it always goes back to "/".
func (s *server) fetchNow(w http.ResponseWriter, r *http.Request) {
	s.trigger()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// backTo is the page a write returns to, chosen from a fixed list by the name
// the form sends ("jobs", "activity", "job"; anything else is the Overview).
// A name is not a path: every result is a constant or built here from what
// nops itself knows, so there is no redirect to validate (decision log,
// 2026-09-24).
func (s *server) backTo(name, namespace, jobID string) string {
	switch name {
	case "jobs":
		return "/jobs"
	case "activity":
		return "/history"
	case "job":
		// The job's own page, from the engine's copy of its name, not the
		// request's.
		for _, o := range s.engine.Observations() {
			if o.Namespace == namespace && o.JobID == jobID {
				return jobPath(o.Namespace, o.JobID)
			}
		}
	}
	return "/"
}
