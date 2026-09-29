package engine

// Adversarial tests for the invariants of docs/philosophy.md: each one tries to
// break an invariant (an apply step between every write of detection, a store
// write that fails at every point of a deployment's life, a live job that moves
// inside the window between the read and the register) instead of exercising
// the path near it. Every one was shown to fail against a deliberate break of
// the code it guards.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// -- a journal of every write, in order --------------------------------------

// journal records, in order, every write to the store and to Nomad and every
// hook run of an engine, so a test can say what came before what.
type journal struct {
	mu      sync.Mutex
	entries []string
}

func (j *journal) add(format string, a ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, fmt.Sprintf(format, a...))
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.entries...)
}

// journalStore journals the writes of the store, and fails the failAt-th one
// (counted from 1, 0 for none) once, without doing it.
type journalStore struct {
	Store
	j      *journal
	failAt int
	writes int
}

// write counts a write and reports whether it is the one to fail.
func (s *journalStore) write(what string) error {
	s.writes++
	if s.writes == s.failAt {
		s.j.add("store:%s:FAILED", what)
		return errors.New("disk I/O error")
	}
	return nil
}

func (s *journalStore) CreateDeployment(ctx context.Context, d *store.Deployment) error {
	what := "create"
	if err := s.write(what); err != nil {
		return err
	}
	if err := s.Store.CreateDeployment(ctx, d); err != nil {
		return err
	}
	s.j.add("store:%s", what)
	return nil
}

func (s *journalStore) Transition(ctx context.Context, id string, to store.State, t store.Transition) error {
	what := "transition:" + string(to)
	if err := s.write(what); err != nil {
		return err
	}
	if err := s.Store.Transition(ctx, id, to, t); err != nil {
		return err
	}
	s.j.add("store:%s", what)
	return nil
}

func (s *journalStore) SetApplied(ctx context.Context, id string, appliedIndex uint64, evalID string) error {
	if err := s.write("applied"); err != nil {
		return err
	}
	if err := s.Store.SetApplied(ctx, id, appliedIndex, evalID); err != nil {
		return err
	}
	s.j.add("store:applied")
	return nil
}

// journalNomad journals the writes to Nomad, and the plans that precede them.
// onPlan runs after a Plan, onRegister after a register that went through.
type journalNomad struct {
	Nomad
	j          *journal
	onPlan     func(id string)
	onRegister func(id string)
}

func (n *journalNomad) Plan(ctx context.Context, job *api.Job) (*api.JobPlanResponse, error) {
	n.j.add("nomad:plan:%s", *job.ID)
	res, err := n.Nomad.Plan(ctx, job)
	if err == nil && n.onPlan != nil {
		n.onPlan(*job.ID)
	}
	return res, err
}

func (n *journalNomad) RegisterCAS(ctx context.Context, job *api.Job, index uint64, preserve bool) (*nomadx.RegisterResult, error) {
	n.j.add("nomad:register:%s", *job.ID)
	res, err := n.Nomad.RegisterCAS(ctx, job, index, preserve)
	if err == nil && n.onRegister != nil {
		n.onRegister(*job.ID)
	}
	return res, err
}

func (n *journalNomad) StopJob(ctx context.Context, ns, id string) error {
	n.j.add("nomad:stop:%s", id)
	return n.Nomad.StopJob(ctx, ns, id)
}

type journalHooks struct {
	Hooks
	j *journal
}

func (h *journalHooks) Run(ctx context.Context, req hooks.Request) (hooks.Result, error) {
	h.j.add("hook:run:%s", req.Phase)
	return h.Hooks.Run(ctx, req)
}

// journaled puts the journal between the engine and its store, Nomad and hooks.
func (h *harness) journaled(failAt int) (*journal, *journalStore, *journalNomad) {
	j := &journal{}
	js := &journalStore{Store: h.store, j: j, failAt: failAt}
	jn := &journalNomad{Nomad: h.nomad, j: j}
	h.engine.store = js
	h.engine.nomad = jn
	h.engine.hooks = &journalHooks{Hooks: h.hooks, j: j}
	return j, js, jn
}

