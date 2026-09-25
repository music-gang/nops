package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// multi creates a deployment of web in pre_hook or post_hook with several
// hooks in each phase, as detection would have frozen them.
func (h *harness) multi(to store.State) *store.Deployment {
	h.t.Helper()
	job := managed("web", "auto", map[string]string{
		"nops_pre_hook": "backup, migrate, warm", "nops_post_hook": "smoke,notify",
	})
	d := h.newDeployment("web", job, store.PolicyAuto, 0)
	if to == store.StatePostHook {
		d = h.move(d, store.StateApplying)
	}
	return h.move(d, to)
}

func (h *harness) hookIDs() []string {
	var out []string
	for _, c := range h.hooks.calls {
		out = append(out, c.HookJobID)
	}
	return out
}

func TestHooksOfAPhaseRunInOrderAndTheDeploymentMovesOnAfterTheLast(t *testing.T) {
	h := newHarness(t)
	d := h.multi(store.StatePreHook)

	h.step(d)

	if got := h.hooks.order(); !reflect.DeepEqual(got, []string{"pre/0", "pre/1", "pre/2"}) {
		t.Fatalf("runs = %v, want the three pre-hooks in the order they are declared", got)
	}
	want := []string{revisionOfPlainHook("backup"), revisionOfPlainHook("migrate"), revisionOfPlainHook("warm")}
	if got := h.hookIDs(); !reflect.DeepEqual(got, want) {
		t.Errorf("dispatched %v, want the revisions of backup, migrate and warm", got)
	}
	for _, id := range []string{"backup", "migrate", "warm"} {
		if _, ok := h.nomad.live[revisionOfPlainHook(id)]; !ok {
			t.Errorf("the revision of %s was not registered", id)
		}
	}
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Errorf("state = %s, want applying only once every pre-hook succeeded", got.State)
	}
}

func TestPostHooksRunInOrderAndCompleteTheDeployment(t *testing.T) {
	h := newHarness(t)
	d := h.multi(store.StatePostHook)

	h.step(d)

	if got := h.hooks.order(); !reflect.DeepEqual(got, []string{"post/0", "post/1"}) {
		t.Fatalf("runs = %v, want smoke then notify", got)
	}
	if got := h.get(d.ID); got.State != store.StateCompleted {
		t.Errorf("state = %s, want completed", got.State)
	}
}

func TestAFailingHookStopsThePhaseAndTheOnesAfterItNeverRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail int
		want []string
	}{
		{"the first", 0, []string{"pre/0"}},
		{"the middle one", 1, []string{"pre/0", "pre/1"}},
		{"the last", 2, []string{"pre/0", "pre/1", "pre/2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.multi(store.StatePreHook)
			h.hooks.setResultAt(d.ID, "pre", tc.fail, hooks.Result{State: store.HookFailed, Error: "exit 1"})

			h.step(d)

			if got := h.hooks.order(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("runs = %v, want %v: nothing after the failure runs", got, tc.want)
			}
			got := h.get(d.ID)
			if got.State != store.StateFailed {
				t.Fatalf("state = %s, want failed", got.State)
			}
			name := []string{"backup", "migrate", "warm"}[tc.fail]
			for _, want := range []string{"pre-hook", name, "failed", "exit 1", "live job left untouched"} {
				if !strings.Contains(got.Error, want) {
					t.Errorf("error = %q, want it to say %q: which hook failed, and that the job was not touched", got.Error, want)
				}
			}
			for _, c := range h.nomad.registerCalls {
				if c.id == "web" {
					t.Errorf("the job was registered after a failed pre-hook: %+v", c)
				}
			}
			// The hooks after the failing one were not even registered in Nomad.
			for i, id := range []string{"backup", "migrate", "warm"} {
				_, registered := h.nomad.live[revisionOfPlainHook(id)]
				if registered != (i <= tc.fail) {
					t.Errorf("revision of %s registered = %v, want %v", id, registered, i <= tc.fail)
				}
			}
		})
	}
}

func TestAFailingPostHookFailsTheDeploymentAndSaysTheApplyStays(t *testing.T) {
	h := newHarness(t)
	d := h.multi(store.StatePostHook)
	h.hooks.setResultAt(d.ID, "post", 0, hooks.Result{State: store.HookTimedOut, Error: "deadline"})

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateFailed || !strings.Contains(got.Error, "post-hook smoke timed_out") || !strings.Contains(got.Error, "apply already live") {
		t.Fatalf("deployment = %s %q", got.State, got.Error)
	}
	if order := h.hooks.order(); !reflect.DeepEqual(order, []string{"post/0"}) {
		t.Errorf("runs = %v, want notify not to run", order)
	}
}

