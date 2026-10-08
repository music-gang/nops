package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// The JSON API under /api/ (docs/api.md). A bearer token acts as itself:
// requireToken puts it in the request's context where a session would, so
// every action below calls the engine as the dashboard does, with the same
// rules, and the token's name as the actor. There is no cookie here and so nothing for another
// site to ride on, which is why this does not use the cross-origin check.

// openAPIFile is the OpenAPI description of the API.
//
//go:embed openapi.json
var openAPIFile []byte

// maxAPIBody is the most a request body may hold: the largest field is a pause
// reason of 500 bytes.
const maxAPIBody = 64 << 10

// apiEndpoint is a route of the API, as the mux takes it ("GET /api/jobs"),
// with what its token needs. openapi.json describes each one, and a test keeps
// the two equal.
type apiEndpoint struct {
	pattern string
	check   check
	handler http.HandlerFunc
}

func (s *server) apiEndpoints() []apiEndpoint {
	endpoints := []apiEndpoint{
		{"GET /api/openapi.json", anyToken(), s.apiOpenAPI},
		{"GET /api/jobs", anyToken(), s.apiListJobs},
		{"GET /api/jobs/{namespace}/{job}", inNamespace(acl.Read), s.apiGetJob},
		{"GET /api/deployments", anyToken(), s.apiListDeployments},
		{"GET /api/deployments/{id}", onDeployment(acl.Read), s.apiGetDeployment},
		{"POST /api/deployments/{id}/approve", onDeployment(acl.Approve), s.apiApprove},
		{"POST /api/deployments/{id}/reject", onDeployment(acl.Approve), s.apiReject},
		{"POST /api/deployments/{id}/promote", onDeployment(acl.Promote), s.apiPromote},
		{"POST /api/deployments/{id}/retry", onDeployment(acl.Retry), s.apiRetry},
		{"POST /api/jobs/{namespace}/{job}/pause", inNamespace(acl.Pause), s.apiPause},
		{"POST /api/jobs/{namespace}/{job}/resume", inNamespace(acl.Pause), s.apiResume},
		{"POST /api/jobs/{namespace}/{job}/deploy-now", inNamespace(acl.DeployNow), s.apiDeployNow},
		{"GET /api/acl/policies", anyToken(), s.apiListPolicies},
		{"GET /api/acl/policies/{name}", anyToken(), s.apiGetPolicy},
		{"PUT /api/acl/policies/{name}", management(), s.apiPutPolicy},
		{"DELETE /api/acl/policies/{name}", management(), s.apiDeletePolicy},
		{"GET /api/acl/binding-rules", management(), s.apiListBindingRules},
		{"POST /api/acl/binding-rules", management(), s.apiCreateBindingRule},
		{"GET /api/acl/binding-rules/{id}", management(), s.apiGetBindingRule},
		{"PUT /api/acl/binding-rules/{id}", management(), s.apiPutBindingRule},
		{"DELETE /api/acl/binding-rules/{id}", management(), s.apiDeleteBindingRule},
		{"GET /api/acl/tokens", management(), s.apiListTokens},
		{"POST /api/acl/tokens", management(), s.apiCreateToken},
		{"GET /api/acl/tokens/{accessor_id}", management(), s.apiGetToken},
		{"DELETE /api/acl/tokens/{accessor_id}", management(), s.apiRevokeToken},
		{"POST /api/acl/tokens/revoke-sessions", management(), s.apiRevokeSessions},
		{"POST /api/acl/tokens/revoke-created", management(), s.apiRevokeCreated},
		{"GET /api/acl/token/self", anyToken(), s.apiSelfToken},
		{"GET /api/acl/changes", management(), s.apiChanges},
	}
	if s.trigger != nil {
		endpoints = append(endpoints, apiEndpoint{"POST /api/fetch", globally(acl.Fetch), s.apiFetch})
	}
	return endpoints
}

func (s *server) apiRoutes(mux *http.ServeMux) {
	for _, e := range s.apiEndpoints() {
		mux.Handle(e.pattern, s.requireToken(s.guard(e.check, apiForbidden, e.handler)))
	}
	// Anything else under /api/ is a JSON 404, behind the token like the rest:
	// whoever has none learns nothing about which paths exist.
	mux.Handle("/api/", s.requireToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiError(w, http.StatusNotFound, "no such endpoint")
	})))
}

