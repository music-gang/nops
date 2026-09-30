package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// observedWeb is a job "web" under the given policy that one detection cycle
// has read, in sync: what a person pauses. A later h.drift() makes it drift.
func observedWeb(t *testing.T, h *harness, policy string) {
	t.Helper()
	h.nomad.setFile("web-v1", managed("web", policy, nil))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	if obs := h.engine.Observations(); len(obs) != 1 {
		t.Fatalf("setup: observations = %+v, want web", obs)
	}
}

func (h *harness) drift() {
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
}

func (h *harness) observation() Observation {
	h.t.Helper()
	obs := h.engine.Observations()
	if len(obs) != 1 {
		h.t.Fatalf("observations = %+v, want one", obs)
	}
	return obs[0]
}

func TestPausedJobGetsNoDeployment(t *testing.T) {
	for _, policy := range []string{"auto", "approval"} {
		t.Run(policy, func(t *testing.T) {
			h := newHarness(t)
			observedWeb(t, h, policy)
			if err := h.engine.Pause(context.Background(), testNamespace, "web", "alice", "db incident"); err != nil {
				t.Fatal(err)
			}

			h.drift()
			h.detect()

			h.noActive("web")
			obs := h.observation()
			if !obs.Drift || obs.PlanDiff == "" {
				t.Errorf("observation = %+v, want the drift still detected and shown", obs)
			}
			if obs.Hold == nil || obs.Hold.Kind != HoldPaused || obs.Hold.By != "alice" || obs.Hold.Note != "db incident" ||
				obs.Hold.Reason != "paused by alice: db incident" || obs.Hold.Since.IsZero() {
				t.Errorf("hold = %+v, want a pause by alice for a db incident", obs.Hold)
			}
		})
	}
}

// A pause that nobody remembers must still show, drift or not.
func TestPausedJobInSyncStillShowsTheHold(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	if err := h.engine.Pause(context.Background(), testNamespace, "web", "alice", ""); err != nil {
		t.Fatal(err)
	}

	h.detect()

	obs := h.observation()
	if obs.Drift || obs.Hold == nil || obs.Hold.Reason != "paused by alice" {
		t.Errorf("observation = %+v, want no drift and a hold reading %q", obs, "paused by alice")
	}
}

func TestResumeLetsTheNextCycleDeployAsUsual(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   store.State
	}{
		{"auto", store.StateDetected},
		{"approval", store.StatePendingApproval},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			h := newHarness(t)
			observedWeb(t, h, tc.policy)
			ctx := context.Background()
			if err := h.engine.Pause(ctx, testNamespace, "web", "alice", ""); err != nil {
				t.Fatal(err)
			}
			h.drift()
			h.detect()
			h.noActive("web")

			if err := h.engine.Resume(ctx, testNamespace, "web", "bob"); err != nil {
				t.Fatal(err)
			}
			h.detect()

			if d := h.active("web"); d.State != tc.want {
				t.Errorf("deployment after the resume = %s, want %s", d.State, tc.want)
			}
			if obs := h.observation(); obs.Hold != nil {
				t.Errorf("hold after the resume = %+v, want none", obs.Hold)
			}
		})
	}
}