// checkOrder fails the test on a write that came before what authorizes it:
//
//   - invariant 1: a register of X needs a plan of X before it, in the same
//     step (the detection cycle's own plan is not the one an apply step acts
//     on), with no other write to the store or to Nomad in between;
//   - invariant 7: the register of the job needs the deployment to be persisted
//     as applying before it, and a hook's registration or run needs it to be
//     persisted as pre_hook or post_hook: the intent is saved, then acted on.
func checkOrder(t *testing.T, name string, entries []string) {
	t.Helper()
	var (
		applying, inHook bool
		lastWrite        = -1
		lastStep         = -1
		plan             = map[string]int{}
		registers        = 0
		fail             = func(i int, format string, a ...any) {
			t.Errorf("%s: %s\njournal: %v", name, fmt.Sprintf(format, a...), entries[:i+1])
		}
		applyingOrHookErr = "before the deployment was persisted as"
	)
	for i, e := range entries {
		switch {
		case e == "step":
			lastStep = i
		case strings.HasPrefix(e, "store:"):
			switch e {
			case "store:transition:applying":
				applying = true
			case "store:transition:pre_hook", "store:transition:post_hook":
				inHook = true
			}
			lastWrite = i
		case strings.HasPrefix(e, "nomad:plan:"):
			plan[strings.TrimPrefix(e, "nomad:plan:")] = i
		case strings.HasPrefix(e, "nomad:register:"):
			id := strings.TrimPrefix(e, "nomad:register:")
			if p, ok := plan[id]; !ok || p < lastStep || p < lastWrite || p < registers {
				fail(i, "%s registered with no plan of it just before (invariant 1)", id)
			}
			if id == "web" && !applying {
				fail(i, "the job was registered %s applying (invariant 7)", applyingOrHookErr)
			}
			if id != "web" && !inHook {
				fail(i, "hook revision %s registered %s pre_hook or post_hook (invariant 7)", id, applyingOrHookErr)
			}
			registers = i
			lastWrite = i
		case strings.HasPrefix(e, "hook:run:"):
			if !inHook {
				fail(i, "a hook ran %s pre_hook or post_hook (invariant 7)", applyingOrHookErr)
			}
		}
	}
}

// lifecycle drives one auto or approval job with a pre-hook and a post-hook
// from detection to completed, the way the loops would (a detection cycle, then
// one apply step, or the human's approval), with the failAt-th store write
// failing once. It returns the journal and how many store writes it made.
func (h *harness) lifecycle(t *testing.T, policy string, failAt int) ([]string, int) {
	t.Helper()
	ctx := context.Background()
	j, js, jn := h.journaled(failAt)
	jn.onRegister = func(id string) {
		if id == "web" { // once registered, git and the cluster agree
			h.nomad.setDrift("web", &api.JobDiff{Type: "None", ID: "web"})
		}
	}
	h.nomad.setFile("web-v1", managed("web", policy, map[string]string{"nops_pre_hook": "backup", "nops_post_hook": "smoke"}))
	h.nomad.setFile("backup-v1", hookJob("backup"))
	h.nomad.setFile("smoke-v1", hookJob("smoke"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	// The Nomad deployment of the first register (index 0 -> 1) is healthy.
	h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: 1, Status: api.DeploymentStatusSuccessful})
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "backup.nomad.hcl", Content: "backup-v1"},
		gitwatch.File{Path: "smoke.nomad.hcl", Content: "smoke-v1"})

	for i := 0; i < 40; i++ {
		_ = h.engine.Detect(ctx) // a failed write aborts the cycle; the next one retries
		d, err := h.store.LatestDeployment(ctx, testNamespace, "web")
		if err != nil {
			continue
		}
		switch d.State {
		case store.StateCompleted, store.StateFailed:
			return j.all(), js.writes
		case store.StatePendingApproval:
			j.add("step")
			_ = h.engine.Approve(ctx, d.ID, d.SpecHash, "alice")
		default:
			j.add("step")
			h.step(d)
		}
	}
	t.Fatalf("%s: the deployment did not finish in 40 rounds: %+v", policy, h.latest("web"))
	return nil, 0
}

func (h *harness) registersOf(id string) int {
	n := 0
	for _, c := range h.nomad.registerCalls {
		if c.id == id {
			n++
		}
	}
	return n
}

