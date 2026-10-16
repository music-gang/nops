//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/store"
)

// scaledE2EHCL is a managed service job under approval whose group has a
// scaling block. Like scalableHCL it has no rolling update, so Nomad creates
// no deployment that would refuse a scale.
func scaledE2EHCL(id, version string) string {
	return fmt.Sprintf(`
job %q {
  type = "service"
  update {
    max_parallel = 0
  }
  meta {
    nops_managed = "true"
    nops_policy  = "approval"
    version      = %q
  }
  group "g" {
    count = 1
    scaling {
      min     = 1
      max     = 3
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
}
`, id, version)
}

// scale moves the count of jobID's group the way an autoscaler does.
func (e *e2eEnv) scale(jobID string, count int) {
	e.t.Helper()
	if _, _, err := e.raw.Jobs().Scale(jobID, "g", &count, "moved by the test", false, nil, nil); err != nil {
		e.t.Fatalf("scale %s to %d: %v", jobID, count, err)
	}
}

// liveCount is the count of jobID's group in Nomad.
func (e *e2eEnv) liveCount(jobID string) int {
	e.t.Helper()
	job, _, err := e.raw.Jobs().Info(jobID, nil)
	if err != nil {
		e.t.Fatalf("job %s: %v", jobID, err)
	}
	return *job.TaskGroups[0].Count
}

// TestE2EAScaleIsNotAnOutsideEdit scales a group with a scaling block through
// Nomad's API while a deployment waits for approval, and after one was
// rejected. Neither changes: a scale moves the live job's index, but the plan
// shows the same diff.
func TestE2EAScaleIsNotAnOutsideEdit(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "scaled")
	push := func(version string) {
		e.repo.commit(t, "job "+jobID+" v"+version, map[string]string{file(jobID): scaledE2EHCL(jobID, version)})
	}
	approve := func(d *store.Deployment) {
		if status := e.dash.approve(t, d.ID, d.SpecHash); status != http.StatusSeeOther {
			t.Fatalf("approve %s: status %d, want 303", d.ID, status)
		}
		e.waitState(d.ID, store.StateCompleted)
	}

	push("1")
	d1 := e.waitNew(jobID, "", store.StatePendingApproval)
	approve(d1)

	// 1. A scale while a deployment waits for approval: the same deployment
	// takes the new index, and the approval applies the change with the count
	// the scale left.
	push("2")
	d2 := e.waitNew(jobID, d1.ID, store.StatePendingApproval)
	e.scale(jobID, 2)
	index := e.liveIndex(jobID)
	deadline := time.Now().Add(e2eWait)
	for {
		got := e.deployment(d2.ID)
		if got.State != store.StatePendingApproval {
			t.Fatalf("after a scale: %s, want it still pending_approval", describe(got))
		}
		if got.CASIndex == index {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cas_index = %d, want the scale's %d within %s", got.CASIndex, index, e2eWait)
		}
		time.Sleep(100 * time.Millisecond)
	}
	approve(d2)
	if got := e.liveVersion(jobID); got != "2" {
		t.Errorf("live version = %q, want 2", got)
	}
	if got := e.liveCount(jobID); got != 2 {
		t.Errorf("live count = %d, want the scale's 2", got)
	}

	// 2. A scale after a rejection: the rejected spec is not proposed again.
	push("3")
	d3 := e.waitNew(jobID, d2.ID, store.StatePendingApproval)
	if status := e.dash.reject(t, d3.ID); status != http.StatusSeeOther {
		t.Fatalf("reject: status %d, want 303", status)
	}
	e.waitState(d3.ID, store.StateRejected)
	e.scale(jobID, 1)
	time.Sleep(2 * time.Second) // ten detection cycles
	if got := e.latest(jobID); got.ID != d3.ID {
		t.Errorf("after a scale, latest deployment = %s, want the rejected %s", describe(got), d3.ID)
	}
}

// scaledBatchHCL is a managed batch job under auto whose group has a scaling
// block, so Nomad creates no deployment to track it. Its group is placed only
// on a node with the meta key gate set to "open": until then its allocations
// are not placed, and nops waits for health.
func scaledBatchHCL(id, gate string) string {
	return fmt.Sprintf(`
job %q {
  type = "batch"
  meta {
    nops_managed = "true"
    nops_policy  = "auto"
  }
  group "g" {
    count = 1
    constraint {
      attribute = "${meta.%s}"
      value     = "open"
    }
    scaling {
      min     = 1
      max     = 3
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
}
`, id, gate)
}

// setNodeMeta sets the meta key on every node of the cluster, removed when the
// test ends.
func (e *e2eEnv) setNodeMeta(key, value string) {
	e.t.Helper()
	nodes, _, err := e.raw.Nodes().List(nil)
	if err != nil {
		e.t.Fatalf("list nodes: %v", err)
	}
	for _, n := range nodes {
		if _, err := e.raw.Nodes().Meta().Apply(&api.NodeMetaApplyRequest{NodeID: n.ID, Meta: map[string]*string{key: &value}}, nil); err != nil {
			e.t.Fatalf("set meta %s on node %s: %v", key, n.ID, err)
		}
		id := n.ID
		e.t.Cleanup(func() {
			_, _ = e.raw.Nodes().Meta().Apply(&api.NodeMetaApplyRequest{NodeID: id, Meta: map[string]*string{key: nil}}, nil)
		})
	}
}

// TestE2EAScaleWhileWaitingForHealthIsNotAnOutsideEdit scales a batch job
// while nops waits for it to be healthy: the deployment keeps waiting, and
// completes once the allocations of the version nops registered and of the
// scale run.
func TestE2EAScaleWhileWaitingForHealthIsNotAnOutsideEdit(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "scaledbatch")
	gate := strings.ReplaceAll(jobID, "-", "_")
	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): scaledBatchHCL(jobID, gate)})

	d := e.waitNew(jobID, "", store.StateApplying)
	deadline := time.Now().Add(e2eWait)
	for d.AppliedIndex == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not registered within %s: %s", e2eWait, describe(d))
		}
		time.Sleep(100 * time.Millisecond)
		d = e.deployment(d.ID)
	}

	e.scale(jobID, 2)
	time.Sleep(2 * time.Second) // ten apply cycles
	if got := e.deployment(d.ID); got.State != store.StateApplying {
		t.Fatalf("after a scale: %s, want it still applying", describe(got))
	}

	e.setNodeMeta(gate, "open")
	e.waitState(d.ID, store.StateCompleted)
	if got := e.liveCount(jobID); got != 2 {
		t.Errorf("live count = %d, want the scale's 2", got)
	}
}
