package engine

// A group with a scaling block keeps its live count: what scales it (an
// autoscaler, a person with `nomad job scale`) owns it. A scale moves the live
// job's modify index but not the plan's diff, since detection plans with the
// live count. These tests make sure Nops never reads a scale as an outside
// edit, and still reads an edit as one.

import (
	"context"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// scaledDiff is what Nomad's plan of git's spec against the live job shows: a
// change to apply, never the count of the scaling group. old is the live
// job's value of the changed key.
func scaledDiff(old string) *api.JobDiff {
	return &api.JobDiff{Type: "Edited", ID: "web", Fields: []*api.FieldDiff{
		{Type: "Edited", Name: "Meta[version]", Old: old, New: "2"},
	}}
}

// scaledJob sets up web with a scaling group: git has version 2, the live job
// version 1 at index 5 with a count of 2. It runs a detection cycle and
// returns the deployment it created.
func scaledJob(t *testing.T, h *harness, policy string) *store.Deployment {
	t.Helper()
	h.nomad.setFile("web-v2", managed("web", policy, map[string]string{"version": "2"}, taskGroup("g", 1, true)))
	h.liveApplied("web", managed("web", policy, map[string]string{"version": "1"}, taskGroup("g", 2, true)), 5, 1)
	h.nomad.setDrift("web", scaledDiff("1"))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"})
	h.detect()
	d := h.active("web")
	if d.CASIndex != 5 {
		t.Fatalf("setup: cas_index = %d, want 5", d.CASIndex)
	}
	return d
}

// scaleTo moves web's live count the way Nomad's scale API does: a new version
// at a new index, and the same diff against git.
func (h *harness) scaleTo(count int, index uint64) {
	h.t.Helper()
	live, err := deepCopyJob(h.nomad.live["web"])
	if err != nil {
		h.t.Fatal(err)
	}
	live.TaskGroups[0].Count = &count
	h.liveApplied("web", live, index, derefUint64(live.Version)+1)
}

// editLive edits web by hand: a new version at a new index, and a diff
// against git that now shows the edit.
func (h *harness) editLive(index uint64) {
	h.t.Helper()
	live, err := deepCopyJob(h.nomad.live["web"])
	if err != nil {
		h.t.Fatal(err)
	}
	live.Meta["version"] = "hand"
	h.liveApplied("web", live, index, derefUint64(live.Version)+1)
	h.nomad.setDrift("web", scaledDiff("hand"))
}

// liveCount is the count of web's scaling group in the live job.
func (h *harness) liveCount() int {
	h.t.Helper()
	return *h.nomad.live["web"].TaskGroups[0].Count
}

func TestAScaleKeepsTheDeploymentWaitingForApproval(t *testing.T) {
	h := newHarness(t)
	d := scaledJob(t, h, "approval")

	h.scaleTo(0, 6)
	h.detect()

	got := h.active("web")
	if got.ID != d.ID {
		t.Fatalf("the scale replaced deployment %s with %s: a count is not an outside edit (old one: %s %q)",
			d.ID, got.ID, h.get(d.ID).State, h.get(d.ID).Error)
	}
	if got.State != store.StatePendingApproval || got.CASIndex != 6 {
		t.Fatalf("deployment = %s at cas_index %d, want pending_approval at the scale's index 6", got.State, got.CASIndex)
	}

	// The approval given after the scale applies at the new index, with the count
	// the scale left.
	if err := h.engine.Approve(context.Background(), got.ID, got.SpecHash, "alice"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	for i := 0; i < 3 && h.get(d.ID).AppliedIndex == 0; i++ {
		h.step(h.get(d.ID))
	}
	if calls := h.nomad.registerCalls; len(calls) != 1 || calls[0].index != 6 {
		t.Fatalf("register calls = %+v, want one at index 6", calls)
	}
	if n := h.liveCount(); n != 0 {
		t.Errorf("live count after the register = %d, want the scale's 0", n)
	}
}

func TestAnOutsideEditStillReplacesTheDeploymentWaitingForApproval(t *testing.T) {
	h := newHarness(t)
	d := scaledJob(t, h, "approval")

	h.editLive(6)
	h.detect()

	if old := h.get(d.ID); old.State != store.StateSuperseded {
		t.Fatalf("deployment after an outside edit = %s, want superseded", old.State)
	}
	if got := h.active("web"); got.ID == d.ID || got.CASIndex != 6 {
		t.Errorf("active deployment = %s at cas_index %d, want a new one at 6", got.ID, got.CASIndex)
	}
}

func TestARejectedSpecStaysBlockedAfterAScale(t *testing.T) {
	h := newHarness(t)
	d := scaledJob(t, h, "approval")
	if err := h.engine.Reject(context.Background(), d.ID, "alice"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	h.detect()
	h.noActive("web")

	h.scaleTo(0, 6)
	h.detect()

	h.noActive("web")
	if obs := h.engine.Observations(); len(obs) != 1 || obs[0].BlockedBy != d.ID {
		t.Fatalf("observations = %+v, want the job blocked by the rejected %s", obs, d.ID)
	}

	// An outside edit is a change: the job is no longer blocked.
	h.editLive(7)
	h.detect()
	if got := h.active("web"); got.State != store.StatePendingApproval {
		t.Errorf("deployment after an outside edit = %s, want a new one pending_approval", got.State)
	}
}

func TestAScaleBeforeTheRegisterIsRegisteredAtTheNewIndex(t *testing.T) {
	h := newHarness(t)
	d := scaledJob(t, h, "auto")
	h.step(d) // detected -> applying

	h.scaleTo(3, 6)
	for i := 0; i < 3 && h.get(d.ID).AppliedIndex == 0 && h.get(d.ID).State == store.StateApplying; i++ {
		h.step(h.get(d.ID))
	}

	if got := h.get(d.ID); got.State != store.StateApplying || got.AppliedIndex == 0 {
		t.Fatalf("deployment = %s, applied at %d (%q), want registered after the scale", got.State, got.AppliedIndex, got.Error)
	}
	if calls := h.nomad.registerCalls; len(calls) != 1 || calls[0].index != 6 {
		t.Fatalf("register calls = %+v, want one at the scale's index 6", calls)
	}
	if n := h.liveCount(); n != 3 {
		t.Errorf("live count after the register = %d, want the scale's 3", n)
	}
}

func TestAnOutsideEditBeforeTheRegisterStillFails(t *testing.T) {
	h := newHarness(t)
	d := scaledJob(t, h, "auto")
	h.step(d) // detected -> applying

	h.editLive(6)
	h.step(h.get(d.ID))

	if got := h.get(d.ID); got.State != store.StateFailed {
		t.Fatalf("deployment after an outside edit = %s, want failed", got.State)
	}
	if len(h.nomad.registerCalls) != 0 {
		t.Errorf("registered %+v over an outside edit", h.nomad.registerCalls)
	}
}