// Invariants 1 and 7. A store write fails once, at every point of a
// deployment's life (created, moved on, the applied index recorded), under auto
// and under approval: the deployment still completes, the job is registered
// exactly once, and no write to Nomad ever comes before the write that
// authorizes it, nor without a plan just before it.
func TestEveryStoreWriteOfADeploymentsLifeCanFailOnceWithoutBreakingTheOrder(t *testing.T) {
	for _, policy := range []string{"auto", "approval"} {
		h := newHarness(t)
		entries, writes := h.lifecycle(t, policy, 0)
		if writes < 6 {
			t.Fatalf("%s: only %d store writes in a life with two hooks: %v", policy, writes, entries)
		}
		checkOrder(t, policy+", no failure", entries)

		for n := 1; n <= writes; n++ {
			t.Run(fmt.Sprintf("%s/write %d fails", policy, n), func(t *testing.T) {
				h := newHarness(t)
				entries, _ := h.lifecycle(t, policy, n)

				if got := h.latest("web"); got.State != store.StateCompleted {
					t.Errorf("deployment = %s %q, want completed after one failed write", got.State, got.Error)
				}
				if got := h.registersOf("web"); got != 1 {
					t.Errorf("the job was registered %d times, want once: a repeated step must recover, not register again", got)
				}
				checkOrder(t, fmt.Sprintf("%s, write %d fails", policy, n), entries)
			})
		}
	}
}