// apiForbidden answers the 403 of a token that may not do what it asked.
func apiForbidden(w http.ResponseWriter, r *http.Request) {
	apiError(w, http.StatusForbidden, "this token does not allow that")
}

// apiOpenAPI serves the OpenAPI description of the API: a file with no secret,
// behind the token like the rest.
func (s *server) apiOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openAPIFile)
}

// requireToken lets a request through only with a valid bearer token, and puts
// who it stands for in its context: the subject, and the actor (UserFrom).
func (s *server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, secret, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(secret) == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			apiError(w, http.StatusUnauthorized, "a token is required: send it as \"Authorization: Bearer <token>\"")
			return
		}
		sub, err := s.tokenSubject(r.Context(), strings.TrimSpace(secret))
		switch {
		case errors.Is(err, store.ErrNotFound):
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			apiError(w, http.StatusUnauthorized, "the token is not valid: it is unknown, expired, or revoked")
		case err != nil:
			s.apiServerError(w, r, "read the token", err)
		default:
			ctx := context.WithValue(r.Context(), subjectKey{}, sub)
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, userKey{}, sub.actor)))
		}
	})
}

// -- answers ---------------------------------------------------------------

func replyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	replyJSON(w, status, map[string]string{"error": msg})
}

// apiServerError logs the error at ERROR and answers a generic 500, like
// serverError: the error may quote a Nomad or SQLite message.
func (s *server) apiServerError(w http.ResponseWriter, r *http.Request, action string, err error) {
	s.log.ErrorContext(r.Context(), "api error", "action", action, "error", err)
	apiError(w, http.StatusInternalServerError, "nops hit an error handling this request; it has been logged")
}

// readBody decodes the JSON body of a request into v. An empty body is fine: it
// leaves v as it is. It answers the 400 itself and returns false on a body that
// is too large, is not JSON, or has a field the endpoint does not know.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAPIBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		apiError(w, http.StatusBadRequest, "the request body is not the JSON this endpoint takes: "+err.Error())
		return false
	}
	return true
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// -- what the reads answer ---------------------------------------------------