// A deployment detected before the pause is put aside, not applied: when the job
// is resumed detection plans again, so what is applied is never a plan from
// before the pause.
func TestPauseSupersedesADetectedDeployment(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	h.drift()
	h.detect()
	before := h.active("web")
	if before.State != store.StateDetected {
		t.Fatalf("setup: state = %s, want detected", before.State)
	}
	ctx := context.Background()
	if err := h.engine.Pause(ctx, testNamespace, "web", "alice", "db incident"); err != nil {
		t.Fatal(err)
	}

	h.detect()

	h.noActive("web")
	got := h.get(before.ID)
	if got.State != store.StateSuperseded {
		t.Fatalf("state = %s, want superseded", got.State)
	}
	evs, err := h.store.Events(ctx, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := evs[len(evs)-1]; last.Message != "paused by alice: db incident" {
		t.Errorf("last event = %+v, want the pause as the reason", last)
	}

	if err := h.engine.Resume(ctx, testNamespace, "web", "alice"); err != nil {
		t.Fatal(err)
	}
	h.detect()
	if after := h.active("web"); after.ID == before.ID || after.State != store.StateDetected {
		t.Errorf("deployment after the resume = %+v, want a new detected one", after)
	}
}

// Under approval the pause is a brake on the whole job: Approve refuses, and
// the deployment waits, pending, for the resume.
func TestPauseLeavesAPendingApprovalPending(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "approval")
	h.drift()
	h.detect()
	pending := h.active("web")
	ctx := context.Background()
	if err := h.engine.Pause(ctx, testNamespace, "web", "alice", ""); err != nil {
		t.Fatal(err)
	}

	h.detect()

	if got := h.active("web"); got.ID != pending.ID || got.State != store.StatePendingApproval {
		t.Fatalf("deployment = %+v, want the same pending_approval one", got)
	}
	err := h.engine.Approve(ctx, pending.ID, pending.SpecHash, "bob")
	if !errors.Is(err, ErrPaused) {
		t.Fatalf("Approve: err = %v, want ErrPaused", err)
	}
	if !strings.Contains(err.Error(), "paused by alice") {
		t.Errorf("Approve error = %q, want it to say who paused the job", err)
	}
	if got := h.get(pending.ID); got.State != store.StatePendingApproval || got.DecidedBy != "" {
		t.Errorf("deployment after the refused Approve = %+v, want pending and undecided", got)
	}

	if err := h.engine.Resume(ctx, testNamespace, "web", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Approve(ctx, pending.ID, pending.SpecHash, "bob"); err != nil {
		t.Fatalf("Approve after the resume: %v", err)
	}
	if got := h.get(pending.ID); got.State != store.StateApplying || got.DecidedBy != "bob" {
		t.Errorf("deployment = %+v, want applying, decided by bob", got)
	}
}

// Rejecting is not starting anything, so a pause does not stand in its way.
func TestPauseDoesNotStopAReject(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "approval")
	h.drift()
	h.detect()
	pending := h.active("web")
	if err := h.engine.Pause(context.Background(), testNamespace, "web", "alice", ""); err != nil {
		t.Fatal(err)
	}

	if err := h.engine.Reject(context.Background(), pending.ID, "bob"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
}

// What is already running finishes: the pause stops the next one.
func TestPauseLeavesADeploymentInFlightAlone(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	h.drift()
	h.detect()
	d := h.active("web")
	h.step(d) // detected -> applying
	d = h.get(d.ID)
	if d.State != store.StateApplying {
		t.Fatalf("setup: state = %s, want applying", d.State)
	}
	ctx := context.Background()
	if err := h.engine.Pause(ctx, testNamespace, "web", "alice", ""); err != nil {
		t.Fatal(err)
	}

	h.detect()
	if got := h.active("web"); got.ID != d.ID || got.State != store.StateApplying {
		t.Fatalf("deployment after a cycle of a paused job = %+v, want the same applying one", got)
	}
	h.step(d)

	if got := h.get(d.ID); got.AppliedIndex == 0 {
		t.Errorf("deployment = %+v, want it registered: a pause does not stop what already started", got)
	}
	if n := len(h.nomad.registerCalls); n != 1 {
		t.Errorf("registers = %d, want 1", n)
	}
}

// Apply and detection are two loops: a deployment still detected when the pause
// lands is not started by apply before detection has put it aside.
func TestApplyDoesNotStartADetectedDeploymentOfAPausedJob(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	h.drift()
	h.detect()
	d := h.active("web")
	ctx := context.Background()
	if err := h.store.PauseJob(ctx, testNamespace, "web", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if h.kicked() != 0 {
		t.Fatalf("setup: %d cycles queued", h.kicked())
	}

	h.step(d)

	if got := h.get(d.ID); got.State != store.StateDetected {
		t.Fatalf("state = %s, want detected: apply must not start a deployment of a paused job", got.State)
	}
	if h.kicked() != 1 {
		t.Errorf("queued cycles = %d, want 1: detection puts the deployment aside", h.kicked())
	}

	if err := h.store.ResumeJob(ctx, testNamespace, "web", "alice"); err != nil {
		t.Fatal(err)
	}
	h.step(d)
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Errorf("state after the resume = %s, want applying", got.State)
	}
}

func TestPauseAndResumeAskForADetectionCycle(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	ctx := context.Background()
	for len(h.engine.kick) > 0 {
		<-h.engine.kick
	}

	if err := h.engine.Pause(ctx, testNamespace, "web", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if h.kicked() != 1 {
		t.Fatalf("queued cycles after Pause = %d, want 1: the job shows as paused from the next cycle", h.kicked())
	}
	<-h.engine.kick
	if err := h.engine.Resume(ctx, testNamespace, "web", "alice"); err != nil {
		t.Fatal(err)
	}
	if h.kicked() != 1 {
		t.Errorf("queued cycles after Resume = %d, want 1", h.kicked())
	}
}

// The page a person is sent back to after Pause or Resume reads the last
// observation: it must already say so, without waiting for the next cycle.
func TestPauseAndResumeShowInTheObservationAtOnce(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	ctx := context.Background()

	if err := h.engine.Pause(ctx, testNamespace, "web", "alice", "db incident"); err != nil {
		t.Fatal(err)
	}
	if hold := h.observation().Hold; hold == nil || hold.By != "alice" || hold.Reason != "paused by alice: db incident" || hold.Since.IsZero() {
		t.Errorf("hold right after Pause = %+v, want the pause by alice, with no cycle in between", hold)
	}

	if err := h.engine.Resume(ctx, testNamespace, "web", "bob"); err != nil {
		t.Fatal(err)
	}
	if hold := h.observation().Hold; hold != nil {
		t.Errorf("hold right after Resume = %+v, want none, with no cycle in between", hold)
	}
}

func TestPauseErrors(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	observedWeb(t, h, "auto")

	if err := h.engine.Pause(ctx, testNamespace, "web", "", ""); err == nil {
		t.Error("Pause with no actor succeeded")
	}
	if err := h.engine.Pause(ctx, testNamespace, "nope", "alice", ""); !errors.Is(err, ErrNotPausable) {
		t.Errorf("Pause of a job that is not in the repository: err = %v, want ErrNotPausable", err)
	}
	if err := h.engine.Pause(ctx, "prod", "web", "alice", ""); !errors.Is(err, ErrNotPausable) {
		t.Errorf("Pause in a namespace nops does not manage: err = %v, want ErrNotPausable", err)
	}
	if err := h.engine.Pause(ctx, testNamespace, "web", "alice", strings.Repeat("x", maxPauseNote+1)); !errors.Is(err, ErrPauseNoteTooLong) {
		t.Errorf("Pause with a long reason: err = %v, want ErrPauseNoteTooLong", err)
	}
	if err := h.engine.Pause(ctx, testNamespace, "web", "alice", "  spaces around  "); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if p, err := h.store.PauseOf(ctx, testNamespace, "web"); err != nil || p.Reason != "spaces around" {
		t.Errorf("stored pause = %+v, %v, want the reason trimmed", p, err)
	}
	if err := h.engine.Pause(ctx, testNamespace, "web", "bob", ""); !errors.Is(err, store.ErrAlreadyPaused) {
		t.Errorf("second Pause: err = %v, want ErrAlreadyPaused", err)
	}
}

func TestResumeErrors(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	observedWeb(t, h, "auto")

	if err := h.engine.Resume(ctx, testNamespace, "web", ""); err == nil {
		t.Error("Resume with no actor succeeded")
	}
	if err := h.engine.Resume(ctx, testNamespace, "web", "alice"); !errors.Is(err, store.ErrNotPaused) {
		t.Errorf("Resume of a job that is not paused: err = %v, want ErrNotPaused", err)
	}
	if err := h.engine.Resume(ctx, "prod", "web", "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Resume in a namespace nops does not manage: err = %v, want ErrNotFound", err)
	}
}

// A pause in one namespace is not a pause of the same ID in another.
func TestPauseIsPerNamespace(t *testing.T) {
	h := newHarnessIn(t, "default", "prod")
	ctx := context.Background()
	prod := managed("web", "auto", nil)
	ns := "prod"
	prod.Namespace = &ns
	h.nomad.setFile("web-default", managed("web", "auto", nil))
	h.nomad.setFile("web-prod", prod)
	h.snap.set("c1",
		gitwatch.File{Path: "a.nomad.hcl", Content: "web-default"},
		gitwatch.File{Path: "b.nomad.hcl", Content: "web-prod"})
	h.detect()
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.nomad.setDrift("prod/web", &api.JobDiff{Type: "Edited", ID: "web"})

	if err := h.engine.Pause(ctx, "prod", "web", "alice", ""); err != nil {
		t.Fatal(err)
	}
	h.detect()

	if d := h.active("web"); d.State != store.StateDetected {
		t.Errorf("default/web deployment = %+v, want a detected one: only prod/web is paused", d)
	}
	if _, err := h.store.ActiveDeployment(ctx, "prod", "web"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("prod/web active deployment: err = %v, want ErrNotFound: it is paused", err)
	}
}

// pauseReadFails is the store with a PauseOf that fails: what a person's pause
// is cannot be known.
type pauseReadFails struct{ Store }

func (pauseReadFails) PauseOf(context.Context, string, string) (store.Pause, error) {
	return store.Pause{}, errors.New("disk I/O error")
}

// Invariant 7: a pause that cannot be read is not "not paused". The apply loop
// leaves a detected deployment as it is, and Approve refuses, rather than start
// a deployment of a job someone may have paused.
func TestAPauseThatCannotBeReadStartsNothing(t *testing.T) {
	t.Run("apply", func(t *testing.T) {
		h := newHarness(t)
		observedWeb(t, h, "auto")
		h.drift()
		h.detect()
		d := h.active("web")
		h.engine.store = pauseReadFails{h.store}

		h.step(d)

		if got := h.get(d.ID); got.State != store.StateDetected {
			t.Errorf("state = %s, want detected: nothing starts on a guess", got.State)
		}
	})
	t.Run("approve", func(t *testing.T) {
		h := newHarness(t)
		observedWeb(t, h, "approval")
		h.drift()
		h.detect()
		pending := h.active("web")
		h.engine.store = pauseReadFails{h.store}

		if err := h.engine.Approve(context.Background(), pending.ID, pending.SpecHash, "bob"); err == nil {
			t.Fatal("Approve succeeded, want the failed read returned")
		}
		if got := h.get(pending.ID); got.State != store.StatePendingApproval || got.DecidedBy != "" {
			t.Errorf("deployment = %+v, want pending and undecided", got)
		}
	})
}
