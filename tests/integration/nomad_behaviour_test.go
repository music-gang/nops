//go:build integration

package integration

import (
	"context"
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
	// Verified on Nomad 2.0.3: a scale is a new job version, so it moves the
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