type apiHold struct {
	Kind   string     `json:"kind"`
	Reason string     `json:"reason"`
	By     string     `json:"by,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
}

type apiDeployment struct {
	ID            string     `json:"id"`
	Namespace     string     `json:"namespace"`
	Job           string     `json:"job"`
	State         string     `json:"state"`
	Policy        string     `json:"policy"`
	SpecHash      string     `json:"spec_hash"`
	CommitSHA     string     `json:"commit_sha"`
	CommitSubject string     `json:"commit_subject,omitempty"`
	CommitAuthor  string     `json:"commit_author,omitempty"`
	Error         string     `json:"error,omitempty"`
	DecidedBy     string     `json:"decided_by,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	RetryOf       string     `json:"retry_of,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// deploymentOf is a deployment as the API shows it: never with its JobSpec,
// which is the full unredacted job.
func deploymentOf(d *store.Deployment) apiDeployment {
	return apiDeployment{
		ID: d.ID, Namespace: d.Namespace, Job: d.JobID, State: string(d.State), Policy: string(d.Policy),
		SpecHash: d.SpecHash, CommitSHA: d.CommitSHA, CommitSubject: d.CommitSubject, CommitAuthor: d.CommitAuthor,
		Error: d.Error, DecidedBy: d.DecidedBy, DecidedAt: timePtr(d.DecidedAt), RetryOf: d.RetryOf,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

func deploymentsOf(list []*store.Deployment) []apiDeployment {
	out := make([]apiDeployment, 0, len(list))
	for _, d := range list {
		out = append(out, deploymentOf(d))
	}
	return out
}

type apiJob struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Policy    string `json:"policy"`
	// Sync is the job's sync state, in the words of docs/dashboard.md#sync-state-of-a-job:
	// "invalid", "paused", "blocked", "orphan", "pending", "deploying", "held",
	// "drift" or "sync".
	Sync  string `json:"sync"`
	Drift bool   `json:"drift"`
	// SpecHash is what a deployment of the job would have now: the one Deploy now asks for.
	SpecHash       string         `json:"spec_hash,omitempty"`
	BlockedBy      string         `json:"blocked_by,omitempty"`
	Hold           *apiHold       `json:"hold,omitempty"`
	LastDeployment *apiDeployment `json:"last_deployment,omitempty"`
}

func jobOf(o engine.Observation, latest *store.Deployment) apiJob {
	j := apiJob{
		Namespace: o.Namespace, Job: o.JobID, Policy: string(o.Policy), Sync: string(jobSync(o, latest)),
		Drift: o.Drift, SpecHash: o.SpecHash, BlockedBy: o.BlockedBy,
	}
	if h := o.Hold; h != nil {
		j.Hold = &apiHold{Kind: string(h.Kind), Reason: h.Reason, By: h.By, Since: timePtr(h.Since)}
	}
	if latest != nil {
		d := deploymentOf(latest)
		j.LastDeployment = &d
	}
	return j
}

func orphanJob(o engine.Orphan, latest *store.Deployment) apiJob {
	j := apiJob{Namespace: o.Namespace, Job: o.JobID, Policy: string(o.Policy), Sync: string(syncOrphan)}
	if latest != nil {
		d := deploymentOf(latest)
		j.LastDeployment = &d
	}
	return j
}

type apiIssue struct {
	Severity string `json:"severity"`
	Key      string `json:"key"`
	Message  string `json:"message"`
}

func issuesOf(list []meta.Issue) []apiIssue {
	var out []apiIssue
	for _, i := range list {
		out = append(out, apiIssue{Severity: string(i.Severity), Key: i.Key, Message: i.Message})
	}
	return out
}

// apiListJobs is GET /api/jobs: every managed job, orphans included.
func (s *server) apiListJobs(w http.ResponseWriter, r *http.Request) {
	latest, err := s.latestByJob(r)
	if err != nil {
		s.apiServerError(w, r, "list latest deployments", err)
		return
	}
	jobs := []apiJob{}
	for _, o := range s.observations(r) {
		jobs = append(jobs, jobOf(o, latest[jobKey(o.Namespace, o.JobID)]))
	}
	for _, o := range s.orphans(r) {
		jobs = append(jobs, orphanJob(o, latest[jobKey(o.Namespace, o.JobID)]))
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		return jobKey(jobs[i].Namespace, jobs[i].Job) < jobKey(jobs[j].Namespace, jobs[j].Job)
	})
	replyJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

type apiJobDetail struct {
	apiJob
	File string `json:"file,omitempty"`
	// Diff is the redacted plan diff of the drift, as Nomad's JSON; absent without drift.
	Diff        json.RawMessage `json:"diff,omitempty"`
	Issues      []apiIssue      `json:"issues,omitempty"`
	Deployments []apiDeployment `json:"deployments"`
}

// apiGetJob is GET /api/jobs/{namespace}/{job}.
func (s *server) apiGetJob(w http.ResponseWriter, r *http.Request) {
	ns, id := r.PathValue("namespace"), r.PathValue("job")
	deps, err := s.store.ListByJob(r.Context(), ns, id, jobHistoryLimit)
	if err != nil {
		s.apiServerError(w, r, "list deployments of a job", err)
		return
	}
	var latest *store.Deployment
	if len(deps) > 0 {
		latest = deps[0]
	}
	var detail *apiJobDetail
	for _, o := range s.engine.Observations() {
		if o.Namespace == ns && o.JobID == id {
			detail = &apiJobDetail{apiJob: jobOf(o, latest), File: o.FilePath, Issues: issuesOf(o.Issues)}
			if o.PlanDiff != "" {
				if !json.Valid([]byte(o.PlanDiff)) {
					s.apiServerError(w, r, "read the drift diff", errors.New("the drift diff is not JSON"))
					return
				}
				detail.Diff = json.RawMessage(o.PlanDiff)
			}
			break
		}
	}
	if detail == nil {
		for _, o := range s.engine.Orphans() {
			if o.Namespace == ns && o.JobID == id {
				detail = &apiJobDetail{apiJob: orphanJob(o, latest)}
				break
			}
		}
	}
	if detail == nil && len(deps) == 0 {
		apiError(w, http.StatusNotFound, "this job does not exist")
		return
	}
	if detail == nil { // gone from git and from Nomad, with a past
		last := deploymentOf(latest)
		detail = &apiJobDetail{apiJob: apiJob{Namespace: ns, Job: id, Policy: string(latest.Policy), LastDeployment: &last}}
	}
	detail.Deployments = deploymentsOf(deps)
	replyJSON(w, http.StatusOK, detail)
}

// apiListDeployments is GET /api/deployments: the history page's list.
func (s *server) apiListDeployments(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListHistory(r.Context(), historyLimit)
	if err != nil {
		s.apiServerError(w, r, "list deployments", err)
		return
	}
	active, err := s.store.ListActive(r.Context())
	if err != nil {
		s.apiServerError(w, r, "list active deployments", err)
		return
	}
	all := readable(r, append(append([]*store.Deployment{}, active...), list...))
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID > all[j].ID
	})
	replyJSON(w, http.StatusOK, map[string]any{"deployments": deploymentsOf(all)})
}

