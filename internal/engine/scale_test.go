package engine

// A group with a scaling block keeps its live count: what scales it (an
// autoscaler, a person with `nomad job scale`) owns it. A scale moves the live
// job's modify index but not the plan's diff, since detection plans with the
// live count. These tests make sure Nops never reads a scale as an outside
// edit, and still reads an edit as one.

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// scaledDiff is what Nomad's plan of git's spec against the live job shows: a
// change to apply, never the count of the scaling group. old is the live
// job's value of the changed key. Like Nomad's, the group carries the
// scheduler's count of the allocations the plan touches: the live count.
func scaledDiff(old string, count int) *api.JobDiff {
	return &api.JobDiff{Type: "Edited", ID: "web",
		Fields: []*api.FieldDiff{{Type: "Edited", Name: "Meta[version]", Old: old, New: "2"}},
		TaskGroups: []*api.TaskGroupDiff{{Type: "None", Name: "g",
			Updates: map[string]uint64{"create/destroy update": uint64(count)}}},
	}
}

// scaledJob sets up web with a scaling group: git has version 2, the live job
// version 1 at index 5 with a count of 2. It runs a detection cycle and
// returns the deployment it created.
func scaledJob(t *testing.T, h *harness, policy string) *store.Deployment {
	t.Helper()
	h.nomad.setFile("web-v2", managed("web", policy, map[string]string{"version": "2"}, taskGroup("g", 1, true)))
	h.liveApplied("web", managed("web", policy, map[string]string{"version": "1"}, taskGroup("g", 2, true)), 5, 1)
	h.nomad.setDrift("web", scaledDiff("1", 2))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"})
	h.detect()
	d := h.active("web")
	if d.CASIndex != 5 {
		t.Fatalf("setup: cas_index = %d, want 5", d.CASIndex)
	}
	return d
}

