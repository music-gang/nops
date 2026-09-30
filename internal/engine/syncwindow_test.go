package engine

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// The harness clock starts at Thursday 2026-09-24 12:00 UTC, and these tests
// use a window that opens at 09:00 for two hours: closed at the start, open
// from the next morning.
const (
	windowKey      = "nops_sync_window"
	windowDuration = "nops_sync_window_duration"
	untilMorning   = 21*time.Hour + 30*time.Minute // 12:00 -> 09:30 the next day
	windowClosed   = 2 * time.Hour                 // 09:30 -> 11:30
)

func windowed() map[string]string {
	return map[string]string{windowKey: "0 9 * * *", windowDuration: "2h"}
}

// windowedWeb is a job "web" with the window above and drift, read by one
// detection cycle (the first is at the start of the harness clock: outside).
func windowedWeb(t *testing.T, h *harness, policy string) {
	t.Helper()
	h.nomad.setFile("web-v1", managed("web", policy, windowed()))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
}

func TestOutsideTheWindowNoDeploymentIsCreated(t *testing.T) {
	h := newHarness(t)
	windowedWeb(t, h, "auto")

	h.detect()

	h.noActive("web")
	obs := h.observation()
	if !obs.Drift || obs.PlanDiff == "" {
		t.Errorf("observation = %+v, want the drift still detected and shown", obs)
	}
	if obs.Hold == nil || obs.Hold.Kind != HoldWindow || obs.Hold.Reason != "outside its sync window, next opens Fri 2026-09-25 09:00 UTC" {
		t.Errorf("hold = %+v, want the window, with when it opens next", obs.Hold)
	}
	if obs.Hold.By != "" || !obs.Hold.Since.IsZero() {
		t.Errorf("hold = %+v, want nobody's name on it: no person set it", obs.Hold)
	}
}

// When the window opens detection plans as usual: what is applied is never a
// plan from hours ago, and the state machine has no state for waiting.
func TestInsideTheWindowADeploymentIsCreated(t *testing.T) {
	h := newHarness(t)
	windowedWeb(t, h, "auto")
	h.detect()
	h.noActive("web")

	h.clock.Advance(untilMorning)
	h.detect()

	if d := h.active("web"); d.State != store.StateDetected {
		t.Errorf("deployment inside the window = %s, want detected", d.State)
	}
	if obs := h.observation(); obs.Hold != nil {
		t.Errorf("hold inside the window = %+v, want none", obs.Hold)
	}
}

