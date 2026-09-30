//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// managedCanaryHCL is a managed service under policy auto with one canary and
// no auto-promotion, and a post-hook: its Nomad deployment waits for a human
// after the canary is healthy. tag is what a commit changes between versions.
func managedCanaryHCL(id, tag, postHook string) string {
	return fmt.Sprintf(`
job %q {
  type = "service"
  meta {
    nops_managed   = "true"
    nops_policy    = "auto"
    nops_post_hook = %q
  }
  update {
    max_parallel      = 1
    canary            = 1
    auto_promote      = false
    min_healthy_time  = "1s"
    healthy_deadline  = "20s"
    progress_deadline = "30s"
  }
  group "g" {
    count = 1
    task "t" {
      driver = "raw_exec"
      env {
        TAG = %q
      }
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}`, id, postHook, tag)
}

// TestE2EApplyWaitsForCanaryPromotion runs a job whose Nomad deployment needs a
// manual promotion, with an apply timeout shorter than the wait: the deployment
// must stay applying (the wait is a human's, not a failure to become healthy),
// the dashboard must say so, and once the canary is promoted in Nomad it
// completes and runs its post-hook.
func TestE2EApplyWaitsForCanaryPromotion(t *testing.T) {
	const applyTimeout = 8 * time.Second
	e := newE2E(t, "NOPS_APPLY_TIMEOUT="+applyTimeout.String())
	jobID := uniqueID(t, e.raw, "canarysvc")
	hookID := uniqueID(t, e.raw, "canaryhook")
	marker := filepath.Join(t.TempDir(), "post-hook-ran")

	files := func(tag string) map[string]string {
		return map[string]string{
			file(jobID):  managedCanaryHCL(jobID, tag, hookID),
			file(hookID): hookCmdHCL(hookID, "touch "+marker, true),
		}
	}
	// A first version has nothing to be a canary of: it completes by itself.
	e.repo.commit(t, "job "+jobID+" v1", files("v1"))
	d1 := e.waitNew(jobID, "", store.StateCompleted)

	e.repo.commit(t, "job "+jobID+" v2", files("v2"))
	var d2 *store.Deployment
	deadline := time.Now().Add(e2eWait)
	for d2 == nil && time.Now().Before(deadline) {
		if l := e.latest(jobID); l != nil && l.ID != d1.ID {
			d2 = l
		}
		time.Sleep(100 * time.Millisecond)
	}
	if d2 == nil {
		t.Fatal("no deployment for the second version")
	}

	// Well past the apply timeout, with the canary healthy and not promoted.
	time.Sleep(applyTimeout + 3*time.Second)
	if got := e.deployment(d2.ID); got.State != store.StateApplying {
		t.Fatalf("deployment is %s (%s) after the apply timeout, want applying: a canary waiting for promotion is not a failure",
			got.State, got.Error)
	}
	e.dash.waitBody(t, "/deployments/"+d2.ID, "Waiting for canary promotion")

	dep, err := e.nomad.LatestDeployment(context.Background(), "default", jobID)
	if err != nil || dep == nil {
		t.Fatalf("Nomad deployment: %v, %v", dep, err)
	}
	if _, _, err := e.raw.Deployments().PromoteAll(dep.ID, nil); err != nil {
		t.Fatalf("promote: %v", err)
	}

	got := e.waitState(d2.ID, store.StateCompleted)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the post-hook did not run after the promotion: %v", err)
	}
	// promoted_at is only recorded if nops looks while the rollout that follows
	// the promotion is still running, which for one allocation is milliseconds.
	if got.PromotionWaitSince.IsZero() {
		t.Error("promotion_wait_since is not recorded for a deployment that waited")
	}
}