// Invariant 7. A write that records the intent to act fails: nothing on Nomad
// happens on its account.
func TestAFailedIntentWriteLeavesNomadUntouched(t *testing.T) {
	failingTransitions := func(h *harness) {
		h.engine.store = &journalStore{Store: h.store, j: &journal{}, failAt: 1}
	}
	touched := func(t *testing.T, h *harness, what string) {
		t.Helper()
		if len(h.nomad.registerCalls) != 0 || len(h.nomad.stopCalls) != 0 || len(h.hooks.calls) != 0 {
			t.Errorf("%s: Nomad or a hook was touched after the write failed: registers %+v, stops %v, hooks %v",
				what, h.nomad.registerCalls, h.nomad.stopCalls, h.hookIDs())
		}
	}

	t.Run("auto, detected to applying", func(t *testing.T) {
		h := newHarness(t)
		h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		d := h.newDeployment("web", managed("web", "auto", nil), store.PolicyAuto, 0)
		failingTransitions(h)
		h.step(d)
		touched(t, h, "auto without hooks")
		if got := h.get(d.ID); got.State != store.StateDetected {
			t.Errorf("state = %s, want it left detected", got.State)
		}
	})
	t.Run("auto, detected to pre_hook", func(t *testing.T) {
		h := newHarness(t)
		d := h.newDeployment("web", managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"}), store.PolicyAuto, 0)
		failingTransitions(h)
		h.step(d)
		touched(t, h, "auto with a pre-hook")
	})
	t.Run("approve", func(t *testing.T) {
		h := newHarness(t)
		h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		d := h.pendingApproval("web", managed("web", "approval", nil), "h1")
		failingTransitions(h)
		if err := h.engine.Approve(context.Background(), d.ID, "h1", "alice"); err == nil {
			t.Error("Approve reported success although its write failed")
		}
		touched(t, h, "approve")
		if got := h.get(d.ID); got.State != store.StatePendingApproval || got.DecidedBy != "" {
			t.Errorf("deployment = %s decided by %q, want it still pending, undecided", got.State, got.DecidedBy)
		}
	})
	t.Run("detection", func(t *testing.T) {
		h := newHarness(t)
		h.nomad.setFile("web-v1", managed("web", "auto", nil))
		h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
		h.engine.store = failingCreate{h.store}
		if err := h.engine.Detect(context.Background()); err == nil {
			t.Error("Detect carried on after its store write failed")
		}
		touched(t, h, "detection")
	})
}

// Invariant 1. A hook revision whose plan fails is not registered, and its hook
// does not run.
func TestAHookRevisionIsNotRegisteredWhenItsPlanFails(t *testing.T) {
	h := newHarness(t)
	d := h.preHooked()
	h.nomad.planErr[revisionOfPlainHook("web-migrate")] = errors.New("plan failed")

	h.step(d)

	if len(h.nomad.registerCalls) != 0 || len(h.hooks.calls) != 0 {
		t.Errorf("registers %+v, hooks %v: nothing may be written after a failed plan", h.nomad.registerCalls, h.hookIDs())
	}
	if got := h.get(d.ID); got.State != store.StatePreHook {
		t.Errorf("state = %s, want it left in pre_hook to retry", got.State)
	}
}

// Invariant 2. The register is made at the index detection captured, not at 0
// and not at what the live job says at that moment.
func TestARegisterUsesTheIndexCapturedAtDetection(t *testing.T) {
	setup := func(t *testing.T) (*harness, *store.Deployment, *api.Job) {
		h := newHarness(t)
		job := managed("web", "auto", nil)
		h.liveApplied("web", job, 7, 1) // the live job is at index 7, as at detection
		h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		return h, h.createApplying("web", job, 7), job
	}

	t.Run("the live job is where detection saw it", func(t *testing.T) {
		h, d, _ := setup(t)
		h.step(d)
		if len(h.nomad.registerCalls) != 1 || h.nomad.registerCalls[0].index != 7 {
			t.Fatalf("register calls = %+v, want one at the captured index 7", h.nomad.registerCalls)
		}
		if got := h.get(d.ID); got.State != store.StateApplying || got.AppliedIndex != 8 {
			t.Errorf("deployment = %s applied at %d, want applying at 8", got.State, got.AppliedIndex)
		}
	})

	t.Run("someone edits the job between the read and the register", func(t *testing.T) {
		h, d, job := setup(t)
		_, _, jn := h.journaled(0)
		jn.onPlan = func(id string) {
			if id == "web" {
				h.liveApplied("web", job, 8, 2) // an outside edit, after the plan
			}
		}

		h.step(d)

		got := h.get(d.ID)
		if got.State != store.StateFailed || !strings.Contains(got.Error, "job modified outside nops") {
			t.Errorf("deployment = %s %q, want failed: job modified outside nops", got.State, got.Error)
		}
		if len(h.nomad.registerCalls) != 0 {
			t.Errorf("registered %+v over an edit nops never saw", h.nomad.registerCalls)
		}
		if idx := derefUint64(h.nomad.live["web"].JobModifyIndex); idx != 8 {
			t.Errorf("live index = %d, want the outside edit's 8 untouched", idx)
		}
	})
}

// -- invariant 3 ---------------------------------------------------------------

// tickAfterWrites runs one apply step on the job's active deployment after
// every write of the store: the apply loop firing at the worst moment, between
// every two writes of detection.
type tickAfterWrites struct {
	Store
	tick   func()
	inTick bool
}

func (s *tickAfterWrites) run() {
	if s.inTick {
		return
	}
	s.inTick = true
	defer func() { s.inTick = false }()
	s.tick()
}

func (s *tickAfterWrites) CreateDeployment(ctx context.Context, d *store.Deployment) error {
	if err := s.Store.CreateDeployment(ctx, d); err != nil {
		return err
	}
	s.run()
	return nil
}

func (s *tickAfterWrites) Transition(ctx context.Context, id string, to store.State, t store.Transition) error {
	if err := s.Store.Transition(ctx, id, to, t); err != nil {
		return err
	}
	s.run()
	return nil
}

// Invariant 3. Whatever detection does to a job under approval (create the
// deployment, supersede it for a newer spec, for a changed hook, for a policy
// that came back), an apply step after every one of its writes never takes a
// deployment past waiting for a human: nothing is registered and no hook runs.
// Both for a deployment born pending_approval and for one an older nops left
// `detected`.
func TestAnApplyStepAfterEveryWriteOfDetectionNeverAppliesUnderApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(Store) Store
	}{
		{"born pending_approval", func(s Store) Store { return s }},
		{"left detected by an older nops", func(s Store) Store { return createdDetected{s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			var seen []string
			ticks := &tickAfterWrites{Store: tc.wrap(h.store)}
			ticks.tick = func() {
				d, err := h.store.ActiveDeployment(ctx, testNamespace, "web")
				if err != nil {
					return
				}
				h.engine.applyStep(ctx, d)
				after := h.get(d.ID)
				seen = append(seen, string(after.State))
				if after.State != store.StateDetected && after.State != store.StatePendingApproval {
					t.Errorf("an apply step took a deployment under approval to %s", after.State)
				}
			}
			h.engine.store = ticks

			extra := map[string]string{"nops_pre_hook": "backup", "nops_post_hook": "smoke"}
			h.nomad.setFile("backup-v1", hookJob("backup"))
			h.nomad.setFile("backup-v2", job("backup", map[string]string{"nops_role": "hook", "note": "2"}))
			h.nomad.setFile("smoke-v1", hookJob("smoke"))
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
			commit := func(sha, web, backup string) {
				h.snap.set(sha,
					gitwatch.File{Path: "web.nomad.hcl", Content: web},
					gitwatch.File{Path: "backup.nomad.hcl", Content: backup},
					gitwatch.File{Path: "smoke.nomad.hcl", Content: "smoke-v1"})
				if err := h.engine.Detect(ctx); err != nil {
					t.Fatalf("Detect at %s: %v", sha, err)
				}
			}
			h.nomad.setFile("web-v1", managed("web", "approval", extra))
			h.nomad.setFile("web-v2", managed("web", "approval", map[string]string{"nops_pre_hook": "backup", "nops_post_hook": "smoke", "note": "2"}))
			commit("c1", "web-v1", "backup-v1")
			commit("c2", "web-v2", "backup-v1") // a newer spec supersedes the first
			commit("c3", "web-v2", "backup-v2") // a changed hook supersedes the second

			if len(seen) < 3 {
				t.Fatalf("only %d apply steps ran between the writes of three cycles: %v", len(seen), seen)
			}
			if len(h.nomad.registerCalls) != 0 || len(h.hooks.calls) != 0 {
				t.Errorf("registers %+v, hooks %v: nothing may be written under approval", h.nomad.registerCalls, h.hookIDs())
			}
		})
	}
}