func TestWindowClosingSupersedesADetectedDeployment(t *testing.T) {
	h := newHarness(t)
	windowedWeb(t, h, "auto")
	h.clock.Advance(untilMorning)
	h.detect()
	before := h.active("web")

	h.clock.Advance(windowClosed)
	h.detect()

	h.noActive("web")
	if got := h.get(before.ID); got.State != store.StateSuperseded {
		t.Fatalf("state = %s, want superseded", got.State)
	}
	evs, err := h.store.Events(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := evs[len(evs)-1]; last.Message != "outside its sync window, next opens Sat 2026-09-26 09:00 UTC" {
		t.Errorf("last event = %+v, want the closed window as the reason", last)
	}

	h.clock.Advance(21*time.Hour + 30*time.Minute) // the next morning
	h.detect()
	if after := h.active("web"); after.ID == before.ID || after.State != store.StateDetected {
		t.Errorf("deployment the next morning = %+v, want a new detected one", after)
	}
}

// The window gates the start: what is running when it closes finishes.
func TestWindowClosingLeavesADeploymentInFlightAlone(t *testing.T) {
	h := newHarness(t)
	windowedWeb(t, h, "auto")
	h.clock.Advance(untilMorning)
	h.detect()
	d := h.active("web")
	h.step(d) // detected -> applying
	d = h.get(d.ID)
	if d.State != store.StateApplying {
		t.Fatalf("setup: state = %s, want applying", d.State)
	}

	h.clock.Advance(windowClosed)
	h.detect()
	if got := h.active("web"); got.ID != d.ID || got.State != store.StateApplying {
		t.Fatalf("deployment after the window closed = %+v, want the same applying one", got)
	}
	h.step(d)

	if n := len(h.nomad.registerCalls); n != 1 {
		t.Errorf("registers = %d, want 1: a window does not stop what already started", n)
	}
}

// Apply and detection are two loops: a deployment created while the window was
// open is not started by apply once it has closed.
func TestApplyDoesNotStartADetectedDeploymentOutsideItsWindow(t *testing.T) {
	h := newHarness(t)
	windowedWeb(t, h, "auto")
	h.clock.Advance(untilMorning)
	h.detect()
	d := h.active("web")
	h.clock.Advance(windowClosed)
	if h.kicked() != 0 {
		t.Fatalf("setup: %d cycles queued", h.kicked())
	}

	h.step(d)

	if got := h.get(d.ID); got.State != store.StateDetected {
		t.Fatalf("state = %s, want detected: apply must not start a deployment outside its window", got.State)
	}
	if h.kicked() != 1 {
		t.Errorf("queued cycles = %d, want 1: detection puts the deployment aside", h.kicked())
	}
}

func TestApplyStartsADetectedDeploymentInsideItsWindow(t *testing.T) {
	h := newHarness(t)
	windowedWeb(t, h, "auto")
	h.clock.Advance(untilMorning)
	h.detect()
	d := h.active("web")

	h.step(d)

	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Errorf("state = %s, want applying", got.State)
	}
}

// Under approval the human OK is the gate: the window neither holds the
// deployment nor the approval, and the job says so.
func TestAWindowIsIgnoredUnderApproval(t *testing.T) {
	h := newHarness(t)
	windowedWeb(t, h, "approval")
	ctx := context.Background()

	h.detect() // outside the window

	pending := h.active("web")
	if pending.State != store.StatePendingApproval {
		t.Fatalf("deployment outside the window = %s, want pending_approval", pending.State)
	}
	obs := h.observation()
	if obs.Hold != nil {
		t.Errorf("hold under approval = %+v, want none", obs.Hold)
	}
	var warned bool
	for _, iss := range obs.Issues {
		warned = warned || (iss.Severity == meta.SeverityWarn && iss.Key == windowKey)
	}
	if !warned {
		t.Errorf("issues = %+v, want a warning that the window is ignored under approval", obs.Issues)
	}
	if err := h.engine.Approve(ctx, pending.ID, pending.SpecHash, "alice"); err != nil {
		t.Fatalf("Approve outside the window: %v", err)
	}
	if got := h.get(pending.ID); got.State != store.StateApplying {
		t.Errorf("state = %s, want applying: an approval outside the window applies at once", got.State)
	}
}

// The instance's time zone decides when 09:00 is.
func TestTheWindowIsReadInTheInstanceTimeZone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	h := newHarness(t)
	h.engine.syncLoc = tokyo
	windowedWeb(t, h, "auto")

	// 12:00 UTC is 21:00 in Tokyo: closed.
	h.detect()
	h.noActive("web")
	if obs := h.observation(); obs.Hold == nil || obs.Hold.Reason != "outside its sync window, next opens Fri 2026-09-25 09:00 JST" {
		t.Errorf("hold = %+v, want the window in Tokyo time", obs.Hold)
	}

	// 00:30 UTC the next day is 09:30 in Tokyo: open.
	h.clock.Advance(12*time.Hour + 30*time.Minute)
	h.detect()
	if d := h.active("web"); d.State != store.StateDetected {
		t.Errorf("deployment at 09:30 in Tokyo = %s, want detected", d.State)
	}
}

