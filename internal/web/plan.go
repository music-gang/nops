package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// maxPlanBody is the most the body of a plan may hold: every job file and
// var-file of a repository.
const maxPlanBody = 8 << 20

// commitPattern is a full commit SHA, SHA-1 or SHA-256: what nops_commit is
// everywhere else.
var commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

type apiPlanRequest struct {
	Repository string        `json:"repository"`
	Commit     string        `json:"commit"`
	Files      []apiPlanFile `json:"files"`
}

type apiPlanFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type apiPlan struct {
	Jobs    []apiPlannedJob `json:"jobs"`
	Errors  []apiPlanError  `json:"errors"`
	Removed []apiRemovedJob `json:"removed"`
	// RemovedUnchecked says a file of a repository does not parse, which
	// keeps Nops from reading any job as removed.
	RemovedUnchecked bool `json:"removed_unchecked,omitempty"`
}

type apiPlannedJob struct {
	apiJob
	// Repository and File are where the job file is: the repository planned,
	// or another one for a job that runs a hook of it.
	Repository string `json:"repository"`
	File       string `json:"file"`
	// Diff is the redacted plan diff, as Nomad's JSON; absent without drift.
	Diff      json.RawMessage `json:"diff,omitempty"`
	PreHooks  []string        `json:"pre_hooks,omitempty"`
	PostHooks []string        `json:"post_hooks,omitempty"`
	Issues    []apiIssue      `json:"issues,omitempty"`
	// Error is why the job would not deploy: invalid meta, a hook missing or
	// invalid, or what Nomad answered.
	Error string `json:"error,omitempty"`
}