// scaleTo moves web's live count the way Nomad's scale API does: a new version
// at a new index, and the same diff against git but for the scheduler's count
// of allocations.
func (h *harness) scaleTo(count int, index uint64) {
	h.t.Helper()
	live, err := deepCopyJob(h.nomad.live["web"])
	if err != nil {
		h.t.Fatal(err)
	}
	live.TaskGroups[0].Count = &count
	h.liveApplied("web", live, index, derefUint64(live.Version)+1)
	h.nomad.setDrift("web", scaledDiff(live.Meta["version"], count))
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
	h.nomad.setDrift("web", scaledDiff("hand", h.liveCount()))
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

// registered drives web's deployment d, under auto, through the register and
// returns it applying at the index of the register. No Nomad deployment
// tracks the job (a batch job, say), so its health is that of its
// allocations, and the allocation of the registered version is still
// starting. The plan of git's spec now shows nothing to apply.
func registered(t *testing.T, h *harness, d *store.Deployment) *store.Deployment {
	t.Helper()
	h.step(d) // detected -> applying
	h.step(h.get(d.ID))
	got := h.get(d.ID)
	if got.State != store.StateApplying || got.AppliedIndex == 0 {
		t.Fatalf("setup: deployment = %s, applied at %d (%q), want registered", got.State, got.AppliedIndex, got.Error)
	}
	h.nomad.setDrift("web", &api.JobDiff{Type: "None", ID: "web"})
	h.nomad.setAllocs("web", nomadx.Alloc{ID: "a1", JobVersion: h.liveVersion(), ClientStatus: "pending"})
	return got
}

// scaleAppliedTo moves web's live count the way Nomad's scale API does, after
// Nops registered git's spec: a new version at a new index, and still nothing
// to apply.
func (h *harness) scaleAppliedTo(count int, index uint64) {
	h.t.Helper()
	live, err := deepCopyJob(h.nomad.live["web"])
	if err != nil {
		h.t.Fatal(err)
	}
	live.TaskGroups[0].Count = &count
	h.liveApplied("web", live, index, derefUint64(live.Version)+1)
}

// liveVersion is the version of web's live job.
func (h *harness) liveVersion() uint64 {
	h.t.Helper()
	return derefUint64(h.nomad.live["web"].Version)
}

func TestAScaleWhileWaitingForHealthKeepsTheDeployment(t *testing.T) {
	h := newHarness(t)
	d := registered(t, h, scaledJob(t, h, "auto"))
	ours := h.liveVersion()

	// Like Nomad's, the scale moves the allocations still running to its
	// version, and leaves those that ended at theirs.
	h.scaleAppliedTo(3, d.AppliedIndex+1)
	scaled := h.liveVersion()
	h.nomad.setAllocs("web",
		nomadx.Alloc{ID: "a0", JobVersion: ours - 1, ClientStatus: "failed", Failure: "before the register"},
		nomadx.Alloc{ID: "a1", JobVersion: ours, ClientStatus: "complete"},
		nomadx.Alloc{ID: "a2", JobVersion: scaled, ClientStatus: "pending"},
	)
	h.step(h.get(d.ID))

	got := h.get(d.ID)
	if got.State != store.StateApplying {
		t.Fatalf("deployment after a scale = %s (%q), want still applying: a count is not an outside edit", got.State, got.Error)
	}
	if got.AppliedIndex != d.AppliedIndex+1 || *got.AppliedVersion != ours {
		t.Errorf("applied at %d, version %d; want the scale's index %d and the registered version %d",
			got.AppliedIndex, *got.AppliedVersion, d.AppliedIndex+1, ours)
	}

	h.nomad.setAllocs("web",
		nomadx.Alloc{ID: "a0", JobVersion: ours - 1, ClientStatus: "failed", Failure: "before the register"},
		nomadx.Alloc{ID: "a1", JobVersion: ours, ClientStatus: "complete"},
		nomadx.Alloc{ID: "a2", JobVersion: scaled, ClientStatus: "running"},
	)
	h.step(h.get(d.ID))
	if got := h.get(d.ID); got.State != store.StateCompleted {
		t.Fatalf("deployment with every allocation running or complete = %s (%q), want completed", got.State, got.Error)
	}
}

func TestAnAllocationOfTheRegisteredVersionStillFailsTheDeploymentAfterAScale(t *testing.T) {
	h := newHarness(t)
	d := registered(t, h, scaledJob(t, h, "auto"))
	ours := h.liveVersion()

	h.scaleAppliedTo(3, d.AppliedIndex+1)
	h.nomad.setAllocs("web",
		nomadx.Alloc{ID: "a1", JobVersion: ours, ClientStatus: "failed", Failure: "oom"},
		nomadx.Alloc{ID: "a2", JobVersion: h.liveVersion(), ClientStatus: "running"},
	)
	h.step(h.get(d.ID))

	if got := h.get(d.ID); got.State != store.StateFailed || !strings.Contains(got.Error, "oom") {
		t.Fatalf("deployment = %s (%q), want failed on the allocation of the registered version", got.State, got.Error)
	}
}

func TestAnOutsideEditWhileWaitingForHealthStillFails(t *testing.T) {
	h := newHarness(t)
	d := registered(t, h, scaledJob(t, h, "auto"))

	h.editLive(d.AppliedIndex + 1)
	h.step(h.get(d.ID))

	if got := h.get(d.ID); got.State != store.StateFailed {
		t.Fatalf("deployment after an outside edit = %s, want failed", got.State)
	}
}

// A deployment registered before the version was recorded can't tell the
// allocations of a scale: any move of the index fails it, as it did then.
func TestAScaleWhileWaitingForHealthFailsADeploymentWithoutItsVersion(t *testing.T) {
	h := newHarness(t)
	d := registered(t, h, scaledJob(t, h, "auto"))

	h.scaleAppliedTo(3, d.AppliedIndex+1)
	d.AppliedVersion = nil
	h.step(d)

	if got := h.get(d.ID); got.State != store.StateFailed {
		t.Fatalf("deployment = %s, want failed", got.State)
	}
}
