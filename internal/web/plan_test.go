package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

const planSHA = "0123456789abcdef0123456789abcdef01234567"

func planBody(t *testing.T, req apiPlanRequest) string {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readPlan(t *testing.T, body []byte) apiPlan {
	t.Helper()
	var p apiPlan
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return p
}

// The plan is asked of the engine with the repository's files as gitwatch
// selects them, and the commit for nops_commit.
func TestAPIPlanAsksTheEngine(t *testing.T) {
	en := &fakeEngine{plan: engine.Plan{Jobs: []engine.PlannedJob{{
		Observation: engine.Observation{JobID: "web", Namespace: "default", FilePath: "web.nomad.hcl", Policy: meta.PolicyApproval,
			Drift: true, PlanDiff: `{"Type":"Edited"}`, SpecHash: "h1", PreHooks: []engine.HookRef{{JobID: "migrate"}}},
		Latest: store.StatePendingApproval,
	}}}}
	ts := newTestServer(t, &fakeStore{}, en, "")

	rec := ts.api("POST", "/api/plan", apiToken(t, ts, "ci", time.Time{}), planBody(t, apiPlanRequest{
		Repository: "default", Commit: planSHA, Files: []apiPlanFile{{Path: "web.nomad.hcl", Content: "job"}},
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := []gitwatch.Snapshot{{Repository: "default", Commit: planSHA, Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "job"}}}}
	if !reflect.DeepEqual(en.planCalls, want) {
		t.Errorf("Plan was asked %+v, want %+v", en.planCalls, want)
	}
	p := readPlan(t, rec.Body.Bytes())
	if len(p.Jobs) != 1 {
		t.Fatalf("jobs = %+v", p.Jobs)
	}
	j := p.Jobs[0]
	if j.Sync != string(syncPending) || j.File != "web.nomad.hcl" || string(j.Diff) != `{"Type":"Edited"}` ||
		!reflect.DeepEqual(j.PreHooks, []string{"migrate"}) || j.Error != "" || j.SpecHash != "h1" {
		t.Errorf("job = %+v, want web awaiting approval with its diff and hook", j)
	}
}

func TestAPIPlanRefusesABadRequest(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	token := apiToken(t, ts, "ci", time.Time{})
	file := func(p string) []apiPlanFile { return []apiPlanFile{{Path: p}} }
	for name, tc := range map[string]struct {
		req    apiPlanRequest
		status int
		says   string
	}{
		"no repository":      {apiPlanRequest{Commit: planSHA}, http.StatusBadRequest, "repository"},
		"no commit":          {apiPlanRequest{Repository: "default"}, http.StatusBadRequest, "commit"},
		"a short commit":     {apiPlanRequest{Repository: "default", Commit: "0123abc"}, http.StatusBadRequest, "full SHA"},
		"an absolute path":   {apiPlanRequest{Repository: "default", Commit: planSHA, Files: file("/etc/web.nomad")}, http.StatusBadRequest, "relative"},
		"a path out":         {apiPlanRequest{Repository: "default", Commit: planSHA, Files: file("../web.nomad")}, http.StatusBadRequest, "relative"},
		"a path not clean":   {apiPlanRequest{Repository: "default", Commit: planSHA, Files: file("jobs//web.nomad")}, http.StatusBadRequest, "relative"},
		"a file twice":       {apiPlanRequest{Repository: "default", Commit: planSHA, Files: append(file("web.nomad"), file("web.nomad")...)}, http.StatusBadRequest, "twice"},
		"another repository": {apiPlanRequest{Repository: "nope", Commit: planSHA}, http.StatusNotFound, `"nope"`},
	} {
		t.Run(name, func(t *testing.T) {
			rec := ts.api("POST", "/api/plan", token, planBody(t, tc.req))
			if rec.Code != tc.status || !strings.Contains(apiErrorOf(t, rec), tc.says) {
				t.Errorf("status %d %s, want %d saying %s", rec.Code, rec.Body, tc.status, tc.says)
			}
		})
	}
}

// The body takes a repository's files, and no more than maxPlanBody.
func TestAPIPlanBodyLimit(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	token := apiToken(t, ts, "ci", time.Time{})
	big := func(n int) string {
		return planBody(t, apiPlanRequest{Repository: "default", Commit: planSHA, Files: []apiPlanFile{{Path: "web.nomad", Content: strings.Repeat("x", n)}}})
	}
	if rec := ts.api("POST", "/api/plan", token, big(maxAPIBody+1)); rec.Code != http.StatusOK {
		t.Errorf("a body over the limit of the other endpoints: status %d, want 200", rec.Code)
	}
	if rec := ts.api("POST", "/api/plan", token, big(maxPlanBody)); rec.Code != http.StatusBadRequest {
		t.Errorf("a body over maxPlanBody: status %d, want 400", rec.Code)
	}
}

// A token plans only where it may: without plan anywhere it is refused, and
// what is in a namespace it may not plan is an error that names the job and
// nothing more.
func TestAPIPlanFollowsTheACL(t *testing.T) {
	en := &fakeEngine{plan: engine.Plan{
		Jobs: []engine.PlannedJob{
			{Observation: engine.Observation{JobID: "web", Namespace: "ci", FilePath: "web.nomad.hcl", Policy: meta.PolicyAuto}},
			{Observation: engine.Observation{JobID: "db", Namespace: "prod", FilePath: "db.nomad.hcl", Policy: meta.PolicyAuto, Drift: true, PlanDiff: `{"secret":"x"}`}},
		},
		Errors: []engine.FileError{
			{Repository: "default", Path: "dup.nomad.hcl", Namespace: "prod", JobID: "dup", Error: "two files: other:dup.nomad.hcl"},
			{Repository: "default", Path: "typo.nomad.hcl", JobID: "typo", Error: `the job declares namespace "stagng", which Nops does not manage`},
		},
		Unplanned: []engine.FileError{{Repository: "default", Path: "api.nomad.hcl", Namespace: "prod", JobID: "api"}},
		Removed:   []engine.RemovedJob{{Namespace: "prod", JobID: "old", Superseded: "d1", Orphan: true}},
	}}
	s := newACLServer(t, &fakeStore{}, en)
	body := planBody(t, apiPlanRequest{Repository: "default", Commit: planSHA})

	if rec := s.api("POST", "/api/plan", s.clientWith(`namespace "*" { policy = "read" }`), body); rec.Code != http.StatusForbidden {
		t.Errorf("a token that may plan nowhere: status %d, want 403", rec.Code)
	}

	rec := s.api("POST", "/api/plan", s.clientWith(`namespace "ci" { capabilities = ["plan"] }`), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	p := readPlan(t, rec.Body.Bytes())
	if len(p.Jobs) != 1 || p.Jobs[0].Job != "web" || len(p.Removed) != 0 {
		t.Errorf("jobs %+v, removed %+v: want web only", p.Jobs, p.Removed)
	}
	refused := `this token may not plan namespace "prod"`
	want := []apiPlanError{
		{File: "db.nomad.hcl", Namespace: "prod", Job: "db", Error: refused},
		{File: "api.nomad.hcl", Namespace: "prod", Job: "api", Error: refused},
		{File: "dup.nomad.hcl", Namespace: "prod", Job: "dup", Error: refused},
		{File: "typo.nomad.hcl", Job: "typo", Error: `the job declares namespace "stagng", which Nops does not manage`},
		{Namespace: "prod", Job: "old", Error: refused},
	}
	if !reflect.DeepEqual(p.Errors, want) {
		t.Errorf("errors = %+v, want %+v", p.Errors, want)
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "other:") {
		t.Errorf("the answer tells about prod: %s", rec.Body)
	}
	if en.planAllow == nil || !en.planAllow("ci") || en.planAllow("prod") {
		t.Error("the engine was not told to plan ci only")
	}
}

// A plan that waits for another, or takes long in Nomad, still answers: the
// write timeout counts from its answer.
func TestAPIPlanOutlastsTheWriteTimeout(t *testing.T) {
	en := &fakeEngine{planWait: 300 * time.Millisecond}
	ts := newTestServerLogin(t, &fakeStore{}, en, "", nil, "", nil, func(o *Options) { o.WriteTimeout = 100 * time.Millisecond })
	srv := httptest.NewUnstartedServer(ts.h)
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("POST", srv.URL+"/api/plan", strings.NewReader(planBody(t, apiPlanRequest{Repository: "default", Commit: planSHA})))
	req.Header.Set("Authorization", "Bearer "+apiToken(t, ts, "ci", time.Time{}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the plan was cut: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
}

// The error of a job is why it would not deploy, in this order.
func TestPlanError(t *testing.T) {
	invalid := []meta.Issue{{Severity: meta.SeverityError, Key: "nops_policy", Message: "unknown"}, {Severity: meta.SeverityWarn, Key: "x", Message: "w"}}
	for name, tc := range map[string]struct {
		obs  engine.Observation
		want string
	}{
		"nothing":                   {engine.Observation{Policy: meta.PolicyAuto}, ""},
		"invalid meta":              {engine.Observation{Issues: invalid, NomadError: "plan job: x"}, "invalid meta: nops_policy: unknown"},
		"Nomad":                     {engine.Observation{Policy: meta.PolicyAuto, NomadError: "plan job: x", HookProblem: "h"}, "plan job: x"},
		"a missing hook":            {engine.Observation{Policy: meta.PolicyApproval, HookProblem: `pre-hook "m" not found in repo`}, `pre-hook "m" not found in repo`},
		"a missing hook, never run": {engine.Observation{Policy: meta.PolicyNone, HookProblem: `pre-hook "m" not found in repo`}, ""},
	} {
		if got := planError(tc.obs); got != tc.want {
			t.Errorf("%s: planError = %q, want %q", name, got, tc.want)
		}
	}
}
