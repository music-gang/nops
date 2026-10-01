package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/hooks"
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

// retryOf is Retry of a deployment by a person.
func (h *harness) retryOf(id string) (string, error) {
	h.t.Helper()
	return h.engine.Retry(context.Background(), id, "iacopo")
}

// What the page a retry sends you back to reads is the store and the last
// observation: neither may need another cycle to say what the retry did.

func TestRetryUnblocksUnderApproval(t *testing.T) {
	h := newHarness(t)
	blocked := blockedWeb(t, h, "approval", store.StateRejected)

	next, err := h.retryOf(blocked.ID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	old := h.get(blocked.ID)
	if old.RetriedBy != "iacopo" || old.RetriedAt.IsZero() || old.State != store.StateRejected {
		t.Errorf("blocked deployment after Retry = %+v, want retried by iacopo and still rejected", old)
	}

	// No cycle ran since: Retry ran it. Invariant 3: retrying unblocks, it never
	// approves. The new deployment waits for a human like any other under policy
	// approval.
	fresh := h.active("web")
	if fresh.ID == blocked.ID || fresh.ID != next || fresh.State != store.StatePendingApproval || fresh.DecidedBy != "" {
		t.Fatalf("deployment after retry = %+v (Retry said %s), want a new pending_approval one, undecided", fresh, next)
	}
	if fresh.SpecHash != blocked.SpecHash || fresh.RetryOf != blocked.ID {
		t.Errorf("new deployment = %+v, want the same spec hash and a retry of %s", fresh, blocked.ID)
	}
	if obs := h.engine.Observations(); len(obs) != 1 || obs[0].BlockedBy != "" {
		t.Errorf("observations = %+v, want unblocked", obs)
	}
}

func TestRetryUnblocksUnderAuto(t *testing.T) {
	h := newHarness(t)
	blocked := blockedWeb(t, h, "auto", store.StateFailed)

	next, err := h.retryOf(blocked.ID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}

	fresh := h.active("web")
	if fresh.ID == blocked.ID || fresh.ID != next || fresh.State != store.StateDetected || fresh.Policy != store.PolicyAuto {
		t.Fatalf("deployment after retry = %+v (Retry said %s), want a new detected auto one", fresh, next)
	}
}

func TestRetryRecordsAnEvent(t *testing.T) {
	h := newHarness(t)
	blocked := blockedWeb(t, h, "auto", store.StateFailed)
	if _, err := h.retryOf(blocked.ID); err != nil {
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
	blocked := blockedWeb(t, h, "auto", store.StateFailed)
	fresh, err := h.retryOf(blocked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(context.Background(), fresh, store.StateFailed,
		store.Transition{From: store.StateDetected, Actor: "nops", Error: "boom again"}); err != nil {
		t.Fatal(err)
	}

	h.detect()

	obs := h.engine.Observations()
	if len(obs) != 1 || obs[0].BlockedBy != fresh {
		t.Fatalf("observations = %+v, want blocked by the new failure %s", obs, fresh)
	}
	if _, err := h.retryOf(fresh); err != nil {
		t.Errorf("second Retry: %v", err)
	}
}

func TestRetryRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("no actor", func(t *testing.T) {
		h := newHarness(t)
		blocked := blockedWeb(t, h, "auto", store.StateFailed)
		if _, err := h.engine.Retry(ctx, blocked.ID, ""); err == nil {
			t.Error("Retry without an actor should fail")
		}
	})

	t.Run("unknown deployment", func(t *testing.T) {
		h := newHarness(t)
		blockedWeb(t, h, "auto", store.StateFailed)
		if _, err := h.retryOf("nope"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want store.ErrNotFound", err)
		}
	})

	t.Run("namespace nops does not manage", func(t *testing.T) {
		h := newHarness(t)
		blockedWeb(t, h, "auto", store.StateFailed)
		other := &store.Deployment{JobID: "web", Namespace: "staging", CommitSHA: "c1", SpecHash: "h", JobSpec: `{"ID":"web"}`, Policy: store.PolicyAuto}
		if err := h.store.CreateDeployment(ctx, other); err != nil {
			t.Fatal(err)
		}
		if err := h.store.Transition(ctx, other.ID, store.StateFailed, store.Transition{From: store.StateDetected, Actor: "nops", Error: "boom"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.retryOf(other.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want store.ErrNotFound", err)
		}
	})

	t.Run("job with no drift", func(t *testing.T) {
		h := newHarness(t)
		blocked := blockedWeb(t, h, "auto", store.StateFailed)
		h.nomad.setDrift("web", &api.JobDiff{Type: "None", ID: "web"})
		h.detect()
		if _, err := h.retryOf(blocked.ID); !errors.Is(err, ErrNotRetryable) {
			t.Errorf("err = %v, want ErrNotRetryable: it never reached the register and nothing differs", err)
		}
		if got := h.get(blocked.ID); !got.RetriedAt.IsZero() {
			t.Error("a refused Retry marked the deployment")
		}
	})

	t.Run("already retried", func(t *testing.T) {
		h := newHarness(t)
		blocked := blockedWeb(t, h, "auto", store.StateFailed)
		if _, err := h.retryOf(blocked.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.retryOf(blocked.ID); !errors.Is(err, store.ErrAlreadyRetried) {
			t.Errorf("err = %v, want store.ErrAlreadyRetried", err)
		}
	})

	t.Run("a newer deployment exists", func(t *testing.T) {
		h := newHarness(t)
		blocked := blockedWeb(t, h, "auto", store.StateFailed)
		// Someone (a restart's recovery, say) left a newer deployment since the
		// last cycle: the failed one is not the head of the job any more.
		newer := &store.Deployment{
			JobID: "web", Namespace: testNamespace, CommitSHA: "c2", SpecHash: "other",
			JobSpec: `{"ID":"web"}`, Policy: store.PolicyAuto,
		}
		if err := h.store.CreateDeployment(ctx, newer); err != nil {
			t.Fatal(err)
		}
		if _, err := h.retryOf(blocked.ID); !errors.Is(err, ErrNotRetryable) {
			t.Errorf("err = %v, want ErrNotRetryable", err)
		}
		if got := h.get(blocked.ID); !got.RetriedAt.IsZero() {
			t.Error("a refused Retry marked the old deployment")
		}
	})

	t.Run("an older spec", func(t *testing.T) {
		// A deployment of a spec git no longer has would be a rollback.
		h := newHarness(t)
		blocked := blockedWeb(t, h, "auto", store.StateFailed)
		h.nomad.setFile("web-v2", managed("web", "auto", map[string]string{"x": "2"}))
		h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"})
		h.nomad.setDrift("web", &api.JobDiff{Type: "None", ID: "web"}) // no new deployment: the failed one is still the latest
		h.detect()
		if _, err := h.retryOf(blocked.ID); !errors.Is(err, ErrNotRetryable) {
			t.Errorf("err = %v, want ErrNotRetryable: git has another spec now", err)
		}
	})

	t.Run("policy none", func(t *testing.T) {
		h := newHarness(t)
		blocked := blockedWeb(t, h, "auto", store.StateFailed)
		h.nomad.setFile("web-none", managed("web", "none", nil))
		h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-none"})
		h.detect()
		if _, err := h.retryOf(blocked.ID); !errors.Is(err, ErrNotRetryable) {
			t.Errorf("err = %v, want ErrNotRetryable: nops deploys nothing of a job under policy none", err)
		}
	})
}

// TestRetryWakesNothingLater: a cycle the loop runs after Retry has nothing
// left to do, because Retry already ran its own.
func TestRetryRunsItsCycleBeforeItAnswers(t *testing.T) {
	h := newHarness(t)
	blocked := blockedWeb(t, h, "auto", store.StateFailed)
	before := h.engine.Status().At

	next, err := h.retryOf(blocked.ID)
	if err != nil {
		t.Fatal(err)
	}

	if !h.engine.Status().At.After(before) {
		t.Error("Retry did not run a detection cycle")
	}
	if got := h.latest("web"); got.ID != next || got.RetryOf != blocked.ID {
		t.Errorf("latest deployment = %+v, want the retry %s of %s", got, next, blocked.ID)
	}
}

// A retry while the loop runs a cycle of its own must not leave the older
// observations in place of the newer: run under -race.
func TestRetryWhileTheDetectionLoopRuns(t *testing.T) {
	h := newHarness(t)
	blocked := blockedWeb(t, h, "auto", store.StateFailed)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.engine.RunDetection(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the first cycle", func() bool { return h.engine.Status().Managed == 1 })

	for range 20 {
		h.engine.kickDetection()
	}
	next, err := h.retryOf(blocked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.latest("web"); got.ID != next {
		t.Errorf("latest = %s, want %s", got.ID, next)
	}
	waitFor(t, "an unblocked observation", func() bool {
		obs := h.engine.Observations()
		return len(obs) == 1 && obs[0].BlockedBy == ""
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
	blocked := blockedWeb(t, h, "auto", store.StateFailed)
	boom := errors.New("disk on fire")
	h.engine.store = failingMarkRetried{Store: h.engine.store, err: boom}

	_, err := h.retryOf(blocked.ID)
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

// -- a retry with nothing to register ------------------------------------------

// postHookWeb brings job web, with the post-hook smoke, to a failed post-hook:
// it is registered and live, the Nomad deployment is successful, and the hook
// failed. The repository and Nomad agree: there is no drift.
func postHookWeb(t *testing.T, h *harness, policy string) *store.Deployment {
	t.Helper()
	ctx := context.Background()
	j := managed("web", policy, map[string]string{"nops_post_hook": "smoke"})
	h.nomad.setFile("web-v1", j)
	h.nomad.setFile("smoke-v1", hookJob("smoke"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "smoke.nomad.hcl", Content: "smoke-v1"})
	h.detect()
	d := h.active("web")
	if policy == "approval" {
		if err := h.engine.Approve(ctx, d.ID, d.SpecHash, "iacopo"); err != nil {
			t.Fatal(err)
		}
	}
	h.step(h.get(d.ID)) // detected -> applying (auto only)
	if policy == "auto" {
		h.step(h.get(d.ID))
	} else {
		h.step(h.get(d.ID))
	}
	h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: h.get(d.ID).AppliedIndex, Status: api.DeploymentStatusSuccessful})
	h.step(h.get(d.ID)) // health -> post_hook
	if got := h.get(d.ID); got.State != store.StatePostHook {
		t.Fatalf("setup: state = %s, want post_hook", got.State)
	}
	h.hooks.setResult(d.ID, "post", hooks.Result{State: store.HookFailed, Error: "exit 1"})
	h.step(h.get(d.ID))
	failed := h.get(d.ID)
	if failed.State != store.StateFailed || failed.AppliedIndex == 0 {
		t.Fatalf("setup: %+v, want failed after the register", failed)
	}
	h.nomad.setDrift("web", &api.JobDiff{Type: "None", ID: "web"})
	h.detect()
	if obs := h.observation(); obs.Drift || obs.BlockedBy != "" {
		t.Fatalf("setup: observation = %+v, want no drift and so no block", obs)
	}
	return failed
}

func TestAFailedPostHookCanBeRetriedAndRunsOnlyThePostHooksAgain(t *testing.T) {
	h := newHarness(t)
	failed := postHookWeb(t, h, "auto")
	registers := len(h.nomad.registerCalls)
	pre := len(h.hooks.calls)

	next, err := h.retryOf(failed.ID)
	if err != nil {
		t.Fatalf("Retry of a failed post-hook: %v", err)
	}

	// The page it returns to has the deployment already.
	got := h.get(next)
	if next == failed.ID || got.RetryOf != failed.ID || got.State != store.StateDetected || got.PlanDiff != "" {
		t.Fatalf("after Retry: %+v, want a detected retry of %s with nothing to apply", got, failed.ID)
	}
	if !Rerun(got) {
		t.Fatal("the new deployment is not a rerun")
	}

	h.step(got) // detected -> applying: no pre-hook, whatever the spec says
	if st := h.get(next).State; st != store.StateApplying {
		t.Fatalf("state = %s, want applying: the pre-hooks do not run again", st)
	}
	h.step(h.get(next)) // the register step: an empty plan, nothing to write
	if len(h.nomad.registerCalls) != registers {
		t.Fatalf("Register calls = %d, want %d: invariant 1, no register without a difference", len(h.nomad.registerCalls), registers)
	}
	applied := h.get(next)
	if applied.State != store.StateApplying || applied.AppliedIndex == 0 {
		t.Fatalf("after the register step: %+v, want applying and waiting for health, not completed", applied)
	}
	h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: applied.AppliedIndex, Status: api.DeploymentStatusSuccessful})
	h.step(applied) // health -> post_hook
	h.step(h.get(next))

	if st := h.get(next).State; st != store.StateCompleted {
		t.Fatalf("state = %s, want completed", st)
	}
	calls := h.hooks.calls[pre:]
	if len(calls) != 1 || calls[0].Phase != "post" || calls[0].DeploymentID != next {
		t.Errorf("hook runs = %+v, want the post-hook of the new deployment, once", calls)
	}
}

func TestARetryWithNothingToRegisterIsKnownAfterARestart(t *testing.T) {
	h := newHarness(t)
	failed := postHookWeb(t, h, "auto")
	next, err := h.retryOf(failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	registers := len(h.nomad.registerCalls)

	// A restart: a new engine on the same store and Nomad, its first apply cycle
	// resumes the deployment from what SQLite holds, in every state it passes.
	restart := func() *Engine {
		return New(Options{
			Store: h.store, Nomad: h.nomad, Snapshots: h.snap, Notifier: h.notifier, Hooks: h.hooks,
			Namespaces: []string{testNamespace}, DriftInterval: time.Hour, EngineInterval: time.Hour, ApplyTimeout: time.Hour,
		})
	}
	for range 2 {
		e := restart()
		e.applyStep(context.Background(), h.get(next))
	}
	got := h.get(next)
	if got.State != store.StateApplying || got.AppliedIndex == 0 {
		t.Fatalf("after the restarts: %+v, want applying, registered by no one", got)
	}
	if len(h.nomad.registerCalls) != registers {
		t.Errorf("Register calls = %d, want %d: recovery does not register a retry with nothing to register", len(h.nomad.registerCalls), registers)
	}
}

func TestARetryWithNothingToRegisterUnderApprovalWaitsForApprovalAndSkipsThePreHooks(t *testing.T) {
	h := newHarness(t)
	failed := postHookWeb(t, h, "approval")

	next, err := h.retryOf(failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := h.get(next)
	if got.State != store.StatePendingApproval || got.DecidedBy != "" || got.RetryOf != failed.ID {
		t.Fatalf("after Retry: %+v, want pending_approval, undecided, a retry of %s", got, failed.ID)
	}

	// A later cycle does not read "no drift" as "in sync" and complete it.
	h.detect()
	if got := h.get(next); got.State != store.StatePendingApproval {
		t.Fatalf("after a cycle: state = %s, want it still waiting for a person", got.State)
	}

	if err := h.engine.Approve(context.Background(), next, got.SpecHash, "iacopo"); err != nil {
		t.Fatal(err)
	}
	if st := h.get(next).State; st != store.StateApplying {
		t.Errorf("state after Approve = %s, want applying: no pre-hook to run", st)
	}
}

func TestARetryWithDriftRunsThePreHooksAgain(t *testing.T) {
	h := newHarness(t)
	j := managed("web", "auto", map[string]string{"nops_pre_hook": "migrate"})
	h.nomad.setFile("web-v1", j)
	h.nomad.setFile("migrate-v1", hookJob("migrate"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "migrate.nomad.hcl", Content: "migrate-v1"})
	h.detect()
	d := h.active("web")
	h.step(d) // detected -> pre_hook
	h.hooks.setResult(d.ID, "pre", hooks.Result{State: store.HookFailed, Error: "exit 1"})
	h.step(h.get(d.ID))
	h.detect()

	next, err := h.retryOf(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := h.get(next)
	if got.PlanDiff == "" || Rerun(got) {
		t.Fatalf("the retry of a deployment that never registered is a rerun: %+v", got)
	}
	h.hooks.setResult(next, "pre", hooks.Result{State: store.HookSucceeded})
	h.step(got)
	if st := h.get(next).State; st != store.StatePreHook {
		t.Errorf("state = %s, want pre_hook: with drift the retry starts over", st)
	}
}

func TestAFailedDeploymentIsRetryableOnlyAsTheHead(t *testing.T) {
	h := newHarness(t)
	failed := postHookWeb(t, h, "auto")
	latest := h.latest("web")

	if !h.engine.Retryable(failed, latest) {
		t.Error("the failed post-hook is not retryable")
	}
	if h.engine.Retryable(failed, nil) {
		t.Error("retryable without knowing the latest deployment")
	}
	other := *failed
	other.ID = "not-the-latest"
	if h.engine.Retryable(&other, latest) {
		t.Error("a deployment that is not the latest is retryable")
	}
	done := *failed
	done.State = store.StateCompleted
	if h.engine.Retryable(&done, &done) {
		t.Error("a completed deployment is retryable")
	}
}

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
