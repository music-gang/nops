//go:build integration

// Package integration holds tests that run against a real Nomad, typically
// `nomad agent -dev`. Set NOPS_TEST_NOMAD_ADDR to enable them; otherwise they
// are skipped.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/nomadx"
)

func testAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("NOPS_TEST_NOMAD_ADDR")
	if addr == "" {
		t.Skip("NOPS_TEST_NOMAD_ADDR not set: skipping integration test")
	}
	return addr
}

// newClient returns a nomadx client and a raw API client for cleanup.
func newClient(t *testing.T) (*nomadx.Client, *api.Client) {
	t.Helper()
	cfg := api.DefaultConfig()
	cfg.Address = testAddr(t)
	// A client of its own, so its idle connections can be closed at the end.
	cfg.HttpClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	// Every client keeps its idle connections until the process exits, and
	// Nomad answers 429 past 100 connections from one address (with the nops
	// processes of the end-to-end tests alongside, a long run or -count=N gets
	// there), so close them when the test is done.
	t.Cleanup(cfg.HttpClient.CloseIdleConnections)
	c, err := nomadx.New(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, raw
}

// uniqueID returns a job ID that cannot collide with other runs, and purges
// the job (and its dispatched children) when the test ends.
func uniqueID(t *testing.T, raw *api.Client, prefix string) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("nops-it-%s-%s", prefix, hex.EncodeToString(b))
	t.Cleanup(func() {
		stubs, _, _ := raw.Jobs().PrefixList(id)
		for _, s := range stubs {
			raw.Jobs().Deregister(s.ID, true, nil)
		}
	})
	return id
}

// batchHCL is a lightweight raw_exec job. tag goes in the job meta, so two
// specs with different tags produce a plan diff.
func batchHCL(id, tag string) string {
	return fmt.Sprintf(`
job %q {
  type = "batch"
  meta {
    tag = %q
  }
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "exit 0"]
      }
    }
  }
}`, id, tag)
}

func hookHCL(id string) string {
	return fmt.Sprintf(`
job %q {
  type = "batch"
  meta {
    nops_role = "hook"
  }
  parameterized {
    meta_required = ["nops_deployment_id"]
    meta_optional = ["nops_phase"]
  }
  group "g" {
    restart {
      attempts = 0
      mode     = "fail"
    }
    reschedule {
      attempts  = 0
      unlimited = false
    }
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "exit 0"]
      }
    }
  }
}`, id)
}

func TestParseHCL(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()

	const withVar = `
variable "image_tag" {
  type = string
}
job "x" {
  meta {
    tag = var.image_tag
  }
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "true"
      }
    }
  }
}`
	job, err := c.ParseHCL(ctx, withVar, `image_tag = "2.3.4"`)
	if err != nil {
		t.Fatalf("with var-file: %v", err)
	}
	if job.Meta["tag"] != "2.3.4" {
		t.Errorf("meta tag = %q, want 2.3.4", job.Meta["tag"])
	}
	if job.Namespace == nil || *job.Namespace == "" {
		t.Errorf("job not canonicalized: namespace = %v", job.Namespace)
	}

	if _, err := c.ParseHCL(ctx, withVar, ""); err == nil || !strings.Contains(err.Error(), "Unset variable") {
		t.Errorf("without a value: err = %v, want Unset variable", err)
	}

	custom := "job \"x\" {\n  nops {\n    policy = \"approval\"\n  }\n}"
	if _, err := c.ParseHCL(ctx, custom, ""); err == nil || !strings.Contains(err.Error(), "Unsupported block type") {
		t.Errorf("custom block: err = %v, want Unsupported block type", err)
	}
}

func TestJobNotFound(t *testing.T) {
	c, _ := newClient(t)
	if _, err := c.Job(context.Background(), "nops-it-does-not-exist"); !errors.Is(err, nomadx.ErrJobNotFound) {
		t.Errorf("err = %v, want ErrJobNotFound", err)
	}
}

