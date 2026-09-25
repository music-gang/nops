package engine

import (
	"context"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/store"
)

// What the dashboard says about a job (in sync, blocked) comes from the last
// detection cycle, which is from before the apply: a deployment that ends must
// ask for another one.

func (h *harness) kicked() int { return len(h.engine.kick) }

func TestACompletedDeploymentAsksForADetectionCycle(t *testing.T) {
	h := newHarness(t)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	d := h.applyingWithIndex("web", managed("web", "auto", nil), 9)
	h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusSuccessful})
	if h.kicked() != 0 {
		t.Fatalf("setup: %d cycles queued", h.kicked())
	}

	h.step(d)

	if got := h.get(d.ID); got.State != store.StateCompleted {
		t.Fatalf("state = %s, want completed", got.State)
	}
	if h.kicked() != 1 {
		t.Errorf("queued cycles = %d, want 1: the job is in sync now and the page still says drift", h.kicked())
	}
}

func TestAFailedDeploymentAsksForADetectionCycle(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"})
	d := h.newDeployment("web", job, store.PolicyAuto, 0)
	d = h.move(d, store.StatePreHook)
	h.hooks.setResult(d.ID, "pre", hooks.Result{State: store.HookFailed, Error: "exit 1"})

	h.step(d)

	if got := h.get(d.ID); got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if h.kicked() != 1 {
		t.Errorf("queued cycles = %d, want 1: the job is blocked now and the page says drift", h.kicked())
	}
}

func TestAStepThatDoesNotEndTheDeploymentDoesNotAskForACycle(t *testing.T) {
	h := newHarness(t)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	d := h.pendingApproval("web", managed("web", "approval", nil), "h1")

	if err := h.engine.Approve(context.Background(), d.ID, "h1", "alice"); err != nil {
		t.Fatal(err)
	}
	h.step(h.get(d.ID)) // applying: the register
	if got := h.get(d.ID); got.State != store.StateApplying || got.AppliedIndex == 0 {
		t.Fatalf("setup: %+v, want registered and waiting for health", got)
	}

	if h.kicked() != 0 {
		t.Errorf("queued cycles = %d, want none while the deployment is still going", h.kicked())
	}

	// Nor does moving on to the next step.
	auto := newHarness(t)
	moving := auto.newDeployment("web", managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"}), store.PolicyAuto, 0)
	auto.step(moving)              // detected -> pre_hook
	auto.step(auto.get(moving.ID)) // pre_hook -> applying
	if got := auto.get(moving.ID); got.State != store.StateApplying || auto.kicked() != 0 {
		t.Errorf("state %s, queued cycles %d; want applying and none", got.State, auto.kicked())
	}
}

func TestARejectedDeploymentAsksForADetectionCycle(t *testing.T) {
	h := newHarness(t)
	d := h.pendingApproval("web", managed("web", "approval", nil), "h1")

	if err := h.engine.Reject(context.Background(), d.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if h.kicked() != 1 {
		t.Errorf("queued cycles = %d, want 1: the job is blocked now", h.kicked())
	}

	// A refused rejection asks for nothing.
	h2 := newHarness(t)
	d2 := h2.pendingApproval("web", managed("web", "approval", nil), "h1")
	if err := h2.engine.Approve(context.Background(), d2.ID, "h1", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := h2.engine.Reject(context.Background(), d2.ID, "alice"); err == nil {
		t.Fatal("rejecting a deployment that is not pending succeeded")
	}
	if h2.kicked() != 0 {
		t.Errorf("a refused Reject asked for a cycle")
	}
}

func TestTwoEndedDeploymentsQueueOneCycle(t *testing.T) {
	h := newHarness(t)
	h.engine.kickDetection()
	h.engine.kickDetection()
	if h.kicked() != 1 {
		t.Errorf("queued cycles = %d, want 1: a cycle already queued is enough", h.kicked())
	}
}

func TestDetectionsOwnTransitionsDoNotAskForACycle(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	first := h.active("web")

	// A newer spec supersedes it, and then the drift goes away: two moves made
	// by detection itself, one of them a "completed", none of which may queue
	// another cycle behind the one that is running.
	h.nomad.setFile("web-v2", managed("web", "approval", map[string]string{"note": "2"}))
	h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"})
	h.detect()
	if got := h.get(first.ID); got.State != store.StateSuperseded {
		t.Fatalf("setup: state = %s", got.State)
	}
	h.nomad.setDrift("web", &api.JobDiff{Type: "None", ID: "web"})
	h.detect()
	if got := h.latest("web"); got.State != store.StateCompleted {
		t.Fatalf("setup: state = %s, want completed (already in sync)", got.State)
	}

	if h.kicked() != 0 {
		t.Errorf("queued cycles = %d, want none: a cycle would keep asking for the next", h.kicked())
	}
}