// A person's pause is the stronger hold, and the one a person can lift.
func TestAPauseWinsOverTheWindow(t *testing.T) {
	h := newHarness(t)
	windowedWeb(t, h, "auto")
	h.detect()
	if err := h.engine.Pause(context.Background(), testNamespace, "web", "alice", ""); err != nil {
		t.Fatal(err)
	}

	h.detect()

	if obs := h.observation(); obs.Hold == nil || obs.Hold.Kind != HoldPaused {
		t.Errorf("hold = %+v, want the pause", obs.Hold)
	}
}

// Self-heal is automatic too: a hand edit outside the window is left until it
// opens, and then put back.
func TestSelfHealWaitsForTheWindow(t *testing.T) {
	h := newHarness(t)
	windowedWeb(t, h, "auto")
	h.detect()
	h.noActive("web")

	h.clock.Advance(untilMorning)
	h.detect()
	d := h.active("web")
	h.step(d)
	h.step(h.get(d.ID))

	if n := len(h.nomad.registerCalls); n != 1 {
		t.Errorf("registers = %d, want 1 once the window is open", n)
	}
}

// A job with no drift in a closed window has nothing to hold: the observation
// carries the hold (the page decides whether to say so) and no deployment.
func TestClosedWindowWithNoDrift(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", windowed()))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	h.noActive("web")
	if obs := h.observation(); obs.Drift || obs.Hold == nil || obs.Hold.Kind != HoldWindow {
		t.Errorf("observation = %+v, want no drift and the window's hold", obs)
	}
}

func TestAnInvalidWindowIsPolicyNone(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{windowKey: "whenever", windowDuration: "2h"}))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	h.noActive("web")
	obs := h.observation()
	if obs.Policy != meta.PolicyNone || obs.Hold != nil {
		t.Errorf("observation = %+v, want policy none and no hold: invariant 4, an invalid key is read as none", obs)
	}
}

// Where the window stands is on the observation, with its zone, whether or not
// the job drifts: a person reads it from the job's page.
func TestTheObservationSaysWhereTheWindowStands(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", windowed())) // in sync: no drift
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect() // 12:00 UTC: closed
	ws := h.observation().Window
	if ws == nil || ws.Open || ws.Spec != "0 9 * * *" || ws.Duration != 2*time.Hour || ws.Zone != "UTC" ||
		ws.Format(ws.Until) != "Fri 2026-09-25 09:00 UTC" {
		t.Errorf("window at 12:00 = %+v, want closed until Fri 2026-09-25 09:00 UTC, read in UTC", ws)
	}

	h.clock.Advance(untilMorning) // 09:30: open
	h.detect()
	ws = h.observation().Window
	if ws == nil || !ws.Open || ws.Format(ws.Until) != "Fri 2026-09-25 11:00 UTC" {
		t.Errorf("window at 09:30 = %+v, want open until Fri 2026-09-25 11:00 UTC", ws)
	}
}

func TestTheWindowStatusNamesTheInstanceTimeZone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	h := newHarness(t)
	h.engine.syncLoc = tokyo
	h.nomad.setFile("web-v1", managed("web", "auto", windowed()))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	ws := h.observation().Window
	if ws == nil || ws.Zone != "Asia/Tokyo" || ws.Format(ws.Until) != "Fri 2026-09-25 09:00 JST" {
		t.Errorf("window = %+v, want it read, and written, in Tokyo time", ws)
	}
}

// A window that does not apply is not shown as if it did.
func TestNoWindowStatusWhenItDoesNotApply(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]string
		pol   string
	}{
		{"no window", nil, "auto"},
		{"under approval", windowed(), "approval"},
		{"under none", windowed(), "none"},
		{"an invalid one", map[string]string{windowKey: "whenever", windowDuration: "2h"}, "auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.nomad.setFile("web-v1", managed("web", tc.pol, tc.extra))
			h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
			h.detect()
			if ws := h.observation().Window; ws != nil {
				t.Errorf("window = %+v, want none", ws)
			}
		})
	}
}
