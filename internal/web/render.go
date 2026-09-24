package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// formatTime is how every timestamp is shown: UTC, so it reads the same
// whatever timezone the operator's browser is in.
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// baseData is what every page template needs for its chrome (nav, actor).
// Page-specific data structs embed it so its fields are promoted, e.g.
// {{.Actor}} works from any page template.
type baseData struct {
	Actor string
	Nav   string // which nav tab is active: "pending", "history" or "drift"
}

func (s *server) base(r *http.Request, nav string) baseData {
	actor, _ := UserFrom(r.Context())
	return baseData{Actor: actor, Nav: nav}
}

// deploymentCard is a deployment as shown in a list (pending, active,
// history) or in the decision rail of its own page. It never carries
// JobSpec: the full unredacted job (see
// docs/design/engine-detection.md#job_spec-keeps-the-full-unredacted-spec).
type deploymentCard struct {
	ID           string
	JobID        string
	Namespace    string
	CommitSHA    string
	SpecHash     string
	State        store.State
	StateLabel   string
	StateClass   string
	Error        string
	DecidedBy    string
	DecidedAt    string // formatted, "" if not decided
	CreatedAt    string
	AppliedIndex uint64
	EvalID       string
}

func toCard(d *store.Deployment) deploymentCard {
	c := deploymentCard{
		ID:           d.ID,
		JobID:        d.JobID,
		Namespace:    d.Namespace,
		CommitSHA:    d.CommitSHA,
		SpecHash:     d.SpecHash,
		State:        d.State,
		StateLabel:   stateLabel(d.State),
		StateClass:   stateClass(d.State),
		Error:        d.Error,
		DecidedBy:    d.DecidedBy,
		CreatedAt:    formatTime(d.CreatedAt),
		AppliedIndex: d.AppliedIndex,
		EvalID:       d.EvalID,
	}
	if !d.DecidedAt.IsZero() {
		c.DecidedAt = formatTime(d.DecidedAt)
	}
	return c
}

// render executes a full page template (its own chrome included) and writes
// it, or falls back to serverError on failure. Templates render into a
// buffer first so a mid-render error never leaves a half-written page with a
// 200 status.
func (s *server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf strings.Builder
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.serverError(w, r, "render "+name, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(buf.String()))
}

type errorData struct {
	baseData
	Status  int
	Title   string
	Message string
}

// notFound renders a styled 404 for an unknown deployment ID. The nav stays
// on whichever tab the visitor came from is not tracked, so it defaults to
// pending; a 404 is not somewhere people navigate around from anyway.
func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	s.notFoundMessage(w, r, "This deployment does not exist.")
}

func (s *server) notFoundMessage(w http.ResponseWriter, r *http.Request, msg string) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, r, "error", errorData{
		baseData: s.base(r, ""),
		Status:   http.StatusNotFound,
		Title:    "Not found",
		Message:  msg,
	})
}

// serverError logs the error at ERROR (fail loud: docs/error-handling.md)
// and renders a generic 500. It never puts err's text in the response: it
// may quote a Nomad or SQLite error that isn't meant for the browser.
func (s *server) serverError(w http.ResponseWriter, r *http.Request, action string, err error) {
	s.log.ErrorContext(r.Context(), "dashboard error", "action", action, "error", err)
	w.WriteHeader(http.StatusInternalServerError)
	s.render(w, r, "error", errorData{
		baseData: s.base(r, ""),
		Status:   http.StatusInternalServerError,
		Title:    "Something went wrong",
		Message:  "nops hit an error handling this page; it has been logged.",
	})
}

// getDeployment fetches a deployment and writes the right response itself on
// failure (404 or 500), so callers only need to check ok.
func (s *server) getDeployment(w http.ResponseWriter, r *http.Request, id string) (*store.Deployment, bool) {
	d, err := s.store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil, false
	}
	if err != nil {
		s.serverError(w, r, "get deployment", err)
		return nil, false
	}
	return d, true
}

// -- state labels and CSS classes, shared by the Go view models and the
// templates (registered in templateFuncs) --------------------------------

func stateLabel(st store.State) string {
	switch st {
	case store.StateDetected:
		return "Detected"
	case store.StatePendingApproval:
		return "Pending approval"
	case store.StatePreHook:
		return "Running pre-hook"
	case store.StateApplying:
		return "Applying"
	case store.StatePostHook:
		return "Running post-hook"
	case store.StateCompleted:
		return "Completed"
	case store.StateFailed:
		return "Failed"
	case store.StateRejected:
		return "Rejected"
	case store.StateSuperseded:
		return "Superseded"
	default:
		return string(st)
	}
}

// stateClass buckets a deployment state into one of the palette's semantic
// colors (docs/dashboard.md's look: pending amber, running blue, completed
// green, failed red, rejected/superseded muted).
func stateClass(st store.State) string {
	switch st {
	case store.StateDetected, store.StatePendingApproval:
		return "state-pending"
	case store.StatePreHook, store.StateApplying, store.StatePostHook:
		return "state-running"
	case store.StateCompleted:
		return "state-success"
	case store.StateFailed:
		return "state-failed"
	case store.StateRejected, store.StateSuperseded:
		return "state-muted"
	default:
		return "state-muted"
	}
}

// isTerminal reports whether a deployment no longer changes on its own: the
// status fragment stops polling once true.
func isTerminal(st store.State) bool {
	switch st {
	case store.StateCompleted, store.StateFailed, store.StateRejected, store.StateSuperseded:
		return true
	default:
		return false
	}
}

func hookStateLabel(st store.HookState) string {
	switch st {
	case store.HookDispatching:
		return "Dispatching"
	case store.HookRunning:
		return "Running"
	case store.HookSucceeded:
		return "Succeeded"
	case store.HookFailed:
		return "Failed"
	case store.HookTimedOut:
		return "Timed out"
	default:
		return string(st)
	}
}

func hookStateClass(st store.HookState) string {
	switch st {
	case store.HookDispatching, store.HookRunning:
		return "state-running"
	case store.HookSucceeded:
		return "state-success"
	case store.HookFailed, store.HookTimedOut:
		return "state-failed"
	default:
		return "state-muted"
	}
}
