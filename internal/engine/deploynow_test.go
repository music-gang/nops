package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// heldWeb is windowedWeb, observed: a job "web" with drift, held by its closed
// window, and the observation a page would show.
func heldWeb(t *testing.T, h *harness) Observation {
	t.Helper()
	windowedWeb(t, h, "auto")
	h.detect()
	h.noActive("web")
	obs := h.observation()
	if obs.Hold == nil || obs.Hold.Kind != HoldWindow {
		t.Fatalf("setup: hold = %+v, want the window", obs.Hold)
	}
	return obs
}

func (h *harness) deployNow(specHash string) (string, error) {
	h.t.Helper()
	return h.engine.DeployNow(context.Background(), testNamespace, "web", specHash, "alice")
}

func (h *harness) lifts() []store.WindowLift {
	h.t.Helper()
	l, err := h.store.WindowLifts(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return l
}

// What the page Deploy now sends you back to reads is the store and the last
// observation: neither may need another cycle to say what it did.
func TestDeployNowStartsTheDeploymentOutsideTheWindow(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)

	id, err := h.deployNow(obs.SpecHash)
	if err != nil {
		t.Fatalf("DeployNow: %v", err)
	}

	d := h.active("web")
	if id != d.ID || d.State != store.StateDetected || d.WindowLiftedBy != "alice" {
		t.Errorf("DeployNow = %q, active = %+v, want the new detected deployment, asked by alice", id, d)
	}
	if got := h.lifts(); len(got) != 0 {
		t.Errorf("lifts after the deployment = %+v, want it used up", got)
	}
	after := h.observation()
	if after.Hold != nil || after.DeployNowBy != "" {
		t.Errorf("observation = %+v, want no hold and no request waiting: the page reads it at once", after)
	}

	// Apply starts it though the window is closed, and the next cycle leaves it.
	h.step(d)
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Fatalf("state after apply's step = %s, want applying", got.State)
	}
	h.detect()
	if got := h.active("web"); got.ID != d.ID || got.State != store.StateApplying {
		t.Errorf("active after the next cycle = %+v, want the same one, applying", got)
	}
}

// Once, for the one deployment: when it is over and the job drifts again, the
// window holds it.
func TestDeployNowIsOneShot(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)
	id, err := h.deployNow(obs.SpecHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(context.Background(), id, store.StateCompleted,
		store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}

	h.detect()

	h.noActive("web")
	if o := h.observation(); o.Hold == nil || o.Hold.Kind != HoldWindow {
		t.Errorf("hold after the deployment = %+v, want the window again", o.Hold)
	}
}

func TestDeployNowRecordsWhoAskedInTheDeploymentsEvents(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)
	id, err := h.deployNow(obs.SpecHash)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := h.store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Actor == "alice" && e.Message == "deploy now requested outside the sync window" {
			return
		}
	}
	t.Errorf("events = %+v, want one by alice for the deploy now", evs)
}

// The page asked for the spec it showed. Git having moved on, the job stays
// held and shows the new drift.
func TestDeployNowForAnotherSpecIsRefused(t *testing.T) {
	h := newHarness(t)
	heldWeb(t, h)

	if _, err := h.deployNow("not-the-spec-on-the-page"); !errors.Is(err, ErrNotDeployable) {
		t.Errorf("DeployNow of another spec: err = %v, want ErrNotDeployable", err)
	}
	if got := h.lifts(); len(got) != 0 {
		t.Errorf("lifts = %+v, want none: a refused request is not kept", got)
	}
	h.noActive("web")
}

// A request is for the spec it was made for. One that git has outdated before
// the cycle reads it is dropped, never applied to the new spec.
func TestDeployNowDoesNotLiftTheWindowForANewerSpec(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)
	if err := h.store.RequestWindowLift(context.Background(), testNamespace, "web", obs.SpecHash, "alice"); err != nil {
		t.Fatal(err)
	}
	h.nomad.setFile("web-v2", managed("web", "auto", windowed(), taskGroup("g", 2, false)))
	h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"})

	h.detect()

	h.noActive("web")
	if got := h.lifts(); len(got) != 0 {
		t.Errorf("lifts = %+v, want the outdated request dropped", got)
	}
	if o := h.observation(); o.Hold == nil || !o.Drift || o.SpecHash == obs.SpecHash {
		t.Errorf("observation = %+v, want the new drift, still held", o)
	}
}