type apiEvent struct {
	Time  time.Time `json:"time"`
	From  string    `json:"from"`
	To    string    `json:"to"`
	Actor string    `json:"actor"`
	// AccessorID is the token the action was made with; absent for Nops itself.
	AccessorID string `json:"accessor_id,omitempty"`
	Message    string `json:"message,omitempty"`
}

type apiHookRun struct {
	Phase      string     `json:"phase"`
	Job        string     `json:"job"`
	State      string     `json:"state"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type apiDeploymentDetail struct {
	apiDeployment
	// Diff is the redacted plan diff, as Nomad's JSON; absent when there is none.
	Diff     json.RawMessage `json:"diff,omitempty"`
	Events   []apiEvent      `json:"events"`
	HookRuns []apiHookRun    `json:"hook_runs"`
}

// apiGetDeployment is GET /api/deployments/{id}.
func (s *server) apiGetDeployment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, err := s.store.GetDeployment(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "this deployment does not exist")
		return
	}
	if err != nil {
		s.apiServerError(w, r, "get deployment", err)
		return
	}
	detail := apiDeploymentDetail{apiDeployment: deploymentOf(d), Events: []apiEvent{}, HookRuns: []apiHookRun{}}
	if d.PlanDiff != "" {
		if !json.Valid([]byte(d.PlanDiff)) {
			s.apiServerError(w, r, "read the deployment diff", errors.New("the plan diff is not JSON"))
			return
		}
		detail.Diff = json.RawMessage(d.PlanDiff)
	}
	events, err := s.store.Events(r.Context(), id)
	if err != nil {
		s.apiServerError(w, r, "list events", err)
		return
	}
	for _, e := range events {
		detail.Events = append(detail.Events, apiEvent{Time: e.Time, From: string(e.From), To: string(e.To), Actor: e.Actor, AccessorID: e.AccessorID, Message: e.Message})
	}
	runs, err := s.store.ListHookRuns(r.Context(), id)
	if err != nil {
		s.apiServerError(w, r, "list hook runs", err)
		return
	}
	for _, run := range runs {
		detail.HookRuns = append(detail.HookRuns, apiHookRun{
			Phase: run.Phase, Job: run.HookJobID, State: string(run.State), Error: run.Error,
			StartedAt: timePtr(run.StartedAt), FinishedAt: timePtr(run.FinishedAt),
		})
	}
	replyJSON(w, http.StatusOK, detail)
}

// -- actions -----------------------------------------------------------------

// specBody is the body of the actions that carry the spec hash the caller saw.
type specBody struct {
	SpecHash string `json:"spec_hash"`
}

// apiSpecHash reads the spec hash of the request, or answers the 400.
func apiSpecHash(w http.ResponseWriter, r *http.Request) (string, bool) {
	var b specBody
	if !readBody(w, r, &b) {
		return "", false
	}
	if b.SpecHash == "" {
		apiError(w, http.StatusBadRequest, `"spec_hash" is required: the spec hash you read`)
		return "", false
	}
	return b.SpecHash, true
}

func (s *server) apiApprove(w http.ResponseWriter, r *http.Request) {
	hash, ok := apiSpecHash(w, r)
	if !ok {
		return
	}
	actor, _ := UserFrom(r.Context())
	s.apiDecided(w, r, "approve deployment", s.engine.Approve(r.Context(), r.PathValue("id"), hash, actor))
}

func (s *server) apiReject(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFrom(r.Context())
	s.apiDecided(w, r, "reject deployment", s.engine.Reject(r.Context(), r.PathValue("id"), actor))
}

// apiDecided answers the result of an approve or a reject, with the status the
// dashboard's decide gives it.
func (s *server) apiDecided(w http.ResponseWriter, r *http.Request, action string, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "this deployment does not exist")
	case errors.Is(err, engine.ErrStaleApproval):
		apiError(w, http.StatusConflict, "the spec changed since you read it: read the deployment again before deciding")
	case errors.Is(err, engine.ErrNotInRepo):
		apiError(w, http.StatusConflict, "the job is not in the repository as nops last read it: there is nothing to approve until it is back, but you can reject it")
	case errors.Is(err, engine.ErrPaused):
		apiError(w, http.StatusConflict, "the job is paused: resume it to approve, or reject the deployment")
	default:
		s.apiServerError(w, r, action, err)
	}
}

func (s *server) apiPromote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor, _ := UserFrom(r.Context())
	err := s.engine.Promote(r.Context(), id, actor)
	switch {
	case err == nil:
		s.dropPanelOf(r, id)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "this deployment does not exist")
	case errors.Is(err, engine.ErrNotWaitingForPromotion):
		apiError(w, http.StatusConflict, "this deployment is not waiting for a canary promotion any more")
	default:
		s.apiServerError(w, r, "promote canaries", err)
	}
}

func (s *server) apiRetry(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFrom(r.Context())
	next, err := s.engine.Retry(r.Context(), r.PathValue("id"), actor)
	switch {
	case err == nil:
		replyJSON(w, http.StatusOK, map[string]string{"deployment_id": next})
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "this deployment does not exist")
	case errors.Is(err, engine.ErrNotRetryable), errors.Is(err, store.ErrAlreadyRetried):
		apiError(w, http.StatusConflict, "this deployment can no longer be retried: it was already retried, a newer deployment replaced it, or git has another spec for the job now")
	default:
		s.apiServerError(w, r, "retry deployment", err)
	}
}

// pauseBody is the optional body of a pause.
type pauseBody struct {
	Reason string `json:"reason,omitempty"`
}

func (s *server) apiPause(w http.ResponseWriter, r *http.Request) {
	var b pauseBody
	if !readBody(w, r, &b) {
		return
	}
	actor, _ := UserFrom(r.Context())
	err := s.engine.Pause(r.Context(), r.PathValue("namespace"), r.PathValue("job"), actor, b.Reason)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, engine.ErrNotPausable):
		apiError(w, http.StatusNotFound, "this job is not in the repository as nops last read it: there is nothing to pause")
	case errors.Is(err, store.ErrAlreadyPaused):
		apiError(w, http.StatusConflict, "this job is already paused")
	case errors.Is(err, engine.ErrPauseNoteTooLong):
		apiError(w, http.StatusBadRequest, "the reason for a pause can be 500 bytes at most")
	default:
		s.apiServerError(w, r, "pause job", err)
	}
}

func (s *server) apiResume(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFrom(r.Context())
	err := s.engine.Resume(r.Context(), r.PathValue("namespace"), r.PathValue("job"), actor)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, engine.ErrNotPausable):
		apiError(w, http.StatusNotFound, "this job does not exist")
	case errors.Is(err, store.ErrNotPaused):
		apiError(w, http.StatusConflict, "this job is not paused")
	default:
		s.apiServerError(w, r, "resume job", err)
	}
}

// apiDeployNow answers the id of the deployment the request led to, or an empty
// one when the cycle left the request to the detection loop.
func (s *server) apiDeployNow(w http.ResponseWriter, r *http.Request) {
	hash, ok := apiSpecHash(w, r)
	if !ok {
		return
	}
	actor, _ := UserFrom(r.Context())
	next, err := s.engine.DeployNow(r.Context(), r.PathValue("namespace"), r.PathValue("job"), hash, actor)
	switch {
	case err == nil:
		replyJSON(w, http.StatusOK, map[string]string{"deployment_id": next})
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "this job does not exist")
	case errors.Is(err, engine.ErrNotDeployable):
		apiError(w, http.StatusConflict, "this job can no longer be deployed outside its sync window: the window opened, a deployment started, the job is paused or blocked, or git has another spec for it now")
	default:
		s.apiServerError(w, r, "deploy job now", err)
	}
}

// apiFetch asks the git watcher for a poll and answers at once: the poll is
// asynchronous, like the dashboard's Fetch now.
func (s *server) apiFetch(w http.ResponseWriter, r *http.Request) {
	s.trigger()
	w.WriteHeader(http.StatusAccepted)
}
