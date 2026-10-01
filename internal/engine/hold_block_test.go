package engine

import (
	"context"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// A job whose drift a failed deployment blocks stays blocked while it is held:
// the hold gates the start, it does not hide the block. The dashboard ranks
// Blocked above Held and lists the block, with its Retry, in Needs attention;
// both read Observation.BlockedBy. A Retry while the job is held lifts the
// block and still starts nothing.
func TestAHoldDoesNotHideABlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind HoldKind
		// hold sets up the hold on a job "web" whose failed deployment blocks it.
		hold func(t *testing.T, h *harness) *store.Deployment
	}{
		{"a closed sync window", HoldWindow, func(t *testing.T, h *harness) *store.Deployment {
			return failedWeb(t, h, managed("web", "auto", windowed()))
		}},
		{"a pause", HoldPaused, func(t *testing.T, h *harness) *store.Deployment {
			failed := failedWeb(t, h, managed("web", "auto", nil))
			h.detect() // the job must be observed to be paused
			if err := h.engine.Pause(context.Background(), testNamespace, "web", "alice", ""); err != nil {
				t.Fatal(err)
			}
			return failed
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			failed := tc.hold(t, h)

			h.detect() // at the start of the harness clock: outside the window

			obs := h.observation()
			if obs.Hold == nil || obs.Hold.Kind != tc.kind {
				t.Fatalf("hold = %+v, want %s", obs.Hold, tc.kind)
			}
			if obs.BlockedBy != failed.ID {
				t.Errorf("BlockedBy = %q, want %q: the failed deployment still blocks the job", obs.BlockedBy, failed.ID)
			}
			if _, err := h.retryOf(failed.ID); err != nil {
				t.Fatalf("Retry of a blocked job that is held: %v, want the block lifted", err)
			}
			h.detect()
			h.noActive("web")
			if obs := h.observation(); obs.BlockedBy != "" || obs.Hold == nil {
				t.Errorf("after the retry: BlockedBy = %q, hold = %+v; want unblocked and still held", obs.BlockedBy, obs.Hold)
			}
		})
	}
}

// failedWeb makes j, as job "web" with drift, the head of the repository, with
// a failed deployment of that same spec: what blocks the job.
func failedWeb(t *testing.T, h *harness, j *api.Job) *store.Deployment {
	t.Helper()
	ctx := context.Background()
	h.nomad.setFile("web-v1", j)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	hash, err := specHash(j)
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: hash,
		JobSpec: `{"ID":"web"}`, Policy: store.PolicyAuto,
	}
	if err := h.store.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(ctx, d.ID, store.StateFailed,
		store.Transition{From: store.StateDetected, Actor: "nops", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	return d
}
