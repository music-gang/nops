package nomadx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
)

// stub is a fake Nomad HTTP server that records the last request.
type stub struct {
	srv    *httptest.Server
	method string
	path   string
	query  map[string][]string
	body   []byte
}

// newStub answers every request with the given status and body.
func newStub(t *testing.T, status int, response string) (*stub, *Client) {
	t.Helper()
	s := &stub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.method, s.path, s.query = r.Method, r.URL.Path, r.URL.Query()
		s.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		io.WriteString(w, response)
	}))
	t.Cleanup(s.srv.Close)

	cfg := api.DefaultConfig()
	cfg.Address = s.srv.URL
	c, err := New(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func testJob(id string) *api.Job {
	return &api.Job{ID: &id, Name: &id}
}

func TestNewDefaultsNamespace(t *testing.T) {
	_, c := newStub(t, 200, `{}`)
	if c.Namespace() != "default" {
		t.Errorf("Namespace() = %q, want default", c.Namespace())
	}
	cfg := api.DefaultConfig()
	cfg.Address = "http://127.0.0.1:1"
	c2, err := New(cfg, "prod")
	if err != nil || c2.Namespace() != "prod" {
		t.Errorf("New with namespace: %v, %v", c2, err)
	}
}

// Invariant 2: a register always carries the enforce-index flag and the index.
func TestRegisterCASAlwaysEnforcesIndex(t *testing.T) {
	for _, idx := range []uint64{0, 42} {
		s, c := newStub(t, 200, `{"EvalID":"e1","JobModifyIndex":7}`)
		res, err := c.RegisterCAS(context.Background(), testJob("web"), idx, true)
		if err != nil {
			t.Fatalf("index %d: %v", idx, err)
		}
		if res.EvalID != "e1" || res.JobModifyIndex != 7 {
			t.Errorf("result = %+v", res)
		}
		var req struct {
			EnforceIndex   bool
			JobModifyIndex uint64
			PreserveCounts bool
		}
		if err := json.Unmarshal(s.body, &req); err != nil {
			t.Fatal(err)
		}
		if !req.EnforceIndex || req.JobModifyIndex != idx || !req.PreserveCounts {
			t.Errorf("index %d: request = %+v (body %s)", idx, req, s.body)
		}
		if s.method != http.MethodPut && s.method != http.MethodPost {
			t.Errorf("method = %s", s.method)
		}
	}
}

func TestRegisterCASConflict(t *testing.T) {
	// The three messages Nomad 2.0.3 returns, all as HTTP 500.
	for _, msg := range []string{
		"Enforcing job modify index 0: job already exists",
		"Enforcing job modify index 999999: job exists with conflicting job modify index: 11",
		"Enforcing job modify index 5: job does not exist",
	} {
		_, c := newStub(t, 500, msg)
		_, err := c.RegisterCAS(context.Background(), testJob("web"), 1, false)
		if !errors.Is(err, ErrCASConflict) {
			t.Errorf("%q: err = %v, want ErrCASConflict", msg, err)
		}
		if err != nil && !strings.Contains(err.Error(), msg) {
			t.Errorf("original message lost: %v", err)
		}
	}

	// Any other failure is not a conflict.
	_, c := newStub(t, 500, "rpc error: no leader")
	_, err := c.RegisterCAS(context.Background(), testJob("web"), 1, false)
	if err == nil || errors.Is(err, ErrCASConflict) {
		t.Errorf("non-CAS failure: err = %v", err)
	}
}

func TestJob(t *testing.T) {
	s, c := newStub(t, 200, `{"ID":"web","JobModifyIndex":11}`)
	job, err := c.Job(context.Background(), "web")
	if err != nil || job.JobModifyIndex == nil || *job.JobModifyIndex != 11 {
		t.Fatalf("Job = %+v, %v", job, err)
	}
	if s.path != "/v1/job/web" || s.query["namespace"][0] != "default" {
		t.Errorf("request = %s %v", s.path, s.query)
	}

	_, c = newStub(t, 404, "job not found")
	if _, err := c.Job(context.Background(), "web"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("404: err = %v, want ErrJobNotFound", err)
	}

	_, c = newStub(t, 500, "boom")
	if _, err := c.Job(context.Background(), "web"); err == nil || errors.Is(err, ErrJobNotFound) {
		t.Errorf("500: err = %v", err)
	}
}

func TestPlanRequestsDiff(t *testing.T) {
	s, c := newStub(t, 200, `{"Diff":{"Type":"Edited","ID":"web"},"JobModifyIndex":11}`)
	resp, err := c.Plan(context.Background(), testJob("web"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Diff == nil || resp.Diff.Type != "Edited" {
		t.Errorf("Diff = %+v", resp.Diff)
	}
	var req struct{ Diff bool }
	if err := json.Unmarshal(s.body, &req); err != nil || !req.Diff {
		t.Errorf("plan request must ask for the diff: %s (%v)", s.body, err)
	}

	_, c = newStub(t, 500, "boom")
	if _, err := c.Plan(context.Background(), testJob("web")); err == nil || !strings.Contains(err.Error(), "plan job web") {
		t.Errorf("err = %v", err)
	}
}

func TestDispatchSendsIdempotencyToken(t *testing.T) {
	s, c := newStub(t, 200, `{"DispatchedJobID":"hook/dispatch-1","EvalID":"e2"}`)
	res, err := c.Dispatch(context.Background(), "hook", map[string]string{"nops_deployment_id": "d1"}, "d1:pre")
	if err != nil {
		t.Fatal(err)
	}
	if res.JobID != "hook/dispatch-1" || res.EvalID != "e2" {
		t.Errorf("result = %+v", res)
	}
	if s.path != "/v1/job/hook/dispatch" {
		t.Errorf("path = %s", s.path)
	}
	if got := s.query["idempotency_token"]; len(got) != 1 || got[0] != "d1:pre" {
		t.Errorf("idempotency_token = %v", got)
	}
	var req struct{ Meta map[string]string }
	if err := json.Unmarshal(s.body, &req); err != nil || req.Meta["nops_deployment_id"] != "d1" {
		t.Errorf("meta not sent: %s (%v)", s.body, err)
	}

	// No token: the parameter must be absent, not empty.
	s, c = newStub(t, 200, `{"DispatchedJobID":"hook/dispatch-2"}`)
	if _, err := c.Dispatch(context.Background(), "hook", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.query["idempotency_token"]; ok {
		t.Errorf("empty token sent: %v", s.query)
	}

	_, c = newStub(t, 500, "Dispatch request included unpermitted metadata keys: [x]")
	if _, err := c.Dispatch(context.Background(), "hook", nil, ""); err == nil || !strings.Contains(err.Error(), "unpermitted metadata keys") {
		t.Errorf("err = %v", err)
	}
}

func TestAllocations(t *testing.T) {
	const body = `[
	  {"ID":"a1","JobVersion":3,"ClientStatus":"complete","DesiredStatus":"run"},
	  {"ID":"a2","ClientStatus":"failed","DesiredStatus":"run","ClientDescription":"Failed tasks",
	   "TaskStates":{
	     "z":{"Failed":false,"Events":[{"Type":"Terminated","DisplayMessage":"ignored"}]},
	     "t":{"Failed":true,"Events":[{"Type":"Started","DisplayMessage":"Task started"},
	                                  {"Type":"Terminated","DisplayMessage":"Exit Code: 1"},
	                                  {"Type":"Not Restarting","DisplayMessage":"Policy allows no restarts"}]},
	     "b":{"Failed":true,"Events":[{"Type":"Driver Failure"}]},
	     "n":{"Failed":true,"Events":[{"Type":"Received","DisplayMessage":"Task received by client"},
	                                  {"Type":"Killed","DisplayMessage":"Task successfully killed"}]}}},
	  {"ID":"a3","ClientStatus":"lost","DesiredStatus":"stop","ClientDescription":"Client lost"}
	]`
	s, c := newStub(t, 200, body)
	got, err := c.Allocations(context.Background(), "hook/dispatch-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.path != "/v1/job/hook/dispatch-1/allocations" || s.query["all"][0] != "true" || s.query["namespace"][0] != "default" {
		t.Errorf("request = %s %v", s.path, s.query)
	}
	want := []Alloc{
		{ID: "a1", JobVersion: 3, ClientStatus: "complete", DesiredStatus: "run"},
		{ID: "a2", ClientStatus: "failed", DesiredStatus: "run", Failure: "Failed tasks; task b: Driver Failure; task n: Task successfully killed; task t: Exit Code: 1"},
		{ID: "a3", ClientStatus: "lost", DesiredStatus: "stop", Failure: "Client lost"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d allocs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("alloc %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	_, c = newStub(t, 404, "job not found")
	if _, err := c.Allocations(context.Background(), "x"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("404: err = %v, want ErrJobNotFound", err)
	}
	_, c = newStub(t, 500, "boom")
	if _, err := c.Allocations(context.Background(), "x"); err == nil || errors.Is(err, ErrJobNotFound) {
		t.Errorf("500: err = %v", err)
	}
}

func TestLatestDeployment(t *testing.T) {
	body := `{"ID":"dep-1","JobModifyIndex":8,"Status":"running","StatusDescription":"Deployment is running"}`
	s, c := newStub(t, 200, body)
	got, err := c.LatestDeployment(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if s.path != "/v1/job/web/deployment" {
		t.Errorf("path = %s", s.path)
	}
	if got == nil || got.ID != "dep-1" || got.JobModifyIndex != 8 || got.Status != "running" {
		t.Errorf("LatestDeployment = %+v", got)
	}

	// A job with no deployment (a batch job, or an update stanza that
	// produces none) answers with an empty body, not a 404.
	_, c = newStub(t, 200, `{}`)
	got, err = c.LatestDeployment(context.Background(), "batch-job")
	if err != nil || got != nil {
		t.Errorf("LatestDeployment = %+v, %v, want nil, nil", got, err)
	}

	_, c = newStub(t, 404, "job not found")
	if _, err := c.LatestDeployment(context.Background(), "x"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("404: err = %v, want ErrJobNotFound", err)
	}
	_, c = newStub(t, 500, "boom")
	if _, err := c.LatestDeployment(context.Background(), "x"); err == nil || errors.Is(err, ErrJobNotFound) {
		t.Errorf("500: err = %v", err)
	}
}

func TestFindDispatched(t *testing.T) {
	var listPrefix string
	var read []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/jobs":
			listPrefix = r.URL.Query().Get("prefix")
			io.WriteString(w, `[{"ID":"hook/dispatch-1","ParentID":"hook"},
			                    {"ID":"hook/dispatch-2","ParentID":"hook"},
			                    {"ID":"hook/dispatch-3","ParentID":"hook"},
			                    {"ID":"hook/dispatch-4","ParentID":"hook"},
			                    {"ID":"hook/dispatch-9","ParentID":"other"}]`)
		case "/v1/job/hook/dispatch-1":
			read = append(read, "1")
			io.WriteString(w, `{"ID":"hook/dispatch-1","DispatchIdempotencyToken":"d1:pre"}`)
		case "/v1/job/hook/dispatch-2":
			read = append(read, "2")
			w.WriteHeader(404) // garbage-collected between the list and the read
		case "/v1/job/hook/dispatch-3":
			read = append(read, "3")
			io.WriteString(w, `{"ID":"hook/dispatch-3"}`) // not dispatched with a token
		case "/v1/job/hook/dispatch-4":
			read = append(read, "4")
			io.WriteString(w, `{"ID":"hook/dispatch-4","DispatchIdempotencyToken":"d2:pre"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := api.DefaultConfig()
	cfg.Address = srv.URL
	c, err := New(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := c.FindDispatched(ctx, "hook", "d2:pre")
	if err != nil || got != "hook/dispatch-4" {
		t.Fatalf("FindDispatched = %q, %v, want hook/dispatch-4", got, err)
	}
	if listPrefix != "hook/dispatch-" {
		t.Errorf("list prefix = %q", listPrefix)
	}
	if strings.Join(read, "") != "1234" {
		t.Errorf("children read = %v: the child of another parent must not be read", read)
	}

	if got, err := c.FindDispatched(ctx, "hook", "nobody:pre"); err != nil || got != "" {
		t.Errorf("unknown token: %q, %v, want empty", got, err)
	}

	_, c = newStub(t, 500, "boom")
	if _, err := c.FindDispatched(ctx, "hook", "x"); err == nil || !strings.Contains(err.Error(), "list children of job hook") {
		t.Errorf("list failure: err = %v", err)
	}
}

func TestStopJob(t *testing.T) {
	s, c := newStub(t, 200, `{"EvalID":"e1"}`)
	if err := c.StopJob(context.Background(), "hook/dispatch-1"); err != nil {
		t.Fatal(err)
	}
	if s.method != http.MethodDelete || s.path != "/v1/job/hook/dispatch-1" {
		t.Errorf("request = %s %s", s.method, s.path)
	}
	if got := s.query["purge"]; len(got) != 1 || got[0] != "false" {
		t.Errorf("purge = %v, want false", got)
	}

	// A job that is already gone is fine.
	_, c = newStub(t, 404, "job not found")
	if err := c.StopJob(context.Background(), "x"); err != nil {
		t.Errorf("404: err = %v, want nil", err)
	}
	_, c = newStub(t, 500, "boom")
	if err := c.StopJob(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "stop job x") {
		t.Errorf("500: err = %v", err)
	}
}

func TestListJobs(t *testing.T) {
	s, c := newStub(t, 200, `[
		{"ID":"z-hook-0a1b2c3d","ParentID":"","Status":"running","Stop":false,"Meta":{"nops_role":"hook"}},
		{"ID":"a-hook-0a1b2c3d/dispatch-1-2dbd0404","ParentID":"a-hook-0a1b2c3d","Status":"dead","Meta":{"nops_role":"hook"}},
		{"ID":"a-hook-0a1b2c3d","Status":"dead","Stop":true,"Meta":{"nops_role":"hook"}},
		{"ID":"plain","Status":"running"}]`)

	got, err := c.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.method != http.MethodGet || s.path != "/v1/jobs" {
		t.Errorf("request = %s %s", s.method, s.path)
	}
	// Nomad leaves the meta out of a listing unless asked.
	if v := s.query["meta"]; len(v) != 1 || v[0] != "true" {
		t.Errorf("meta = %v, want true", v)
	}
	var ids []string
	for _, j := range got {
		ids = append(ids, j.ID)
	}
	if want := "a-hook-0a1b2c3d a-hook-0a1b2c3d/dispatch-1-2dbd0404 plain z-hook-0a1b2c3d"; strings.Join(ids, " ") != want {
		t.Fatalf("ids = %v, want them ordered by ID", ids)
	}
	if !got[0].Stop || got[0].Meta["nops_role"] != "hook" || got[1].ParentID != "a-hook-0a1b2c3d" || got[2].Meta != nil || got[3].Stop {
		t.Errorf("listing = %+v", got)
	}

	_, c = newStub(t, 500, "boom")
	if _, err := c.ListJobs(context.Background()); err == nil || !strings.Contains(err.Error(), "list jobs") {
		t.Errorf("500: err = %v", err)
	}
}

func TestParseHCL(t *testing.T) {
	s, c := newStub(t, 200, `{"ID":"web"}`)
	job, err := c.ParseHCL(context.Background(), `job "web" {}`, `tag = "1"`)
	if err != nil || job.ID == nil || *job.ID != "web" {
		t.Fatalf("ParseHCL = %+v, %v", job, err)
	}
	var req api.JobsParseRequest
	if err := json.Unmarshal(s.body, &req); err != nil {
		t.Fatal(err)
	}
	if req.JobHCL != `job "web" {}` || req.Variables != `tag = "1"` || !req.Canonicalize {
		t.Errorf("request = %+v", req)
	}

	_, c = newStub(t, 400, `Unset variable "image_tag"`)
	if _, err := c.ParseHCL(context.Background(), "x", ""); err == nil || !strings.Contains(err.Error(), "parse job") {
		t.Errorf("err = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, c = newStub(t, 200, `{}`)
	if _, err := c.ParseHCL(ctx, "x", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: err = %v", err)
	}
	if s.method != "" {
		t.Errorf("a cancelled context must not reach Nomad, got %s %s", s.method, s.path)
	}
}

func TestIsCASConflict(t *testing.T) {
	if isCASConflict(nil) {
		t.Error("nil is not a conflict")
	}
	if isCASConflict(errors.New("job not found")) {
		t.Error("unrelated error flagged as conflict")
	}
	if !isCASConflict(errors.New("Unexpected response code: 500 (Enforcing job modify index 3: job already exists)")) {
		t.Error("wrapped Nomad message not recognised")
	}
}

func TestDeref(t *testing.T) {
	s := "x"
	if deref(&s) != "x" || deref(nil) != "<nil>" {
		t.Error("deref")
	}
}
