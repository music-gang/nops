//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// managedServiceHCL is a lightweight raw_exec service with an update block, so
// Nomad tracks the register with a Nomad deployment of its own: the other way
// apply decides health than the allocations of a batch job.
func managedServiceHCL(id string) string {
	return fmt.Sprintf(`
job %q {
  type = "service"
  meta {
    nops_managed = "true"
    nops_policy  = "auto"
  }
  update {
    max_parallel     = 1
    min_healthy_time = "1s"
    healthy_deadline = "30s"
  }
  group "g" {
    count = 1
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}`, id)
}

// TestEngineApplyWaitsForTheNomadDeployment covers the health path that
// TestEngineApplyAgainstRealNomad (a batch job, no Nomad deployment) does not:
// a service with an update block. Apply must match the Nomad deployment by the
// applied index (Deployment.JobModifyIndex == applied_index) and complete once
// it is successful.
func TestEngineApplyWaitsForTheNomadDeployment(t *testing.T) {
	ctx := context.Background()
	c, raw := newClient(t)
	jobID := uniqueID(t, raw, "svc")

	snap := staticSnapshot{changed: make(chan struct{}, 1), snap: gitwatch.Snapshot{
		Commit: "c1", Files: []gitwatch.File{{Path: "job.nomad.hcl", Content: managedServiceHCL(jobID)}},
	}}
	e, st := newEngine(t, snap)
	if err := e.Detect(ctx); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	d, err := st.ActiveDeployment(ctx, "default", jobID)
	if err != nil {
		t.Fatalf("ActiveDeployment: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.RunApply(runCtx)

	deadline := time.Now().Add(60 * time.Second)
	for {
		got, err := st.GetDeployment(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == store.StateCompleted {
			break
		}
		if got.State == store.StateFailed {
			t.Fatalf("deployment failed: %s", got.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not complete in time: %+v", got)
		}
		time.Sleep(200 * time.Millisecond)
	}

	got, _ := st.GetDeployment(ctx, d.ID)
	dep, err := c.LatestDeployment(ctx, "default", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if dep == nil {
		t.Fatal("Nomad created no deployment for a service with an update block")
	}
	if dep.JobModifyIndex != got.AppliedIndex {
		t.Errorf("Nomad deployment tracks index %d, nops recorded applied_index %d: apply would have taken the allocations path",
			dep.JobModifyIndex, got.AppliedIndex)
	}
	if dep.Status != api.DeploymentStatusSuccessful {
		t.Errorf("Nomad deployment status = %q, want successful", dep.Status)
	}
}
