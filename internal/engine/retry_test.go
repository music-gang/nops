package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// blockedWeb sets up a job "web" with drift, whose latest deployment (for the
// same spec) ended in the given terminal state, and runs a detection cycle so
// the engine has seen it blocked.
func blockedWeb(t *testing.T, h *harness, policy string, end store.State) *store.Deployment {
	t.Helper()
	ctx := context.Background()
	j := managed("web", policy, nil)
	h.nomad.setFile("web-v1", j)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	hash, err := specHash(j)
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Deployment{
		JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: hash,
		JobSpec: `{"ID":"web"}`, Policy: store.Policy(policy), CASIndex: 0,
	}
	if err := h.store.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	switch end {
	case store.StateFailed:
		if err := h.store.Transition(ctx, d.ID, store.StateFailed,
			store.Transition{From: store.StateDetected, Actor: "nops", Error: "boom"}); err != nil {
			t.Fatal(err)
		}
	case store.StateRejected:
		if err := h.store.Transition(ctx, d.ID, store.StatePendingApproval,
			store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.Transition(ctx, d.ID, store.StateRejected,
			store.Transition{From: store.StatePendingApproval, Actor: "iacopo", DecidedBy: "iacopo"}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("blockedWeb: unsupported end state %s", end)
	}

	h.detect()
	if obs := h.engine.Observations(); len(obs) != 1 || obs[0].BlockedBy != d.ID {
		t.Fatalf("setup: observations = %+v, want blocked by %s", obs, d.ID)
	}
	return d
}

func TestRetryUnblocksUnderApproval(t *testing.T) {
	h := newHarness(t)
	blocked := blockedWeb(t, h, "approval", store.StateRejected)

	if err := h.engine.Retry(context.Background(), testNamespace, "web", "iacopo"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	old, err := h.store.GetDeployment(context.Background(), blocked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.RetriedBy != "iacopo" || old.RetriedAt.IsZero() || old.State != store.StateRejected {
		t.Errorf("blocked deployment after Retry = %+v, want retried by iacopo and still rejected", old)
	}
	if len(h.engine.kick) != 1 {
		t.Fatalf("Retry did not ask the detection loop for a cycle")
	}

	h.detect()

	// Invariant 3: retrying unblocks, it never approves. The new deployment
	// waits for a human like any other under policy approval.
	fresh := h.active("web")
	if fresh.ID == blocked.ID || fresh.State != store.StatePendingApproval || fresh.DecidedBy != "" {
		t.Fatalf("deployment after retry = %+v, want a new pending_approval one, undecided", fresh)
	}
	if fresh.SpecHash != blocked.SpecHash {
		t.Errorf("new deployment has another spec hash: the retry should be for the same spec")
	}
	if obs := h.engine.Observations(); len(obs) != 1 || obs[0].BlockedBy != "" {
		t.Errorf("observations = %+v, want unblocked", obs)
	}
}

func TestRetryUnblocksUnderAuto(t *testing.T) {
	h := newHarness(t)
	blocked := blockedWeb(t, h, "auto", store.StateFailed)

	if err := h.engine.Retry(context.Background(), testNamespace, "web", "iacopo"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	h.detect()

	fresh := h.active("web")
	if fresh.ID == blocked.ID || fresh.State != store.StateDetected || fresh.Policy != store.PolicyAuto {
		t.Fatalf("deployment after retry = %+v, want a new detected auto one", fresh)
	}
}

func TestRetryRecordsAnEvent(t *testing.T) {
	h := newHarness(t)
	blocked := blockedWeb(t, h, "auto", store.StateFailed)
	if err := h.engine.Retry(context.Background(), testNamespace, "web", "iacopo"); err != nil {
		t.Fatal(err)
	}
	evs, err := h.store.Events(context.Background(), blocked.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Actor != "iacopo" || last.Message != "retry requested" {
		t.Errorf("last event = %+v, want the retry by iacopo", last)
	}
}

func TestRetryAgainBlocksAgain(t *testing.T) {
	// A retry buys one attempt: if the new deployment fails the same way, the
	// job is blocked again and needs another explicit retry.
	h := newHarness(t)
	blockedWeb(t, h, "auto", store.StateFailed)
	if err := h.engine.Retry(context.Background(), testNamespace, "web", "iacopo"); err != nil {
		t.Fatal(err)
	}
	h.detect()
	fresh := h.active("web")
	if err := h.store.Transition(context.Background(), fresh.ID, store.StateFailed,
		store.Transition{From: store.StateDetected, Actor: "nops", Error: "boom again"}); err != nil {
		t.Fatal(err)
	}

	h.detect()

	obs := h.engine.Observations()
	if len(obs) != 1 || obs[0].BlockedBy != fresh.ID {
		t.Fatalf("observations = %+v, want blocked by the new failure %s", obs, fresh.ID)
	}
	if err := h.engine.Retry(context.Background(), testNamespace, "web", "iacopo"); err != nil {
		t.Errorf("second Retry: %v", err)
	}
}

func TestRetryRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("no actor", func(t *testing.T) {
		h := newHarness(t)
		blockedWeb(t, h, "auto", store.StateFailed)
		if err := h.engine.Retry(ctx, testNamespace, "web", ""); err == nil {
			t.Error("Retry without an actor should fail")
		}
	})

	t.Run("another namespace", func(t *testing.T) {
		h := newHarness(t)
		blockedWeb(t, h, "auto", store.StateFailed)
		if err := h.engine.Retry(ctx, "staging", "web", "iacopo"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want store.ErrNotFound", err)
		}
	})

	t.Run("unknown job", func(t *testing.T) {
		h := newHarness(t)
		blockedWeb(t, h, "auto", store.StateFailed)
		if err := h.engine.Retry(ctx, testNamespace, "nope", "iacopo"); !errors.Is(err, ErrNotBlocked) {
			t.Errorf("err = %v, want ErrNotBlocked", err)
		}
	})

	t.Run("job with no drift", func(t *testing.T) {
		h := newHarness(t)
		h.nomad.setFile("web-v1", managed("web", "auto", nil))
		h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
		h.detect()
		if err := h.engine.Retry(ctx, testNamespace, "web", "iacopo"); !errors.Is(err, ErrNotBlocked) {
			t.Errorf("err = %v, want ErrNotBlocked", err)
		}
		if len(h.engine.kick) != 0 {
			t.Error("a refused Retry asked for a cycle")
		}
	})

	t.Run("already retried", func(t *testing.T) {
		h := newHarness(t)
		blockedWeb(t, h, "auto", store.StateFailed)
		if err := h.engine.Retry(ctx, testNamespace, "web", "iacopo"); err != nil {
			t.Fatal(err)
		}
		// No cycle ran in between: the observation still says blocked, but the
		// deployment is already marked.
		if err := h.engine.Retry(ctx, testNamespace, "web", "iacopo"); !errors.Is(err, store.ErrAlreadyRetried) {
			t.Errorf("err = %v, want store.ErrAlreadyRetried", err)
		}
	})

	t.Run("observation older than the latest deployment", func(t *testing.T) {
		h := newHarness(t)
		blocked := blockedWeb(t, h, "auto", store.StateFailed)
		// Someone (a restart's recovery, say) left a newer deployment since the
		// last cycle: the observation no longer describes the job.
		newer := &store.Deployment{
			JobID: "web", Namespace: testNamespace, CommitSHA: "c2", SpecHash: "other",
			JobSpec: `{"ID":"web"}`, Policy: store.PolicyAuto,
		}
		if err := h.store.CreateDeployment(ctx, newer); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.Retry(ctx, testNamespace, "web", "iacopo"); !errors.Is(err, ErrNotBlocked) {
			t.Errorf("err = %v, want ErrNotBlocked", err)
		}
		if got, _ := h.store.GetDeployment(ctx, blocked.ID); !got.RetriedAt.IsZero() {
			t.Error("a refused Retry marked the old deployment")
		}
	})
}

// TestRetryWakesTheDetectionLoop runs the real loop, with an hour between
// ticks: the new deployment can only come from the kick Retry sends.
func TestRetryWakesTheDetectionLoop(t *testing.T) {
	h := newHarness(t)
	blocked := blockedWeb(t, h, "auto", store.StateFailed)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.engine.RunDetection(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// The loop's first cycle sees the job blocked; wait for it, then retry.
	waitFor(t, "the first cycle", func() bool { return h.engine.Status().Managed == 1 })
	if err := h.engine.Retry(ctx, testNamespace, "web", "iacopo"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a new deployment", func() bool {
		d, err := h.store.LatestDeployment(ctx, testNamespace, "web")
		return err == nil && d.ID != blocked.ID
	})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRetryStoreFailureIsReturned(t *testing.T) {
	h := newHarness(t)
	blockedWeb(t, h, "auto", store.StateFailed)
	boom := errors.New("disk on fire")
	h.engine.store = failingMarkRetried{Store: h.engine.store, err: boom}

	err := h.engine.Retry(context.Background(), testNamespace, "web", "iacopo")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store's error", err)
	}
	if len(h.engine.kick) != 0 {
		t.Error("Retry asked for a cycle although nothing was persisted")
	}
}

type failingMarkRetried struct {
	Store
	err error
}

func (f failingMarkRetried) MarkRetried(context.Context, string, string) error { return f.err }

func TestStatus(t *testing.T) {
	h := newHarness(t)
	if st := h.engine.Status(); !st.At.IsZero() {
		t.Fatalf("Status before any cycle = %+v, want zero", st)
	}

	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.nomad.setFile("db-v1", managed("db", "approval", nil))
	h.nomad.setFile("hook", hookJob("migrate"))
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "db.nomad.hcl", Content: "db-v1"},
		gitwatch.File{Path: "migrate.nomad.hcl", Content: "hook"})
	h.detect()

	st := h.engine.Status()
	if st.At.IsZero() || st.Duration <= 0 || st.Error != "" || st.Managed != 2 || st.Skipped != 0 || st.Unparsed != 0 {
		t.Errorf("Status after a clean cycle = %+v, want 2 managed and nothing skipped", st)
	}
	first := st.At

	// Nomad fails to plan one job and cannot parse one file: the cycle still
	// runs to the end, and Status says what it could not do.
	h.nomad.planErr["db"] = errors.New("nomad is down")
	h.nomad.setParseErr("broken", fmt.Errorf("syntax error"))
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "db.nomad.hcl", Content: "db-v1"},
		gitwatch.File{Path: "broken.nomad.hcl", Content: "broken"})
	h.clock.Advance(time.Minute)
	h.detect()

	st = h.engine.Status()
	if st.Error != "" || st.Managed != 2 || st.Skipped != 1 || st.Unparsed != 1 || !st.At.After(first.Add(time.Minute)) {
		t.Errorf("Status after a degraded cycle = %+v, want 2 managed, 1 skipped, 1 unparsed", st)
	}
}

func TestStatusRecordsAnAbortedCycle(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.engine.store = failingCreate{Store: h.engine.store}

	if err := h.engine.Detect(context.Background()); err == nil {
		t.Fatal("Detect should fail when the store does")
	}
	st := h.engine.Status()
	if st.Error == "" || st.At.IsZero() {
		t.Errorf("Status after an aborted cycle = %+v, want the error and a time", st)
	}
}

type failingCreate struct{ Store }

func (failingCreate) CreateDeployment(context.Context, *store.Deployment) error {
	return errors.New("disk on fire")
}