type apiPlanError struct {
	File      string `json:"file,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Job       string `json:"job,omitempty"`
	Error     string `json:"error"`
}

type apiRemovedJob struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	// Superseded is the deployment waiting for the job that would be superseded.
	Superseded string `json:"superseded,omitempty"`
	// Orphan says the job would be an orphan: deployed by Nops, still running.
	Orphan bool `json:"orphan"`
}

// apiPlan is POST /api/plan: what the next detection cycle would do if a
// repository held the files of the request (docs/cli.md#planning-a-change).
// It writes nothing. A job of a namespace where the token may not plan is an
// error, with nothing about it but its name.
func (s *server) apiPlan(w http.ResponseWriter, r *http.Request) {
	var req apiPlanRequest
	if !readBodyMax(w, r, &req, maxPlanBody) {
		return
	}
	files, err := planFiles(req)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	selected, ok := s.git.Select(req.Repository, files)
	if !ok {
		apiError(w, http.StatusNotFound, fmt.Sprintf("no repository is named %q", req.Repository))
		return
	}
	sub := subjectOf(r.Context())
	allow := func(ns string) bool { return sub.acl.Allow(ns, acl.Plan) }
	plan, err := s.engine.Plan(r.Context(), gitwatch.Snapshot{Repository: req.Repository, Commit: req.Commit, Files: selected}, allow)
	// A plan waits for the one before it and parses and plans in Nomad: the
	// write timeout counts from its answer, not from the request. The client
	// going away ends it.
	if s.writeWait > 0 {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.writeWait)) // without support, the server's deadline stands
	}
	switch {
	case errors.Is(err, engine.ErrUnknownRepository):
		apiError(w, http.StatusNotFound, fmt.Sprintf("no repository is named %q", req.Repository))
		return
	case err != nil:
		s.apiServerError(w, r, "plan a change", err)
		return
	}
	answer, err := planOf(plan, sub.acl)
	if err != nil {
		s.apiServerError(w, r, "plan a change", err)
		return
	}
	s.log.InfoContext(r.Context(), "change planned", "actor", sub.actor, "accessor_id", sub.accessorID,
		"repository", req.Repository, "commit", req.Commit, "files", len(selected), "jobs", len(answer.Jobs), "errors", len(answer.Errors))
	replyJSON(w, http.StatusOK, answer)
}

// planFiles checks the request and indexes its files by path.
func planFiles(req apiPlanRequest) (map[string]string, error) {
	if req.Repository == "" {
		return nil, errors.New("repository is required: the name of a repository block")
	}
	if !commitPattern.MatchString(req.Commit) {
		return nil, errors.New("commit is required: the full SHA of the commit, in lowercase hex")
	}
	files := make(map[string]string, len(req.Files))
	for _, f := range req.Files {
		if f.Path == "" || path.Clean(f.Path) != f.Path || path.IsAbs(f.Path) || f.Path == ".." || strings.HasPrefix(f.Path, "../") {
			return nil, fmt.Errorf("file %q: a path is relative to the repository, with no . or .. in it", f.Path)
		}
		if _, dup := files[f.Path]; dup {
			return nil, fmt.Errorf("file %q is there twice", f.Path)
		}
		files[f.Path] = f.Content
	}
	return files, nil
}

// planOf is a plan as the API answers it to a token with a: what is in a
// namespace it may not plan becomes an error that names only the job. A file
// in a namespace Nops does not manage has none: its error names it.
func planOf(p engine.Plan, a *acl.ACL) (apiPlan, error) {
	out := apiPlan{Jobs: []apiPlannedJob{}, Errors: []apiPlanError{}, Removed: []apiRemovedJob{}, RemovedUnchecked: p.RemovedUnchecked}
	denied := func(file, ns, job string) apiPlanError {
		return apiPlanError{File: file, Namespace: ns, Job: job, Error: fmt.Sprintf("this token may not plan namespace %q", ns)}
	}
	for _, j := range p.Jobs {
		if !a.Allow(j.Namespace, acl.Plan) {
			out.Errors = append(out.Errors, denied(j.FilePath, j.Namespace, j.JobID))
			continue
		}
		var latest *store.Deployment
		if j.Latest != "" {
			latest = &store.Deployment{State: j.Latest}
		}
		pj := apiPlannedJob{apiJob: jobOf(j.Observation, nil), Repository: j.Repository, File: j.FilePath, Issues: issuesOf(j.Issues), Error: planError(j.Observation)}
		pj.Sync = string(jobSync(j.Observation, latest))
		for _, h := range j.PreHooks {
			pj.PreHooks = append(pj.PreHooks, h.JobID)
		}
		for _, h := range j.PostHooks {
			pj.PostHooks = append(pj.PostHooks, h.JobID)
		}
		if j.PlanDiff != "" {
			if !json.Valid([]byte(j.PlanDiff)) {
				return apiPlan{}, errors.New("the plan diff is not JSON")
			}
			pj.Diff = json.RawMessage(j.PlanDiff)
		}
		out.Jobs = append(out.Jobs, pj)
	}
	for _, f := range p.Unplanned {
		out.Errors = append(out.Errors, denied(f.Path, f.Namespace, f.JobID))
	}
	for _, f := range p.Errors {
		if f.Namespace != "" && !a.Allow(f.Namespace, acl.Plan) {
			out.Errors = append(out.Errors, denied(f.Path, f.Namespace, f.JobID))
			continue
		}
		out.Errors = append(out.Errors, apiPlanError{File: f.Path, Namespace: f.Namespace, Job: f.JobID, Error: f.Error})
	}
	for _, j := range p.Removed {
		if !a.Allow(j.Namespace, acl.Plan) {
			out.Errors = append(out.Errors, denied("", j.Namespace, j.JobID))
			continue
		}
		out.Removed = append(out.Removed, apiRemovedJob{Namespace: j.Namespace, Job: j.JobID, Superseded: j.Superseded, Orphan: j.Orphan})
	}
	return out, nil
}

// planError is why a planned job would not deploy, "" when nothing stops it
// but what its state says.
func planError(o engine.Observation) string {
	var invalid []string
	for _, iss := range o.Issues {
		if iss.Severity == meta.SeverityError {
			invalid = append(invalid, iss.Key+": "+iss.Message)
		}
	}
	switch {
	case len(invalid) > 0:
		return "invalid meta: " + strings.Join(invalid, "; ")
	case o.NomadError != "":
		return o.NomadError
	case o.Policy != meta.PolicyNone:
		return o.HookProblem
	}
	return ""
}
