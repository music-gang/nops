package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

func TestPausedNopsGivesEveryJobNoDeployment(t *testing.T) {
	for _, policy := range []string{"auto", "approval"} {
		t.Run(policy, func(t *testing.T) {
			h := newHarness(t)
			observedWeb(t, h, policy)
			if err := h.engine.PauseAll(context.Background(), "alice", "nomad upgrade"); err != nil {
				t.Fatal(err)
			}

			h.drift()
			h.detect()

			h.noActive("web")
			obs := h.observation()
			if !obs.Drift || obs.PlanDiff == "" {
				t.Errorf("observation = %+v, want the drift still detected and shown", obs)
			}
			if obs.Hold == nil || obs.Hold.Kind != HoldPaused || !obs.Hold.All || obs.Hold.By != "alice" || obs.Hold.Note != "nomad upgrade" ||
				obs.Hold.Reason != "every job paused by alice: nomad upgrade" || obs.Hold.Since.IsZero() {
				t.Errorf("hold = %+v, want the pause of every job by alice for a nomad upgrade", obs.Hold)
			}
		})
	}
}

// A job added to git after the pause is held too: the pause is of Nops, not of
// the jobs that existed when it was made.
func TestPausedNopsHoldsAJobAddedMeanwhile(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	if err := h.engine.PauseAll(context.Background(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	h.nomad.setFile("api-v1", managed("api", "auto", nil))
	h.snap.set("c2",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "api.nomad.hcl", Content: "api-v1"})

	h.detect()

	h.noActive("api")
	for _, o := range h.engine.Observations() {
		if o.Hold == nil || !o.Hold.All {
			t.Errorf("hold of %s = %+v, want the pause of every job", o.JobID, o.Hold)
		}
	}
}

// A job paused on its own stays paused when every job is resumed, and its own
// pause is the hold it shows meanwhile.
func TestAJobPausedOnItsOwnStaysPausedAfterResumeAll(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	ctx := context.Background()
	if err := h.engine.Pause(ctx, testNamespace, "web", "alice", "hand fix"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseAll(ctx, "bob", ""); err != nil {
		t.Fatal(err)
	}
	h.drift()
	h.detect()
	if hold := h.observation().Hold; hold == nil || hold.All || hold.By != "alice" {
		t.Errorf("hold with both pauses = %+v, want the job's own, by alice", hold)
	}

	if err := h.engine.ResumeAll(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	h.detect()

	h.noActive("web")
	if hold := h.observation().Hold; hold == nil || hold.All || hold.By != "alice" {
		t.Errorf("hold after ResumeAll = %+v, want the job's own pause by alice", hold)
	}
}

func TestResumeAllLetsTheNextCycleDeployAsUsual(t *testing.T) {
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
			if err := h.engine.PauseAll(ctx, "alice", ""); err != nil {
				t.Fatal(err)
			}
			h.drift()
			h.detect()
			h.noActive("web")

			if err := h.engine.ResumeAll(ctx, "bob"); err != nil {
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

func TestPauseAllSupersedesADetectedDeployment(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	h.drift()
	h.detect()
	before := h.active("web")
	ctx := context.Background()
	if err := h.engine.PauseAll(ctx, "alice", "nomad upgrade"); err != nil {
		t.Fatal(err)
	}

	h.detect()

	h.noActive("web")
	if got := h.get(before.ID); got.State != store.StateSuperseded {
		t.Fatalf("state = %s, want superseded", got.State)
	}
	if err := h.engine.ResumeAll(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	h.detect()
	if after := h.active("web"); after.ID == before.ID || after.State != store.StateDetected {
		t.Errorf("deployment after the resume = %+v, want a new detected one", after)
	}
}

func TestPauseAllRefusesAnApprovalAndStillRejects(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "approval")
	h.drift()
	h.detect()
	pending := h.active("web")
	ctx := context.Background()
	if err := h.engine.PauseAll(ctx, "alice", ""); err != nil {
		t.Fatal(err)
	}
	h.detect()

	err := h.engine.Approve(ctx, pending.ID, pending.SpecHash, "bob")
	if !errors.Is(err, ErrPaused) || !strings.Contains(err.Error(), "every job paused by alice") {
		t.Fatalf("Approve: err = %v, want ErrPaused saying who paused every job", err)
	}
	if got := h.get(pending.ID); got.State != store.StatePendingApproval || got.DecidedBy != "" {
		t.Errorf("deployment after the refused Approve = %+v, want pending and undecided", got)
	}

	if err := h.engine.ResumeAll(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Approve(ctx, pending.ID, pending.SpecHash, "bob"); err != nil {
		t.Fatalf("Approve after the resume: %v", err)
	}

	h2 := newHarness(t)
	observedWeb(t, h2, "approval")
	h2.drift()
	h2.detect()
	p2 := h2.active("web")
	if err := h2.engine.PauseAll(ctx, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := h2.engine.Reject(ctx, p2.ID, "bob"); err != nil {
		t.Errorf("Reject while every job is paused: %v", err)
	}
}

// What is already running finishes: the pause stops the next one.
func TestPauseAllLeavesADeploymentInFlightAlone(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	h.drift()
	h.detect()
	d := h.active("web")
	h.step(d) // detected -> applying
	d = h.get(d.ID)
	if err := h.engine.PauseAll(context.Background(), "alice", ""); err != nil {
		t.Fatal(err)
	}

	h.detect()
	if got := h.active("web"); got.ID != d.ID || got.State != store.StateApplying {
		t.Fatalf("deployment after a cycle of a paused Nops = %+v, want the same applying one", got)
	}
	h.step(d)

	if got := h.get(d.ID); got.AppliedIndex == 0 {
		t.Errorf("deployment = %+v, want it registered: a pause does not stop what already started", got)
	}
}

// Apply and detection are two loops: a deployment still detected when the pause
// lands is not started by apply before detection has put it aside.
func TestApplyDoesNotStartADetectedDeploymentWhileNopsIsPaused(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	h.drift()
	h.detect()
	d := h.active("web")
	ctx := context.Background()
	if err := h.store.PauseAll(ctx, "alice", ""); err != nil {
		t.Fatal(err)
	}
	for len(h.engine.kick) > 0 {
		<-h.engine.kick
	}

	h.step(d)

	if got := h.get(d.ID); got.State != store.StateDetected {
		t.Fatalf("state = %s, want detected: apply must not start a deployment while Nops is paused", got.State)
	}
	if h.kicked() != 1 {
		t.Errorf("queued cycles = %d, want 1: detection puts the deployment aside", h.kicked())
	}

	if err := h.store.ResumeAll(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	h.step(d)
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Errorf("state after the resume = %s, want applying", got.State)
	}
}

// The page a person is sent back to reads the last observation and PausedAll:
// they must say so at once, and a cycle asked for.
func TestPauseAllAndResumeAllShowAtOnce(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	ctx := context.Background()
	for len(h.engine.kick) > 0 {
		<-h.engine.kick
	}

	if err := h.engine.PauseAll(ctx, "alice", "nomad upgrade"); err != nil {
		t.Fatal(err)
	}
	if hold := h.observation().Hold; hold == nil || !hold.All || hold.By != "alice" {
		t.Errorf("hold right after PauseAll = %+v, want the pause of every job by alice", hold)
	}
	if all := h.engine.PausedAll(); all == nil || all.By != "alice" || all.Note != "nomad upgrade" {
		t.Errorf("PausedAll right after PauseAll = %+v, want the pause by alice", all)
	}
	if h.kicked() != 1 {
		t.Errorf("queued cycles after PauseAll = %d, want 1", h.kicked())
	}
	<-h.engine.kick

	if err := h.engine.ResumeAll(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if hold := h.observation().Hold; hold != nil {
		t.Errorf("hold right after ResumeAll = %+v, want none", hold)
	}
	if all := h.engine.PausedAll(); all != nil {
		t.Errorf("PausedAll right after ResumeAll = %+v, want none", all)
	}
	if h.kicked() != 1 {
		t.Errorf("queued cycles after ResumeAll = %d, want 1", h.kicked())
	}
}

// PausedAll is read from the store by every cycle, so a pause made elsewhere
// (a restart) shows too.
func TestDetectionReadsThePauseOfEveryJob(t *testing.T) {
	h := newHarness(t)
	observedWeb(t, h, "auto")
	if err := h.store.PauseAll(context.Background(), "alice", ""); err != nil {
		t.Fatal(err)
	}

	h.detect()

	if all := h.engine.PausedAll(); all == nil || all.By != "alice" {
		t.Errorf("PausedAll after a cycle = %+v, want the pause by alice", all)
	}
}

func TestPauseAllNotifiesOnPauseAndResume(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.engine.PauseAll(ctx, "alice", "nomad upgrade"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ResumeAll(ctx, "bob"); err != nil {
		t.Fatal(err)
	}

	got := h.notifier.pausesSent(t, 2)
	if !(strings.Contains(got[0], "paused alice: nomad upgrade") && strings.Contains(got[1], "resumed bob")) &&
		!(strings.Contains(got[1], "paused alice: nomad upgrade") && strings.Contains(got[0], "resumed bob")) {
		t.Errorf("notifications = %v, want a pause by alice and a resume by bob", got)
	}
}

func TestPauseAllErrors(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if err := h.engine.PauseAll(ctx, "", ""); err == nil {
		t.Error("PauseAll with no actor succeeded")
	}
	if err := h.engine.PauseAll(ctx, "alice", strings.Repeat("x", maxPauseNote+1)); !errors.Is(err, ErrPauseNoteTooLong) {
		t.Errorf("PauseAll with a long reason: err = %v, want ErrPauseNoteTooLong", err)
	}
	if err := h.engine.PauseAll(ctx, "alice", "  spaces around  "); err != nil {
		t.Fatalf("PauseAll: %v", err)
	}
	if p, err := h.store.AllPause(ctx); err != nil || p.Reason != "spaces around" {
		t.Errorf("stored pause = %+v, %v, want the reason trimmed", p, err)
	}
	if err := h.engine.PauseAll(ctx, "bob", ""); !errors.Is(err, store.ErrAlreadyPaused) {
		t.Errorf("second PauseAll: err = %v, want ErrAlreadyPaused", err)
	}
	h.notifier.pausesSent(t, 1) // the refused ones sent nothing
}

func TestResumeAllErrors(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if err := h.engine.ResumeAll(ctx, ""); err == nil {
		t.Error("ResumeAll with no actor succeeded")
	}
	if err := h.engine.ResumeAll(ctx, "alice"); !errors.Is(err, store.ErrNotPaused) {
		t.Errorf("ResumeAll of a Nops that is not paused: err = %v, want ErrNotPaused", err)
	}
}

// allPauseReadFails is the store with an AllPause that fails: whether Nops is
// paused cannot be known.
type allPauseReadFails struct{ Store }

func (allPauseReadFails) AllPause(context.Context) (store.Pause, error) {
	return store.Pause{}, errors.New("disk I/O error")
}

// Invariant 7: a pause that cannot be read is not "not paused". Detection
// aborts, apply leaves a detected deployment as it is, and Approve refuses.
func TestThePauseOfEveryJobThatCannotBeReadStartsNothing(t *testing.T) {
	t.Run("detection", func(t *testing.T) {
		h := newHarness(t)
		observedWeb(t, h, "auto")
		h.drift()
		h.engine.store = allPauseReadFails{h.store}

		if err := h.engine.Detect(context.Background()); err == nil {
			t.Fatal("Detect succeeded, want the failed read returned")
		}
		h.noActive("web")
	})
	t.Run("apply", func(t *testing.T) {
		h := newHarness(t)
		observedWeb(t, h, "auto")
		h.drift()
		h.detect()
		d := h.active("web")
		h.engine.store = allPauseReadFails{h.store}

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
		h.engine.store = allPauseReadFails{h.store}

		if err := h.engine.Approve(context.Background(), pending.ID, pending.SpecHash, "bob"); err == nil {
			t.Fatal("Approve succeeded, want the failed read returned")
		}
		if got := h.get(pending.ID); got.State != store.StatePendingApproval || got.DecidedBy != "" {
			t.Errorf("deployment = %+v, want pending and undecided", got)
		}
	})
}
