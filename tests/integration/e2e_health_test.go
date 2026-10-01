//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// TestE2EPromoteFromTheDashboard is the flow of TestE2EApplyWaitsForCanaryPromotion
// with the promotion done where the person sees the wait: the Nomad panel shows
// the Nomad deployment and its canary, the Promote button posts, the request is
// on the timeline with who made it, and the deployment completes and runs its
// post-hook without anyone touching Nomad.
func TestE2EPromoteFromTheDashboard(t *testing.T) {
	const nomadUI = "https://nomad-ui.example.test/nomad" // where a person opens Nomad, not where nops does
	e := newE2E(t, "NOPS_APPLY_TIMEOUT=8s", "NOPS_NOMAD_UI_URL="+nomadUI)
	jobID := uniqueID(t, e.raw, "promotesvc")
	hookID := uniqueID(t, e.raw, "promotehook")
	marker := filepath.Join(t.TempDir(), "post-hook-ran")

	files := func(tag string) map[string]string {
		return map[string]string{
			file(jobID):  managedCanaryHCL(jobID, tag, hookID),
			file(hookID): hookCmdHCL(hookID, "touch "+marker, true),
		}
	}
	e.repo.commit(t, "job "+jobID+" v1", files("v1"))
	d1 := e.waitNew(jobID, "", store.StateCompleted)

	e.repo.commit(t, "job "+jobID+" v2", files("v2"))
	d2 := e.waitNew(jobID, d1.ID, store.StateApplying)

	// The wait, as the dashboard says it: the notice, the button, and Nomad's own
	// account of the deployment it waits on, on the deployment's page and the job's.
	page := "/deployments/" + d2.ID
	e.dash.waitBody(t, page, "Waiting for canary promotion in Nomad.")
	// The panel is up to a few seconds behind Nomad (its cache), so wait for it.
	e.dash.waitBody(t, page, "requires manual promotion")
	_, body := e.dash.get(t, page)
	for _, want := range []string{`action="` + page + `/promote"`, "This is the one this deployment waits on.", "requires manual promotion", "Nomad deployment"} {
		if !strings.Contains(body, want) {
			t.Errorf("the deployment page is missing %q while it waits for a promotion", want)
		}
	}
	e.dash.waitBody(t, "/jobs/default/"+jobID, "requires manual promotion")
	jobInNomad := nomadUI + "/ui/jobs/" + jobID + "@default"
	e.dash.waitBody(t, "/jobs/default/"+jobID, `href="`+jobInNomad+`"`)
	e.dash.waitBody(t, page, `href="`+jobInNomad+`/deployments"`)

	if status, _ := e.dash.post(t, page+"/promote", nil); status != http.StatusOK && status != http.StatusSeeOther {
		t.Fatalf("POST %s/promote answered %d", page, status)
	}
	// The page it returns to no longer shows the wait, from nops or from its
	// cached Nomad panel: not at the next apply cycle, now.
	_, body = e.dash.get(t, page)
	for _, stale := range []string{"Waiting for canary promotion in Nomad.", "requires manual promotion", `action="` + page + `/promote"`} {
		if strings.Contains(body, stale) {
			t.Errorf("the deployment page still shows %q right after the promotion", stale)
		}
	}
	e.waitState(d2.ID, store.StateCompleted)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the post-hook did not run after the promotion: %v", err)
	}

	evs, err := e.st.Events(context.Background(), d2.ID)
	if err != nil {
		t.Fatal(err)
	}
	var requested int
	for _, ev := range evs {
		if ev.Message == "promotion requested" {
			requested++
			if ev.Actor != e2eUser || ev.From != store.StateApplying || ev.To != store.StateApplying {
				t.Errorf("promotion event = %+v, want applying -> applying by %s", ev, e2eUser)
			}
		}
	}
	if requested != 1 {
		t.Errorf("%d promotion requests on the timeline, want 1: %+v", requested, evs)
	}

	// Nothing left to promote: a second click is refused, and asks nothing of Nomad.
	if status, _ := e.dash.post(t, page+"/promote", nil); status != http.StatusConflict {
		t.Errorf("a second promote answered %d, want 409", status)
	}
}