// The full plan → CAS register cycle that the engine relies on.
func TestPlanAndRegisterCAS(t *testing.T) {
	c, raw := newClient(t)
	ctx := context.Background()
	id := uniqueID(t, raw, "cas")

	parse := func(tag string) *api.Job {
		t.Helper()
		j, err := c.ParseHCL(ctx, batchHCL(id, tag), "")
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	liveIndex := func() uint64 {
		t.Helper()
		j, err := c.Job(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return *j.JobModifyIndex
	}
	diffType := func(j *api.Job) string {
		t.Helper()
		p, err := c.Plan(ctx, j)
		if err != nil {
			t.Fatal(err)
		}
		return p.Diff.Type
	}

	v1, v2 := parse("one"), parse("two")

	// A job that does not exist yet: the plan says Added.
	if got := diffType(v1); got != "Added" {
		t.Errorf("plan of a new job: diff type = %q, want Added", got)
	}

	// Index 0 creates it. Registering with index 0 again is a conflict.
	if _, err := c.RegisterCAS(ctx, v1, 0, false); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.RegisterCAS(ctx, v1, 0, false); !errors.Is(err, nomadx.ErrCASConflict) {
		t.Errorf("second create with index 0: err = %v, want ErrCASConflict", err)
	}
	idx1 := liveIndex()
	if idx1 == 0 {
		t.Fatal("live JobModifyIndex is 0")
	}

	// Same spec: the plan is empty ("None").
	if got := diffType(v1); got != "None" {
		t.Errorf("plan of an unchanged spec: diff type = %q, want None", got)
	}
	// A different spec: Edited.
	if got := diffType(v2); got != "Edited" {
		t.Errorf("plan of a changed spec: diff type = %q, want Edited", got)
	}

	// A wrong index is refused and the job stays untouched.
	if _, err := c.RegisterCAS(ctx, v2, idx1+1000, false); !errors.Is(err, nomadx.ErrCASConflict) {
		t.Errorf("wrong index: err = %v, want ErrCASConflict", err)
	}
	if got := liveIndex(); got != idx1 {
		t.Errorf("a refused register changed the job: index %d -> %d", idx1, got)
	}

	// The right index applies the change.
	res, err := c.RegisterCAS(ctx, v2, idx1, false)
	if err != nil {
		t.Fatalf("register at the right index: %v", err)
	}
	idx2 := liveIndex()
	if idx2 == idx1 {
		t.Errorf("a real change did not move the job modify index (%d)", idx2)
	}
	t.Logf("register response JobModifyIndex=%d, live JobModifyIndex=%d", res.JobModifyIndex, idx2)

	// The spec we approved earlier (index idx1) is now stale.
	if _, err := c.RegisterCAS(ctx, v1, idx1, false); !errors.Is(err, nomadx.ErrCASConflict) {
		t.Errorf("stale index: err = %v, want ErrCASConflict", err)
	}
	if got := diffType(v2); got != "None" {
		t.Errorf("after applying v2 the plan should be empty, got %q", got)
	}
}

// Registering a spec identical to the live one passes the CAS check but does
// not move the live job's modify index. The register response, on the other
// hand, has been seen to report a different number (over the raw HTTP API), so
// the engine must re-read the live job instead of trusting
// RegisterResult.JobModifyIndex. Only the live index is asserted here; the
// response index is logged.
func TestRegisterIdenticalSpecKeepsLiveIndex(t *testing.T) {
	c, raw := newClient(t)
	ctx := context.Background()
	id := uniqueID(t, raw, "same")

	job, err := c.ParseHCL(ctx, batchHCL(id, "one"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, job, 0, false); err != nil {
		t.Fatal(err)
	}
	live, err := c.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	before := *live.JobModifyIndex

	res, err := c.RegisterCAS(ctx, job, before, false)
	if err != nil {
		t.Fatalf("identical register at the live index: %v", err)
	}
	live, err = c.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if *live.JobModifyIndex != before {
		t.Errorf("identical register moved the live index: %d -> %d", before, *live.JobModifyIndex)
	}
	t.Logf("live index %d, register response index %d", before, res.JobModifyIndex)
}

func TestDispatch(t *testing.T) {
	c, raw := newClient(t)
	ctx := context.Background()
	id := uniqueID(t, raw, "hook")

	hook, err := c.ParseHCL(ctx, hookHCL(id), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, hook, 0, false); err != nil {
		t.Fatal(err)
	}

	meta := map[string]string{"nops_deployment_id": "d1", "nops_phase": "pre"}

	first, err := c.Dispatch(ctx, id, meta, "d1:pre")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.HasPrefix(first.JobID, id+"/dispatch-") {
		t.Errorf("child ID = %q", first.JobID)
	}

	// Same token: same child, no second dispatch.
	again, err := c.Dispatch(ctx, id, meta, "d1:pre")
	if err != nil || again.JobID != first.JobID {
		t.Errorf("same token: %+v, %v (want child %s)", again, err, first.JobID)
	}
	// Different token: a new child.
	other, err := c.Dispatch(ctx, id, meta, "d2:pre")
	if err != nil || other.JobID == first.JobID {
		t.Errorf("different token: %+v, %v", other, err)
	}

	// The token still deduplicates once the child has finished, which is what
	// makes re-dispatching after a nops crash safe.
	deadline := time.Now().Add(30 * time.Second)
	for {
		child, err := c.Job(ctx, first.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if child.Status != nil && *child.Status == "dead" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %s did not finish in time (status %v)", first.JobID, child.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	afterDone, err := c.Dispatch(ctx, id, meta, "d1:pre")
	if err != nil || afterDone.JobID != first.JobID {
		t.Errorf("token after the child finished: %+v, %v (want child %s)", afterDone, err, first.JobID)
	}

	// Meta the hook does not declare, or a missing required one, is refused.
	if _, err := c.Dispatch(ctx, id, map[string]string{"nops_deployment_id": "d3", "nops_undeclared": "x"}, "d3:pre"); err == nil ||
		!strings.Contains(err.Error(), "unpermitted metadata keys") {
		t.Errorf("undeclared meta: err = %v", err)
	}
	if _, err := c.Dispatch(ctx, id, nil, "d4:pre"); err == nil || !strings.Contains(err.Error(), "required meta keys") {
		t.Errorf("missing required meta: err = %v", err)
	}

	if _, err := c.Dispatch(ctx, "nops-it-no-such-job", meta, "x"); err == nil {
		t.Error("dispatch of a missing job succeeded")
	}
}

// TestListJobsCarriesMetaAndStop is what the hook revision GC relies on: the
// listing carries each job's meta (Nomad leaves it out unless asked, and gives
// a dispatched child its parent's), the parent of a child, and whether a job
// was stopped.
func TestListJobsCarriesMetaAndStop(t *testing.T) {
	c, raw := newClient(t)
	ctx := context.Background()
	id := uniqueID(t, raw, "list")

	hook, err := c.ParseHCL(ctx, hookHCL(id), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, hook, 0, false); err != nil {
		t.Fatal(err)
	}
	child, err := c.Dispatch(ctx, id, map[string]string{"nops_deployment_id": "d1"}, "d1:pre")
	if err != nil {
		t.Fatal(err)
	}

	find := func() map[string]nomadx.JobStub {
		stubs, err := c.ListJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]nomadx.JobStub{}
		for i, s := range stubs {
			out[s.ID] = s
			if i > 0 && stubs[i-1].ID > s.ID {
				t.Errorf("listing not ordered by ID: %s before %s", stubs[i-1].ID, s.ID)
			}
		}
		return out
	}

	got := find()
	parent, kid := got[id], got[child.JobID]
	if parent.Meta["nops_role"] != "hook" || parent.ParentID != "" || parent.Stop {
		t.Errorf("parent = %+v, want the hook's meta, no parent and not stopped", parent)
	}
	if kid.ParentID != id || kid.Meta["nops_role"] != "hook" {
		t.Errorf("child = %+v, want its parent and, like Nomad does, the parent's meta", kid)
	}

	// Stopped without a purge: still listed, and its dispatched run untouched.
	if err := c.StopJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if after := find(); !after[id].Stop || after[child.JobID].ParentID != id {
		t.Errorf("after StopJob: parent %+v, child %+v; want the parent listed as stopped and its run still there", after[id], after[child.JobID])
	}
	if _, err := c.Job(ctx, child.JobID); err != nil {
		t.Errorf("the dispatched run is gone after its parent was stopped: %v", err)
	}
}

// TestHookRevisionRegistration checks against Nomad the behaviour
// registerRevision counts on: a job registers under an ID other than its own
// name, a stopped job shows as a plan difference and is registered again at
// the index it has (a stopped job still exists, so index 0 is a conflict),
// and a second identical plan is a no-op.
func TestHookRevisionRegistration(t *testing.T) {
	c, raw := newClient(t)
	ctx := context.Background()
	hookID := uniqueID(t, raw, "rev")
	revision := hookID + "-0a1b2c3d"

	hook, err := c.ParseHCL(ctx, hookHCL(hookID), "")
	if err != nil {
		t.Fatal(err)
	}
	job := *hook
	job.ID = &revision // Name stays the hook's own

	plan, err := c.Plan(ctx, &job)
	if err != nil || plan.Diff == nil || plan.Diff.Type != "Added" {
		t.Fatalf("plan of an unregistered revision = %+v, %v; want Added", plan, err)
	}
	if _, err := c.RegisterCAS(ctx, &job, 0, false); err != nil {
		t.Fatalf("register under another ID than the name: %v", err)
	}
	live, err := c.Job(ctx, revision)
	if err != nil || live.Name == nil || *live.Name != hookID {
		t.Fatalf("live = %+v, %v; want the revision ID with the hook's name %q", live, err, hookID)
	}
	if plan, err := c.Plan(ctx, &job); err != nil || plan.Diff == nil || plan.Diff.Type != "None" {
		t.Errorf("plan of what is registered = %+v, %v; want None", plan, err)
	}

	if err := c.StopJob(ctx, revision); err != nil {
		t.Fatal(err)
	}
	if plan, err := c.Plan(ctx, &job); err != nil || plan.Diff == nil || plan.Diff.Type == "None" {
		t.Fatalf("plan of a stopped revision = %+v, %v; want a difference (it is not running)", plan, err)
	}
	if _, err := c.RegisterCAS(ctx, &job, 0, false); !errors.Is(err, nomadx.ErrCASConflict) {
		t.Errorf("register of a stopped revision at index 0: %v; want a CAS conflict, the job exists", err)
	}
	stopped, err := c.Job(ctx, revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, &job, *stopped.JobModifyIndex, false); err != nil {
		t.Fatalf("register of a stopped revision at its own index: %v", err)
	}
	if again, err := c.Job(ctx, revision); err != nil || again.Stop != nil && *again.Stop {
		t.Errorf("revision after registering again = %+v, %v; want it running", again, err)
	}
}
