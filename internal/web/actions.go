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
// The optional "next" form value says which page to go back to (the button
// lives on more than one); it is confined to this site by safeNext.
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
		http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
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
func (s *server) fetchNow(w http.ResponseWriter, r *http.Request) {
	s.trigger()
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}