func TestDeployNowDoesNotLiftAPause(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)
	if err := h.engine.Pause(context.Background(), testNamespace, "web", "bob", "db incident"); err != nil {
		t.Fatal(err)
	}

	if _, err := h.deployNow(obs.SpecHash); !errors.Is(err, ErrNotDeployable) {
		t.Errorf("DeployNow of a paused job: err = %v, want ErrNotDeployable", err)
	}

	// A pause that lands between the request and the cycle wins as well.
	if err := h.store.RequestWindowLift(context.Background(), testNamespace, "web", obs.SpecHash, "alice"); err != nil {
		t.Fatal(err)
	}
	h.detect()
	h.noActive("web")
	if got := h.lifts(); len(got) != 0 {
		t.Errorf("lifts = %+v, want the request dropped", got)
	}
}

// The lifted deployment is not started yet, and a pause still stops it.
func TestAPauseSupersedesALiftedDeployment(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)
	id, err := h.deployNow(obs.SpecHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Pause(context.Background(), testNamespace, "web", "bob", ""); err != nil {
		t.Fatal(err)
	}

	h.detect()

	h.noActive("web")
	if got := h.get(id); got.State != store.StateSuperseded {
		t.Errorf("state = %s, want superseded by the pause", got.State)
	}
}

func TestDeployNowOnAJobThatIsNotHeldByItsWindow(t *testing.T) {
	t.Run("the window is open", func(t *testing.T) {
		h := newHarness(t)
		windowedWeb(t, h, "auto")
		h.clock.Advance(untilMorning)
		h.detect()
		// Detection created the deployment: nothing is held, nothing to lift.
		obs := h.observation()
		if _, err := h.deployNow(obs.SpecHash); !errors.Is(err, ErrNotDeployable) {
			t.Errorf("DeployNow inside the window: err = %v, want ErrNotDeployable", err)
		}
	})
	t.Run("approval ignores the window", func(t *testing.T) {
		h := newHarness(t)
		windowedWeb(t, h, "approval")
		h.detect()
		obs := h.observation()
		if _, err := h.deployNow(obs.SpecHash); !errors.Is(err, ErrNotDeployable) {
			t.Errorf("DeployNow under approval: err = %v, want ErrNotDeployable", err)
		}
	})
	t.Run("nothing to deploy", func(t *testing.T) {
		h := newHarness(t)
		obs := heldWeb(t, h)
		h.nomad.setDrift("web", nil)
		h.detect()
		if _, err := h.deployNow(obs.SpecHash); !errors.Is(err, ErrNotDeployable) {
			t.Errorf("DeployNow without drift: err = %v, want ErrNotDeployable", err)
		}
	})
	t.Run("a failed deployment blocks it", func(t *testing.T) {
		h := newHarness(t)
		failedWeb(t, h, managed("web", "auto", windowed()))
		h.detect()
		obs := h.observation()
		if obs.BlockedBy == "" {
			t.Fatalf("setup: observation = %+v, want blocked", obs)
		}
		if _, err := h.deployNow(obs.SpecHash); !errors.Is(err, ErrNotDeployable) {
			t.Errorf("DeployNow of a blocked job: err = %v, want ErrNotDeployable: a retry lifts a block", err)
		}
	})
	t.Run("a deployment is running", func(t *testing.T) {
		h := newHarness(t)
		windowedWeb(t, h, "auto")
		h.clock.Advance(untilMorning)
		h.detect()
		d := h.active("web")
		h.step(d) // applying
		h.clock.Advance(windowClosed)
		h.detect()
		obs := h.observation()
		if obs.Hold == nil || !obs.Drift {
			t.Fatalf("setup: observation = %+v, want held with drift", obs)
		}
		if _, err := h.deployNow(obs.SpecHash); !errors.Is(err, ErrNotDeployable) {
			t.Errorf("DeployNow with a deployment running: err = %v, want ErrNotDeployable", err)
		}
	})
	t.Run("a job nops does not know", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.engine.DeployNow(context.Background(), testNamespace, "ghost", "h", "alice"); !errors.Is(err, ErrNotDeployable) {
			t.Errorf("DeployNow of an unobserved job: err = %v, want ErrNotDeployable", err)
		}
		if _, err := h.engine.DeployNow(context.Background(), "elsewhere", "web", "h", "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("DeployNow in another namespace: err = %v, want ErrNotFound", err)
		}
	})
}