// -- invariant 4 ---------------------------------------------------------------

// Invariant 4. An invalid meta key reads the whole job as policy none, with an
// ERROR, even when the policy itself is a valid `auto`; and what the live job
// says about policy and hooks is never read.
func TestAnInvalidMetaKeyIsReadFromGitAsPolicyNoneWhateverTheLiveJobSays(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy string
		extra  map[string]string
		key    string
	}{
		{"a typo in the policy", "autoo", nil, "nops_policy"},
		{"a valid policy and another invalid key", "auto", map[string]string{"nops_notify_completed": "maybe"}, "nops_notify_completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var logs bytes.Buffer
			h.engine.log = slog.New(slog.NewTextHandler(&logs, nil))
			h.nomad.setFile("web-v1", managed("web", tc.policy, tc.extra))
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
			// The live job's own meta says auto and a pre-hook: stale, never read.
			live := managed("web", "auto", map[string]string{"nops_pre_hook": "ghost"})
			h.liveApplied("web", live, 5, 1)
			h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

			h.detect()

			h.noActive("web")
			obs := h.engine.Observations()
			if len(obs) != 1 || obs[0].Policy != meta.PolicyNone || !obs[0].Drift {
				t.Fatalf("observations = %+v, want web read as policy none, with its drift shown", obs)
			}
			if len(obs[0].PreHooks) != 0 {
				t.Errorf("pre-hooks = %+v: the live job's meta was read", obs[0].PreHooks)
			}
			var invalid bool
			for _, iss := range obs[0].Issues {
				invalid = invalid || (iss.Key == tc.key && iss.Severity == meta.SeverityError)
			}
			if !invalid {
				t.Errorf("issues = %+v, want an error on %s", obs[0].Issues, tc.key)
			}
			if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, tc.key) {
				t.Errorf("log = %q, want an ERROR naming %s", out, tc.key)
			}
		})
	}
}

func TestTheLivePolicyIsNeverWhatDecidesWhetherToDeploy(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.liveApplied("web", managed("web", "none", nil), 5, 1) // the live meta opts out
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	if d := h.active("web"); d.Policy != store.PolicyAuto || d.CASIndex != 5 {
		t.Errorf("deployment = %+v, want auto at index 5: git decides", d)
	}
}

// An invalid key introduced into a job that waits for approval closes it: the
// conservative reading is none.
func TestAnInvalidKeyIntroducedIntoAPendingApprovalSupersedesIt(t *testing.T) {
	for name, broken := range map[string]*api.Job{
		"a typo in the policy":       managed("web", "approvall", nil),
		"another key, policy intact": managed("web", "approval", map[string]string{"nops_notify_completed": "maybe"}),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.nomad.setFile("web-v1", managed("web", "approval", nil))
			h.nomad.setFile("web-v2", broken)
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
			h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
			h.detect()
			first := h.active("web")

			h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"})
			h.detect()

			if got := h.get(first.ID); got.State != store.StateSuperseded {
				t.Fatalf("deployment = %s, want superseded by the invalid meta", got.State)
			}
			h.noActive("web")
		})
	}
}

// -- invariant 5 ---------------------------------------------------------------

func metaOf(t *testing.T, spec string) map[string]string {
	t.Helper()
	var job api.Job
	if err := json.Unmarshal([]byte(spec), &job); err != nil {
		t.Fatal(err)
	}
	return job.Meta
}

