package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// formatTime is how an absolute timestamp is shown: UTC, so it reads the same
// whatever timezone the operator's browser is in.
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// timeView is a moment as the pages show it: relative ("3m ago") in the text,
// with the absolute UTC time on hover and the ISO one for the <time> element.
// The zero value means "no such moment" and renders as nothing.
type timeView struct {
	Rel, Full, ISO string
}

// when builds the timeView of t as seen from the server's clock.
func (s *server) when(t time.Time) timeView {
	if t.IsZero() {
		return timeView{}
	}
	return timeView{Rel: relative(s.now(), t), Full: formatTime(t), ISO: t.UTC().Format(time.RFC3339)}
}

// relative says how long before now t was. Under a minute it is "just now"
// (nops's own clocks and the operator's need not agree to the second, so a t a
// little in the future reads the same); past a month it is the date.
func relative(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	default:
		return t.UTC().Format("2006-01-02")
	}
}

// baseData is what every page template needs for its chrome (nav, actor).
// Page-specific data structs embed it so its fields are promoted, e.g.
// {{.Actor}} works from any page template.
type baseData struct {
	Actor string
	Nav   string // which nav tab is active: "overview", "jobs" or "history"
	Self  string // this request's URL (path and query): what a live page polls
}

func (s *server) base(r *http.Request, nav string) baseData {
	actor, _ := UserFrom(r.Context())
	return baseData{Actor: actor, Nav: nav, Self: r.URL.RequestURI()}
}

// duration writes d the way a person would: "10m", not Go's "10m0s".
func duration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// jobPath is where a job's page lives. ns and job are path-escaped: both can
// hold characters a URL path does not.
func jobPath(namespace, jobID string) string {
	return "/jobs/" + url.PathEscape(namespace) + "/" + url.PathEscape(jobID)
}

// deploymentCard is a deployment as shown in a list or on its own page. It
// never carries JobSpec: the full unredacted job (see
// docs/design/engine-detection.md#job_spec-keeps-the-full-unredacted-spec).
// Long identifiers come twice: the short form for the text and the full one
// for the hover title.
type deploymentCard struct {
	ID         string
	JobID      string
	Namespace  string
	Title      string // namespace/job
	JobPath    string
	Path       string // the deployment's own page
	Policy     store.Policy
	State      store.State
	StateLabel string
	StateClass string
	Error      string
	Retried    bool

	CommitSHA    string
	CommitShort  string
	CommitURL    string // "" when there is no link
	CommitSubj   string // "" for a deployment created before it was recorded
	CommitAuthor string

	SpecHash  string
	SpecShort string
	CASIndex  uint64
	EvalID    string
	EvalShort string
	Applied   bool // the register was reached

	DecidedBy string
	DecidedAt timeView
	RetriedBy string
	RetriedAt timeView
	CreatedAt timeView
	UpdatedAt timeView
}

func (s *server) card(d *store.Deployment) deploymentCard {
	c := deploymentCard{
		ID:           d.ID,
		JobID:        d.JobID,
		Namespace:    d.Namespace,
		Title:        d.Namespace + "/" + d.JobID,
		JobPath:      jobPath(d.Namespace, d.JobID),
		Path:         "/deployments/" + url.PathEscape(d.ID),
		Policy:       d.Policy,
		State:        d.State,
		StateLabel:   stateLabel(d.State),
		StateClass:   stateClass(d.State),
		Error:        d.Error,
		Retried:      !d.RetriedAt.IsZero(),
		CommitSHA:    d.CommitSHA,
		CommitShort:  shortCommit(d.CommitSHA),
		CommitSubj:   d.CommitSubject,
		CommitAuthor: d.CommitAuthor,
		SpecHash:     d.SpecHash,
		SpecShort:    short(12, d.SpecHash),
		CASIndex:     d.CASIndex,
		EvalID:       d.EvalID,
		EvalShort:    short(8, d.EvalID),
		Applied:      d.AppliedIndex != 0,
		DecidedBy:    d.DecidedBy,
		DecidedAt:    s.when(d.DecidedAt),
		RetriedBy:    d.RetriedBy,
		RetriedAt:    s.when(d.RetriedAt),
		CreatedAt:    s.when(d.CreatedAt),
		UpdatedAt:    s.when(d.UpdatedAt),
	}
	if s.commitURL != nil {
		c.CommitURL = s.commitURL(d.CommitSHA)
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
