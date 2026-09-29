//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// scalableHCL is a service job whose only group has a scaling policy, the
// shape an autoscaler owns. It has no rolling update (max_parallel = 0), so
// Nomad creates no deployment for it: while one is active Nomad refuses to
// scale the job ("job scaling blocked due to active deployment"), which would
// make the test depend on timing. The task is never waited for: the tests only
// read what Nomad records about the job.
func scalableHCL(id string) string {
	return `
job "` + id + `" {
  type = "service"
  update {
    max_parallel = 0
  }
  group "g" {
    count = 1
    scaling {
      min     = 1
      max     = 10
      enabled = true
    }
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}`
}

// TestScaleChangesTheLiveJobsIndex settles what docs/design/engine-detection.md
// says an autoscaler never does to nops: does moving a count through Nomad's
// scale API change the live job's JobModifyIndex (what nops keeps as cas_index)
// and its Version? Detection supersedes a pending deployment when the index
// moves (reconcileDeployment), so the answer decides whether "an autoscaler
// never supersedes a pending approval" can hold.
func TestScaleChangesTheLiveJobsIndex(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := uniqueID(t, raw, "scale")
	job, err := c.ParseHCL(ctx, scalableHCL(id), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, job, 0, false); err != nil {
		t.Fatal(err)
	}
	before, err := c.Job(ctx, "default", id)
	if err != nil {
		t.Fatal(err)
	}

	count := 3
	if _, _, err := raw.Jobs().Scale(id, "g", &count, "moved by the test", false, nil, nil); err != nil {
		t.Fatalf("scale: %v", err)
	}
	after, err := c.Job(ctx, "default", id)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("before: count=%d JobModifyIndex=%d ModifyIndex=%d Version=%d",
		*before.TaskGroups[0].Count, *before.JobModifyIndex, *before.ModifyIndex, *before.Version)
	t.Logf("after:  count=%d JobModifyIndex=%d ModifyIndex=%d Version=%d",
		*after.TaskGroups[0].Count, *after.JobModifyIndex, *after.ModifyIndex, *after.Version)

	if got := *after.TaskGroups[0].Count; got != count {
		t.Fatalf("count after the scale = %d, want %d", got, count)
	}
	// Nomad treats a scale as a new job version, so it moves the
	// index nops compares (cas_index) whether or not the spec changed.
	if *after.JobModifyIndex <= *before.JobModifyIndex {
		t.Errorf("JobModifyIndex %d -> %d: a scale is expected to move it", *before.JobModifyIndex, *after.JobModifyIndex)
	}
	if *after.Version <= *before.Version {
		t.Errorf("Version %d -> %d: a scale is expected to bump it", *before.Version, *after.Version)
	}
}

// TestMetaBlockAndObjectFormsCannotBeMixed checks what docs/meta-keys.md says
// about a job that also needs meta keys with dots: HCL does not allow the
// block form and the object form for the same attribute, so the whole meta
// has to be written as an object.
func TestMetaBlockAndObjectFormsCannotBeMixed(t *testing.T) {
	c, _ := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const body = `
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/true"
      }
    }
  }
}`
	mixed := `job "mixed" {
  meta {
    nops_managed = "true"
  }
  meta = {
    "diun.enable" = "true"
  }` + body
	if _, err := c.ParseHCL(ctx, mixed, ""); err == nil {
		t.Error("a job with meta as a block and as an object parsed: docs/meta-keys.md says it cannot")
	}

	object := `job "object" {
  meta = {
    "nops_managed" = "true"
    "diun.enable"  = "true"
  }` + body
	job, err := c.ParseHCL(ctx, object, "")
	if err != nil {
		t.Fatalf("meta as one object: %v", err)
	}
	if job.Meta["nops_managed"] != "true" || job.Meta["diun.enable"] != "true" {
		t.Errorf("meta = %v, want both keys", job.Meta)
	}
}

// TestStoppingAHookRevisionLeavesItsRunningRun checks what
// docs/design/engine-detection.md relies on when it garbage collects a hook
// revision: deregistering the parameterized parent without purge does not take
// a run it dispatched away, even one still running, so a revision can be
// stopped while its run is being read.
func TestStoppingAHookRevisionLeavesItsRunningRun(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id := uniqueID(t, raw, "gcrun")

	parent, err := c.ParseHCL(ctx, fmt.Sprintf(`
job %q {
  type = "batch"
  parameterized {}
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}`, id), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, parent, 0, false); err != nil {
		t.Fatal(err)
	}
	run, err := c.Dispatch(ctx, "default", id, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for {
		allocs, _, err := raw.Jobs().Allocations(run.JobID, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(allocs) > 0 && allocs[0].ClientStatus == "running" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the run %s never started", run.JobID)
		case <-time.After(200 * time.Millisecond):
		}
	}

	if err := c.StopJob(ctx, "default", id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Job(ctx, "default", run.JobID); err != nil {
		t.Errorf("the run %s cannot be read once its parent was stopped: %v", run.JobID, err)
	}
	allocs, _, err := raw.Jobs().Allocations(run.JobID, true, nil)
	if err != nil || len(allocs) == 0 {
		t.Errorf("allocations of the run = %d, %v; want them readable", len(allocs), err)
	}
}

// TestLongJobIDIsAccepted checks the claim of docs/design/engine-detection.md
// that the "-<8 hex>" suffix of a hook revision hits no limit on the length of
// a job ID: an ID of several hundred characters registers and reads back.
func TestLongJobIDIsAccepted(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := uniqueID(t, raw, "long")
	id += strings.Repeat("x", 400-len(id)) + "-0a1b2c3d"

	job, err := c.ParseHCL(ctx, batchHCL(id, "one"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, job, 0, false); err != nil {
		t.Fatalf("register with an ID of %d characters: %v", len(id), err)
	}
	live, err := c.Job(ctx, "default", id)
	if err != nil || live.ID == nil || *live.ID != id {
		t.Fatalf("live = %+v, %v; want the job back under its long ID", live, err)
	}
}