// Invariant 5. What detection stores to register is the spec git has, meta
// included: nothing of nops's is added to it, whatever the live job carries.
func TestDetectionStoresTheSpecExactlyAsGitHasIt(t *testing.T) {
	h := newHarness(t)
	web := managed("web", "approval", map[string]string{"nops_pre_hook": "backup", "nops_post_hook": "smoke", "owner": "me"}, taskGroup("api", 2, true))
	backup := job("backup", map[string]string{"nops_role": "hook", "nops_timeout": "5m", "note": "x"})
	h.nomad.setFile("web-v1", web)
	h.nomad.setFile("backup-v1", backup)
	h.nomad.setFile("smoke-v1", hookJob("smoke"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	// A live job with stale meta and another count: the scaling count is taken
	// from it, nothing else is.
	h.liveApplied("web", managed("web", "auto", map[string]string{"stale": "1"}, taskGroup("api", 5, true)), 3, 1)
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "backup.nomad.hcl", Content: "backup-v1"},
		gitwatch.File{Path: "smoke.nomad.hcl", Content: "smoke-v1"})

	h.detect()

	d := h.active("web")
	if got := metaOf(t, d.JobSpec); !reflect.DeepEqual(got, web.Meta) {
		t.Errorf("stored meta = %v, want exactly git's %v", got, web.Meta)
	}
	frozen, err := h.store.DeploymentHooks(context.Background(), d.ID)
	if err != nil || len(frozen) != 2 {
		t.Fatalf("frozen hooks = %+v, %v", frozen, err)
	}
	if got := metaOf(t, frozen[0].JobSpec); !reflect.DeepEqual(got, backup.Meta) {
		t.Errorf("frozen hook meta = %v, want exactly git's %v", got, backup.Meta)
	}
}

// Invariant 5. A hook revision is registered with the meta of the hook as it
// was frozen, and nothing else.
func TestAHookRevisionIsRegisteredWithTheMetaOfItsSpec(t *testing.T) {
	h := newHarness(t)
	backup := job("backup", map[string]string{"nops_role": "hook", "nops_timeout": "5m", "note": "x"})
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{"nops_pre_hook": "backup"}))
	h.nomad.setFile("backup-v1", backup)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "backup.nomad.hcl", Content: "backup-v1"})
	h.detect()
	d := h.active("web")
	h.step(d)           // detected -> pre_hook
	h.step(h.get(d.ID)) // registers the revision, runs the hook
	frozen, err := h.store.DeploymentHooks(context.Background(), d.ID)
	if err != nil || len(frozen) != 1 {
		t.Fatalf("frozen hooks = %+v, %v", frozen, err)
	}

	live, ok := h.nomad.live[frozen[0].Revision]
	if !ok {
		t.Fatalf("the revision %s was not registered: %+v", frozen[0].Revision, h.nomad.registerCalls)
	}
	if !reflect.DeepEqual(live.Meta, backup.Meta) {
		t.Errorf("registered meta = %v, want exactly the hook's %v", live.Meta, backup.Meta)
	}
}

// -- invariant 6 ---------------------------------------------------------------

// lockedOut is a store where one job's lock is already held when detection
// tries to create its deployment.
type lockedOut struct {
	Store
	jobID string
}

func (s lockedOut) CreateDeployment(ctx context.Context, d *store.Deployment) error {
	if d.JobID == s.jobID {
		return fmt.Errorf("create deployment: %w: %s/%s", store.ErrActiveDeployment, d.Namespace, d.JobID)
	}
	return s.Store.CreateDeployment(ctx, d)
}

// Invariant 6. The database says a job already has an active deployment: that
// job is skipped for the cycle, with a WARN, and the others go on.
func TestALockHeldByTheDatabaseSkipsTheJobAndNotTheCycle(t *testing.T) {
	h := newHarness(t)
	var logs bytes.Buffer
	h.engine.log = slog.New(slog.NewTextHandler(&logs, nil))
	for _, id := range []string{"web", "db"} {
		h.nomad.setFile(id+"-v1", managed(id, "auto", nil))
		h.nomad.setDrift(id, &api.JobDiff{Type: "Edited", ID: id})
	}
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "db.nomad.hcl", Content: "db-v1"})
	h.engine.store = lockedOut{h.store, "web"}

	if err := h.engine.Detect(context.Background()); err != nil {
		t.Fatalf("Detect = %v, want the cycle to go on", err)
	}

	h.noActive("web")
	if d := h.active("db"); d.State != store.StateDetected {
		t.Errorf("db = %+v, want its deployment created", d)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "active deployment appeared concurrently") {
		t.Errorf("log = %q, want a WARN for the skipped job", out)
	}
}