func TestAnErrorMidListIsRetriedFromTheStartAndFinishedHooksAreNotRunAgain(t *testing.T) {
	h := newHarness(t)
	d := h.multi(store.StatePreHook)
	h.hooks.setErrAt(d.ID, "pre", 1, errors.New("nomad unreachable"))

	h.step(d)
	if got := h.get(d.ID); got.State != store.StatePreHook {
		t.Fatalf("state = %s, want pre_hook unchanged after an error", got.State)
	}

	h.hooks.mu.Lock()
	delete(h.hooks.errsAt, hookKeyAt(d.ID, "pre", 1))
	h.hooks.mu.Unlock()
	h.step(h.get(d.ID))

	// The step starts over; the runner answers the first hook from its stored
	// result (checked with the real runner below).
	if got := h.hooks.order(); !reflect.DeepEqual(got, []string{"pre/0", "pre/1", "pre/0", "pre/1", "pre/2"}) {
		t.Errorf("runs = %v", got)
	}
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Errorf("state = %s, want applying", got.State)
	}
}

func TestEachHookRunsWithItsOwnTimeout(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_pre_hook": "backup,migrate"})
	specJSON := mustMarshal(t, job)
	frozen := frozenFromSpec(specJSON)
	frozen[0].JobSpec = mustMarshal(t, withMeta(hookJob("backup"), "nops_timeout", "30m"))
	d := &store.Deployment{JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h", JobSpec: specJSON, Hooks: frozen, Policy: store.PolicyAuto}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	h.step(h.move(d, store.StatePreHook))

	if len(h.hooks.calls) != 2 || h.hooks.calls[0].Timeout != 30*time.Minute || h.hooks.calls[1].Timeout != meta.DefaultHookTimeout {
		t.Fatalf("timeouts = %+v, want 30m from the backup hook and the default for migrate", h.hooks.calls)
	}
}

func TestRegistrationOfALaterHookIsTimedFromTheEndOfThePreviousOne(t *testing.T) {
	h := newHarness(t)
	d := h.multi(store.StatePreHook)
	h.nomad.registerErr[revisionOfPlainHook("migrate")] = errors.New("nomad said no")

	// The backup ran for longer than the migration is allowed: that time is not
	// the migration's. The fake runner writes no hook run, so leave the one a
	// real runner would have.
	h.clock.Advance(meta.DefaultHookTimeout + time.Minute)
	ctx := context.Background()
	run, _, err := h.store.EnsureHookRun(ctx, d.ID, "pre", 0, revisionOfPlainHook("backup"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateHookRun(ctx, run.ID, store.HookUpdate{State: store.HookSucceeded}); err != nil {
		t.Fatal(err)
	}

	h.step(d)
	if got := h.get(d.ID); got.State != store.StatePreHook {
		t.Fatalf("state = %s, want pre_hook: the deployment entered the phase long ago but the second hook has just started", got.State)
	}

	h.clock.Advance(meta.DefaultHookTimeout + time.Second)
	h.step(h.get(d.ID))
	got := h.get(d.ID)
	if got.State != store.StateFailed || !strings.Contains(got.Error, "migrate") || !strings.Contains(got.Error, "nomad said no") {
		t.Fatalf("deployment = %s %q, want failed once the migration's own timeout passed", got.State, got.Error)
	}
}

// -- with the real runner: a crash in the middle of the list -----------------

func TestRecoveryResumesAtTheFirstUnfinishedHook(t *testing.T) {
	h := newHarness(t)
	d := h.newDeployment("web", managed("web", "auto", map[string]string{"nops_pre_hook": "backup,migrate"}), store.PolicyAuto, 0)
	d = h.move(d, store.StatePreHook)

	// What the crash left: the backup finished, the migration was dispatched
	// and its ID never reached the store.
	ctx := context.Background()
	first, _, err := h.store.EnsureHookRun(ctx, d.ID, "pre", 0, revisionOfPlainHook("backup"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateHookRun(ctx, first.ID, store.HookUpdate{State: store.HookSucceeded, DispatchedJobID: "backup/dispatch-1"}); err != nil {
		t.Fatal(err)
	}
	second, _, err := h.store.EnsureHookRun(ctx, d.ID, "pre", 1, revisionOfPlainHook("migrate"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateHookRun(ctx, second.ID, store.HookUpdate{State: store.HookRunning}); err != nil {
		t.Fatal(err)
	}
	hn := &fakeHookNomad{child: "migrate/dispatch-1", token: second.IdempotencyToken, childStatus: "complete"}
	runner := hooks.New(hn, h.store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, hooks.WithClock(h.clock.Now))

	e := h.restart(runner)
	stop := runApplyInBackground(t, e)
	defer stop()
	waitUntil(t, func() bool { return h.get(d.ID).State == store.StateApplying }, "the first RunApply cycle did not finish the phase")
	settle(t, e)

	if hn.dispatches != 0 {
		t.Errorf("dispatches = %d: neither the finished hook nor the one in flight may be dispatched again", hn.dispatches)
	}
	runs, err := h.store.ListHookRuns(ctx, d.ID)
	if err != nil || len(runs) != 2 || runs[0].State != store.HookSucceeded || runs[1].State != store.HookSucceeded || runs[1].DispatchedJobID != "migrate/dispatch-1" {
		t.Errorf("runs = %+v, err %v; want both succeeded, the second on the adopted run", runs, err)
	}
}

// -- detection ------------------------------------------------------------------

func TestDetectionFreezesSeveralHooksInOrder(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", map[string]string{"nops_pre_hook": "backup,migrate", "nops_post_hook": "smoke"}))
	for _, id := range []string{"backup", "migrate", "smoke"} {
		h.nomad.setFile(id+"-v1", hookJob(id))
	}
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	files := []gitwatch.File{{Path: "web.nomad.hcl", Content: "web-v1"}}
	for _, id := range []string{"backup", "migrate", "smoke"} {
		files = append(files, gitwatch.File{Path: id + ".nomad.hcl", Content: id + "-v1"})
	}
	h.snap.set("c1", files...)

	h.detect()

	d := h.active("web")
	frozen, err := h.store.DeploymentHooks(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range frozen {
		got = append(got, f.Phase+"/"+string(rune('0'+f.Position))+"/"+f.HookID)
	}
	if want := []string{"pre/0/backup", "pre/1/migrate", "post/0/smoke"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frozen = %v, want %v", got, want)
	}
	if d.State != store.StatePendingApproval {
		t.Errorf("state = %s", d.State)
	}
}

func TestTheOrderOfTheHooksIsPartOfWhatWasApproved(t *testing.T) {
	cfgA := cfgOf(map[string]string{"nops_pre_hook": "backup,migrate"})
	cfgB := cfgOf(map[string]string{"nops_pre_hook": "migrate,backup"})
	files := hookFiles(hookJob("backup"), hookJob("migrate"))

	_, a, _, _ := freezeHooks(cfgA, files, "t")
	_, b, _, _ := freezeHooks(cfgB, files, "t")
	if a == b {
		t.Error("running the same hooks in another order did not change the hash")
	}
}

func TestAHookWithInvalidMetaFailsTheDeploymentAndFixingItUnblocks(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{"nops_pre_hook": "backup,migrate"}))
	h.nomad.setFile("backup-v1", hookJob("backup"))
	h.nomad.setFile("migrate-bad", withMeta(hookJob("migrate"), "nops_timeout", "soon"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "backup.nomad.hcl", Content: "backup-v1"},
		gitwatch.File{Path: "migrate.nomad.hcl", Content: "migrate-bad"})
	h.detect()

	failed := h.latest("web")
	if failed.State != store.StateFailed || !strings.Contains(failed.Error, `pre-hook "migrate" has invalid meta`) {
		t.Fatalf("deployment = %s %q, want failed, naming the hook with the invalid meta", failed.State, failed.Error)
	}
	h.detect()
	if h.latest("web").ID != failed.ID {
		t.Fatal("a new deployment was created for the same failure")
	}

	h.nomad.setFile("migrate-good", hookJob("migrate"))
	h.snap.set("c2",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "backup.nomad.hcl", Content: "backup-v1"},
		gitwatch.File{Path: "migrate.nomad.hcl", Content: "migrate-good"})
	h.detect()

	if next := h.active("web"); next.ID == failed.ID || next.State != store.StateDetected {
		t.Fatalf("after fixing the hook: %+v, want a new deployment", next)
	}
}

func TestASecondHookMissingFromTheRepoFailsAtDetection(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{"nops_pre_hook": "backup,migrate"}))
	h.nomad.setFile("backup-v1", hookJob("backup"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "backup.nomad.hcl", Content: "backup-v1"})

	h.detect()

	got := h.latest("web")
	if got.State != store.StateFailed || !strings.Contains(got.Error, `pre-hook "migrate" not found in repo at commit c1`) {
		t.Fatalf("deployment = %s %q", got.State, got.Error)
	}
}

func TestRoutingUsesTheLengthOfTheLists(t *testing.T) {
	if got := nextAfterDecision(managed("web", "auto", map[string]string{"nops_pre_hook": "a,b"})); got != store.StatePreHook {
		t.Errorf("with pre-hooks: %s", got)
	}
	if got := nextAfterDecision(managed("web", "auto", map[string]string{"nops_post_hook": "a,b"})); got != store.StateApplying {
		t.Errorf("with post-hooks only: %s, want applying", got)
	}
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_post_hook": "smoke,notify"})
	d := h.applyingWithIndex("web", job, 9)
	h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusSuccessful})

	h.step(d)

	if got := h.get(d.ID); got.State != store.StatePostHook {
		t.Errorf("state = %s, want post_hook once healthy", got.State)
	}
}