func TestDeployNowRequiresAnActor(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)
	if _, err := h.engine.DeployNow(context.Background(), testNamespace, "web", obs.SpecHash, ""); err == nil {
		t.Error("DeployNow without an actor: want an error")
	}
}

// The request is on record before anything is done (invariant 7): a restart
// between the deployment and its start loses nothing.
func TestALiftedDeploymentSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)
	id, err := h.deployNow(obs.SpecHash)
	if err != nil {
		t.Fatal(err)
	}

	h.restart(h.hooks)
	h.detect()
	d := h.active("web")
	if d.ID != id || d.WindowLiftedBy != "alice" {
		t.Fatalf("active after the restart = %+v, want %s, asked by alice", d, id)
	}
	h.step(d)
	if got := h.get(id); got.State != store.StateApplying {
		t.Errorf("state after the restart = %s, want applying", got.State)
	}
}

// An accepted request survives a cycle that fails: the page says it is waiting,
// and the next cycle that runs uses it.
func TestDeployNowKeepsTheRequestWhenTheCycleFails(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)
	fs := &failingLifts{Store: h.store}
	e := New(Options{
		Store: fs, Nomad: h.nomad, Snapshots: h.snap, Notifier: h.notifier, Hooks: h.hooks,
		Namespaces: []string{testNamespace}, DriftInterval: time.Hour, EngineInterval: time.Hour, ApplyTimeout: time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	e.now = h.clock.Now
	h.engine = e
	h.detect()
	fs.fail = true

	id, err := h.deployNow(obs.SpecHash)
	if err != nil || id != "" {
		t.Fatalf("DeployNow = %q, %v, want no deployment yet and no error: the request is accepted", id, err)
	}
	if got := h.lifts(); len(got) != 1 || got[0].RequestedBy != "alice" {
		t.Errorf("lifts = %+v, want alice's request kept", got)
	}
	if got := h.observation(); got.DeployNowBy != "alice" {
		t.Errorf("observation = %+v, want it to say alice's request waits: the page must not offer the button again", got)
	}
	if h.kicked() == 0 {
		t.Error("detection was not asked for a cycle")
	}

	fs.fail = false
	h.detect()
	if d := h.active("web"); d.WindowLiftedBy != "alice" {
		t.Errorf("active = %+v, want the deployment of alice's request", d)
	}
	if got := h.observation(); got.DeployNowBy != "" {
		t.Errorf("observation = %+v, want the wait over", got)
	}
}

// failingLifts is a store whose WindowLifts fails on demand: a detection cycle
// that cannot read the requests aborts.
type failingLifts struct {
	Store
	fail bool
}

func (f *failingLifts) WindowLifts(ctx context.Context) ([]store.WindowLift, error) {
	if f.fail {
		return nil, errors.New("disk is full")
	}
	return f.Store.WindowLifts(ctx)
}

// A request for a job that left the repository must not wait for the job to
// come back.
func TestAWindowLiftOfARemovedJobIsDropped(t *testing.T) {
	h := newHarness(t)
	obs := heldWeb(t, h)
	if err := h.store.RequestWindowLift(context.Background(), testNamespace, "web", obs.SpecHash, "alice"); err != nil {
		t.Fatal(err)
	}
	h.snap.set("c2", gitwatch.File{Path: "other.nomad.hcl", Content: "other-v1"})
	h.nomad.setFile("other-v1", managed("other", "auto", nil))

	h.detect()

	if got := h.lifts(); len(got) != 0 {
		t.Errorf("lifts = %+v, want the request of the removed job dropped", got)
	}
}
