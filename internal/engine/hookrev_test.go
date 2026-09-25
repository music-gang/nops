package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// -- hash, revision and frozen hooks -----------------------------------------

func hookFiles(jobs ...*api.Job) map[string]parsedFile {
	m := make(map[string]parsedFile, len(jobs))
	for _, j := range jobs {
		m[*j.ID] = parsedFile{path: *j.ID + ".nomad.hcl", job: j, cfg: meta.Parse(j.Meta)}
	}
	return m
}

func cfgOf(extra map[string]string) meta.Config {
	return meta.Parse(managed("web", "auto", extra).Meta)
}

func TestFreezeHooksWithoutHooksKeepsTheTargetHash(t *testing.T) {
	frozen, hash, missing, err := freezeHooks(cfgOf(nil), nil, "target-hash")
	if err != nil || len(frozen) != 0 || missing != "" || hash != "target-hash" {
		t.Fatalf("frozen %+v hash %q missing %q err %v; want none and the target's own hash", frozen, hash, missing, err)
	}
}

func TestFreezeHooksFreezesEachDeclaredHook(t *testing.T) {
	pre, post := hookJob("web-migrate"), hookJob("web-smoke")
	cfg := cfgOf(map[string]string{"nops_pre_hook": "web-migrate", "nops_post_hook": "web-smoke"})

	frozen, hash, missing, err := freezeHooks(cfg, hookFiles(pre, post), "target-hash")
	if err != nil || missing != "" {
		t.Fatalf("missing %q err %v", missing, err)
	}
	if len(frozen) != 2 || frozen[0].Phase != "pre" || frozen[1].Phase != "post" {
		t.Fatalf("frozen = %+v, want pre then post", frozen)
	}
	preHash, _ := specHash(pre)
	if frozen[0].HookID != "web-migrate" || frozen[0].SpecHash != preHash ||
		frozen[0].Revision != "web-migrate-"+preHash[:8] {
		t.Errorf("pre = %+v, want the hook, its hash and the revision id-<8 hex>", frozen[0])
	}
	if !strings.Contains(frozen[0].JobSpec, `"web-migrate"`) {
		t.Errorf("pre spec = %s, want the hook as read from git", frozen[0].JobSpec)
	}
	if hash == "target-hash" || len(hash) != 64 {
		t.Errorf("combined hash = %q, want a new sha-256 that covers the hooks", hash)
	}
}

func TestFreezeHooksHashChangesWithWhatRuns(t *testing.T) {
	cfg := cfgOf(map[string]string{"nops_pre_hook": "web-migrate", "nops_post_hook": "web-smoke"})
	pre, post := hookJob("web-migrate"), hookJob("web-smoke")
	_, base, _, _ := freezeHooks(cfg, hookFiles(pre, post), "t")

	changed := hookJob("web-migrate")
	changed.Meta["extra"] = "1"
	_, afterHookChange, _, _ := freezeHooks(cfg, hookFiles(changed, post), "t")
	_, afterTargetChange, _, _ := freezeHooks(cfg, hookFiles(pre, post), "t2")
	_, again, _, _ := freezeHooks(cfg, hookFiles(pre, post), "t")

	if again != base {
		t.Errorf("the same inputs hash differently: %s / %s", base, again)
	}
	if afterHookChange == base {
		t.Error("a changed hook did not change the hash")
	}
	if afterTargetChange == base {
		t.Error("a changed target did not change the hash")
	}

	// The same two hooks in the other phases are another deployment.
	swapped := cfgOf(map[string]string{"nops_pre_hook": "web-smoke", "nops_post_hook": "web-migrate"})
	_, swappedHash, _, _ := freezeHooks(swapped, hookFiles(pre, post), "t")
	if swappedHash == base {
		t.Error("swapping the phases did not change the hash")
	}
}

func TestFreezeHooksNamesAMissingHookAndHashesItsAbsence(t *testing.T) {
	cfg := cfgOf(map[string]string{"nops_pre_hook": "web-migrate", "nops_post_hook": "web-smoke"})

	frozen, without, missing, err := freezeHooks(cfg, hookFiles(hookJob("web-smoke")), "t")
	if err != nil || missing != `pre-hook "web-migrate"` {
		t.Fatalf("missing = %q, err %v", missing, err)
	}
	if len(frozen) != 1 || frozen[0].HookID != "web-smoke" {
		t.Errorf("frozen = %+v, want only the hook that exists", frozen)
	}
	_, with, missing, _ := freezeHooks(cfg, hookFiles(hookJob("web-migrate"), hookJob("web-smoke")), "t")
	if missing != "" || with == without {
		t.Errorf("adding the missing hook: missing %q, hash equal %v; want it found and a different hash", missing, with == without)
	}
}

func TestRevisionJobKeepsTheHooksNameAndTakesTheRevisionID(t *testing.T) {
	hj := hookJob("web-migrate")
	frozen, _, _, _ := freezeHooks(cfgOf(map[string]string{"nops_pre_hook": "web-migrate"}), hookFiles(hj), "t")

	job, err := revisionJob(frozen[0])
	if err != nil {
		t.Fatal(err)
	}
	if *job.ID != frozen[0].Revision || job.Name == nil || *job.Name != "web-migrate" {
		t.Errorf("job ID %q name %v; want the revision as ID and the hook's own name", *job.ID, job.Name)
	}

	named := hookJob("web-migrate")
	name := "Migrate the schema"
	named.Name = &name
	frozen, _, _, _ = freezeHooks(cfgOf(map[string]string{"nops_pre_hook": "web-migrate"}), hookFiles(named), "t")
	if job, _ = revisionJob(frozen[0]); *job.Name != name {
		t.Errorf("name = %q, want the hook's explicit name kept", *job.Name)
	}

	if _, err := revisionJob(store.DeploymentHook{HookID: "x", JobSpec: "not json"}); err == nil {
		t.Error("a corrupt spec must be an error")
	}
}

// -- detection ------------------------------------------------------------------

// hookedWeb sets up a snapshot with `web` (declaring the pre-hook web-migrate,
// drifting) and the hook file, whose content is hookContent.
func hookedWeb(h *harness, policy, commit, hookContent string, hj *api.Job) {
	h.nomad.setFile("web-v1", managed("web", policy, map[string]string{"nops_pre_hook": "web-migrate"}))
	h.nomad.setFile(hookContent, hj)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set(commit,
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "web-migrate.nomad.hcl", Content: hookContent})
}

func withMeta(j *api.Job, k, v string) *api.Job { j.Meta[k] = v; return j }

func TestDeploymentHashCoversTheHooks(t *testing.T) {
	h := newHarness(t)
	hookedWeb(h, "approval", "c1", "hook-v1", hookJob("web-migrate"))
	h.detect()
	first := h.active("web")

	_, want, _, _ := freezeHooks(cfgOf(map[string]string{"nops_pre_hook": "web-migrate"}),
		hookFiles(hookJob("web-migrate")), mustSpecHash(t, managed("web", "approval", map[string]string{"nops_pre_hook": "web-migrate"})))
	if first.SpecHash != want {
		t.Errorf("spec_hash = %s, want the target's and the hook's combined (%s)", first.SpecHash, want)
	}
}

func mustSpecHash(t *testing.T, j *api.Job) string {
	t.Helper()
	s, err := specHash(j)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestChangedHookSupersedesAPendingDeployment(t *testing.T) {
	h := newHarness(t)
	hookedWeb(h, "approval", "c1", "hook-v1", hookJob("web-migrate"))
	h.detect()
	first := h.active("web")
	if first.State != store.StatePendingApproval {
		t.Fatalf("setup: %+v", first)
	}

	// Only the hook changes: the target file is the same content.
	hookedWeb(h, "approval", "c2", "hook-v2", withMeta(hookJob("web-migrate"), "note", "v2"))
	h.detect()

	old := h.get(first.ID)
	if old.State != store.StateSuperseded {
		t.Fatalf("the approved-to-be deployment = %s, want superseded: the approval covered the old hook", old.State)
	}
	next := h.active("web")
	if next.ID == first.ID || next.State != store.StatePendingApproval || next.SpecHash == first.SpecHash {
		t.Fatalf("new deployment = %+v, want a new one waiting for approval", next)
	}
	frozen, _ := h.store.DeploymentHooks(context.Background(), next.ID)
	if len(frozen) != 1 || frozen[0].Revision == "" || !strings.Contains(frozen[0].JobSpec, "v2") {
		t.Errorf("frozen = %+v, want the new hook", frozen)
	}
}

func TestChangedHookAloneNeverCreatesADeploymentWithoutDrift(t *testing.T) {
	h := newHarness(t)
	hookedWeb(h, "auto", "c1", "hook-v1", hookJob("web-migrate"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "None", ID: "web"}) // target in sync
	h.detect()
	h.noActive("web")

	hookedWeb(h, "auto", "c2", "hook-v2", withMeta(hookJob("web-migrate"), "note", "v2"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "None", ID: "web"})
	h.detect()

	h.noActive("web")
	if _, err := h.store.LatestDeployment(context.Background(), testNamespace, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a hook change with no drift created a deployment (err %v)", err)
	}
}

func TestFixingAMissingHookUnblocksTheJobWithoutARetry(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"}))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	failed := h.latest("web")
	if failed.State != store.StateFailed {
		t.Fatalf("setup: %+v", failed)
	}
	h.detect()
	if blocked := h.engine.Observations()[0]; blocked.BlockedBy != failed.ID {
		t.Fatalf("setup: not blocked (%+v)", blocked)
	}

	// The hook file lands; the target's file, and its own hash, do not change.
	h.nomad.setFile("hook-v1", hookJob("web-migrate"))
	h.snap.set("c2",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "web-migrate.nomad.hcl", Content: "hook-v1"})
	h.detect()

	next := h.active("web")
	if next.ID == failed.ID || next.State != store.StateDetected {
		t.Fatalf("after the hook appeared: %+v, want a new deployment", next)
	}
	if obs := h.engine.Observations()[0]; obs.BlockedBy != "" {
		t.Errorf("still blocked by %s", obs.BlockedBy)
	}
}

func TestTwoJobsSharingAHookFreezeTheSameRevision(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{"nops_pre_hook": "migrate"}))
	h.nomad.setFile("api-v1", managed("api", "auto", map[string]string{"nops_pre_hook": "migrate"}))
	h.nomad.setFile("hook-v1", hookJob("migrate"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.nomad.setDrift("api", &api.JobDiff{Type: "Edited", ID: "api"})
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "api.nomad.hcl", Content: "api-v1"},
		gitwatch.File{Path: "migrate.nomad.hcl", Content: "hook-v1"})

	h.detect()

	a, _ := h.store.DeploymentHooks(context.Background(), h.active("web").ID)
	b, _ := h.store.DeploymentHooks(context.Background(), h.active("api").ID)
	if len(a) != 1 || len(b) != 1 || a[0].Revision != b[0].Revision {
		t.Fatalf("revisions %+v / %+v, want one shared", a, b)
	}
}

// -- registration at dispatch --------------------------------------------------

// preHooked creates a deployment of web in pre_hook whose hook is a plain
// hook job, as detection would have frozen it.
func (h *harness) preHooked() *store.Deployment {
	h.t.Helper()
	job := managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"})
	d := h.newDeployment("web", job, store.PolicyAuto, 0)
	return h.move(d, store.StatePreHook)
}

func (h *harness) revisionRegisters() []registerCall {
	var out []registerCall
	for _, c := range h.nomad.registerCalls {
		if c.id != "web" {
			out = append(out, c)
		}
	}
	return out
}

func TestStepHookRegistersTheRevisionRightBeforeDispatch(t *testing.T) {
	h := newHarness(t)
	d := h.preHooked()
	rev := revisionOfPlainHook("web-migrate")
	if _, ok := h.nomad.live[rev]; ok {
		t.Fatal("setup: the revision is registered before the hook step")
	}

	h.step(d)

	if regs := h.revisionRegisters(); len(regs) != 1 || regs[0].id != rev || regs[0].index != 0 {
		t.Fatalf("registered %+v, want the revision once, at index 0 (it must not exist)", h.nomad.registerCalls)
	}
	live := h.nomad.live[rev]
	if live == nil || *live.ID != rev || *live.Name != "web-migrate" {
		t.Fatalf("live revision = %+v, want the revision as ID and the hook's own name", live)
	}
	if calls := h.hooks.calls; len(calls) != 1 || calls[0].HookJobID != rev {
		t.Fatalf("hook calls = %+v, want a dispatch of the revision", calls)
	}
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Errorf("state = %s, want applying", got.State)
	}
}

func TestStepHookLeavesTheHookUnderItsPlainIDAlone(t *testing.T) {
	h := newHarness(t)
	old := hookJob("web-migrate")
	idx := uint64(7)
	old.JobModifyIndex = &idx
	h.nomad.setLive(old) // an old fixed-ID hook, or one registered by hand
	d := h.preHooked()

	h.step(d)

	for _, c := range h.nomad.registerCalls {
		if c.id == "web-migrate" {
			t.Errorf("registered the plain hook ID: %+v", c)
		}
	}
	if len(h.nomad.stopCalls) != 0 {
		t.Errorf("stopped %v, want nothing", h.nomad.stopCalls)
	}
}

func TestStepHookSkipsARevisionThatIsAlreadyRegistered(t *testing.T) {
	h := newHarness(t)
	rev := revisionOfPlainHook("web-migrate")
	idx := uint64(4)
	h.nomad.setLive(&api.Job{ID: &rev, JobModifyIndex: &idx, Meta: map[string]string{"nops_role": "hook"}}) // Plan: None
	d := h.preHooked()

	h.step(d)

	if regs := h.revisionRegisters(); len(regs) != 0 {
		t.Fatalf("registered %+v, want nothing: the plan shows no change", regs)
	}
	if len(h.hooks.calls) != 1 {
		t.Errorf("hook calls = %+v, want the dispatch", h.hooks.calls)
	}
}

func TestStepHookRegistersAStoppedRevisionAgainAtItsOwnIndex(t *testing.T) {
	h := newHarness(t)
	rev := revisionOfPlainHook("web-migrate")
	idx, stop := uint64(9), true
	h.nomad.setLive(&api.Job{ID: &rev, JobModifyIndex: &idx, Stop: &stop, Meta: map[string]string{"nops_role": "hook"}}) // GC'd earlier
	d := h.preHooked()

	h.step(d)

	if regs := h.revisionRegisters(); len(regs) != 1 || regs[0].id != rev || regs[0].index != 9 {
		t.Fatalf("registered %+v, want the revision once at its live index 9 (a stopped job still exists)", h.nomad.registerCalls)
	}
}

func TestStepHookRegisterFailureRetriesThenFailsAtTheTimeout(t *testing.T) {
	h := newHarness(t)
	rev := revisionOfPlainHook("web-migrate")
	h.nomad.registerErr[rev] = errors.New("nomad said no")
	d := h.preHooked()

	h.step(d)
	if got := h.get(d.ID); got.State != store.StatePreHook || len(h.hooks.calls) != 0 {
		t.Fatalf("after a failed register: state %s, hook calls %+v; want pre_hook and no dispatch", got.State, h.hooks.calls)
	}

	h.clock.Advance(meta.DefaultHookTimeout + time.Second)
	h.step(h.get(d.ID))

	got := h.get(d.ID)
	if got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed once the hook's timeout has passed", got.State)
	}
	for _, want := range []string{"pre-hook", "web-migrate", "nomad said no", "live job left untouched"} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("error = %q, want it to say %q", got.Error, want)
		}
	}
}

func TestStepHookWithoutAFrozenHookFailsTheDeployment(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"})
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1", JobSpec: specJSON, Policy: store.PolicyAuto} // no Hooks
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	d = h.move(d, store.StatePreHook)

	h.step(d)

	got := h.get(d.ID)
	if got.State != store.StateFailed || !strings.Contains(got.Error, "no frozen hook") {
		t.Fatalf("deployment = %s %q, want failed and why", got.State, got.Error)
	}
	if len(h.hooks.calls) != 0 {
		t.Errorf("hook calls = %+v, want none", h.hooks.calls)
	}
}

func TestStepHookRunsTheFrozenSpecNotWhatGitHasNow(t *testing.T) {
	h := newHarness(t)
	d := h.preHooked() // froze a plain hook job
	// git has moved on: the snapshot now holds another version of the hook.
	h.nomad.setFile("hook-v2", withMeta(hookJob("web-migrate"), "note", "v2"))
	h.snap.set("c9", gitwatch.File{Path: "web-migrate.nomad.hcl", Content: "hook-v2"})

	h.step(d)

	live := h.nomad.live[revisionOfPlainHook("web-migrate")]
	if live == nil || live.Meta["note"] != "" {
		t.Fatalf("registered %+v, want the frozen spec (no note)", live)
	}
}

func TestStepPostHookRegistersItsRevisionToo(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", map[string]string{"nops_post_hook": "web-smoke"})
	d := h.applyingWithIndex("web", job, 9)
	d = h.move(d, store.StatePostHook)
	h.hooks.setResult(d.ID, "post", hooks.Result{State: store.HookSucceeded})

	h.step(d)

	rev := revisionOfPlainHook("web-smoke")
	if _, ok := h.nomad.live[rev]; !ok {
		t.Fatalf("the post-hook revision %s was not registered: %+v", rev, h.nomad.registerCalls)
	}
	if got := h.get(d.ID); got.State != store.StateCompleted {
		t.Errorf("state = %s, want completed", got.State)
	}
}

// -- GC ---------------------------------------------------------------------------

func liveHook(h *harness, id string) {
	idx := uint64(3)
	h.nomad.setLive(&api.Job{ID: &id, JobModifyIndex: &idx, Meta: map[string]string{"nops_role": "hook"}})
}

func (h *harness) gc() {
	h.t.Helper()
	if err := h.engine.gcHookRevisions(context.Background()); err != nil {
		h.t.Fatalf("gcHookRevisions: %v", err)
	}
}

func TestGCDeregistersARevisionNothingUses(t *testing.T) {
	h := newHarness(t)
	liveHook(h, "web-migrate-0a1b2c3d")

	h.gc()

	if got := h.nomad.stopCalls; len(got) != 1 || got[0] != "web-migrate-0a1b2c3d" {
		t.Fatalf("stopped %v, want the unused revision", got)
	}
	// Stopped, not purged: the job is still there, and a second pass leaves it.
	if _, ok := h.nomad.live["web-migrate-0a1b2c3d"]; !ok {
		t.Error("the revision was purged")
	}
	h.gc()
	if len(h.nomad.stopCalls) != 1 {
		t.Errorf("a stopped revision was stopped again: %v", h.nomad.stopCalls)
	}
}

func TestGCKeepsTheRevisionsOfNonTerminalDeployments(t *testing.T) {
	h := newHarness(t)
	d := h.preHooked() // pre_hook
	rev := revisionOfPlainHook("web-migrate")
	liveHook(h, rev)
	liveHook(h, "web-migrate-ffffffff")

	h.gc()
	if got := h.nomad.stopCalls; len(got) != 1 || got[0] != "web-migrate-ffffffff" {
		t.Fatalf("stopped %v, want only the revision nobody uses", got)
	}

	// Once the deployment is over the revision goes too.
	if err := h.store.Transition(context.Background(), d.ID, store.StateFailed,
		store.Transition{From: store.StatePreHook, Actor: "nops", Message: "x"}); err != nil {
		t.Fatal(err)
	}
	h.gc()
	if got := h.nomad.stopCalls; len(got) != 2 || got[1] != rev {
		t.Errorf("stopped %v, want the revision once its deployment ended", got)
	}
}

func TestGCKeepsARevisionAPendingDeploymentWillNeed(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "approval", map[string]string{"nops_pre_hook": "web-migrate"})
	specJSON := mustMarshal(t, job)
	d := &store.Deployment{JobID: "web", Namespace: testNamespace, CommitSHA: "c1", SpecHash: "h1", JobSpec: specJSON,
		Hooks: frozenFromSpec(specJSON), Policy: store.PolicyApproval}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	liveHook(h, revisionOfPlainHook("web-migrate"))

	h.gc()

	if len(h.nomad.stopCalls) != 0 {
		t.Errorf("stopped %v, want nothing: a detected deployment still needs it", h.nomad.stopCalls)
	}
}

func TestGCOnlyTouchesHookRevisions(t *testing.T) {
	h := newHarness(t)
	idx := uint64(3)
	parent := "web-migrate-0a1b2c3d"
	stopped := true
	for _, j := range []*api.Job{
		{ID: ptr("web-0a1b2c3d"), JobModifyIndex: &idx, Meta: map[string]string{"nops_managed": "true"}},                                           // shaped like a revision, not a hook
		{ID: ptr("plain-hook"), JobModifyIndex: &idx, Meta: map[string]string{"nops_role": "hook"}},                                                // a hook under its plain ID
		{ID: ptr("hook-0a1b2c3"), JobModifyIndex: &idx, Meta: map[string]string{"nops_role": "hook"}},                                              // 7 hex
		{ID: ptr("hook-0A1B2C3D"), JobModifyIndex: &idx, Meta: map[string]string{"nops_role": "hook"}},                                             // not lower-case hex
		{ID: ptr(parent + "/dispatch-1790330374-2dbd0404"), ParentID: &parent, JobModifyIndex: &idx, Meta: map[string]string{"nops_role": "hook"}}, // a dispatched child
		{ID: ptr("old-0a1b2c3d"), JobModifyIndex: &idx, Stop: &stopped, Meta: map[string]string{"nops_role": "hook"}},                              // already stopped
		{ID: ptr("no-meta-0a1b2c3d"), JobModifyIndex: &idx},
	} {
		h.nomad.setLive(j)
	}

	h.gc()

	if len(h.nomad.stopCalls) != 0 {
		t.Fatalf("stopped %v, want nothing", h.nomad.stopCalls)
	}
}

func ptr[T any](v T) *T { return &v }

func TestGCFailureIsAnErrorNotAFailedCycle(t *testing.T) {
	h := newHarness(t)
	liveHook(h, "a-0a1b2c3d")
	liveHook(h, "b-0a1b2c3d")
	h.nomad.stopErr["a-0a1b2c3d"] = errors.New("nomad said no")

	h.gc() // must not fail

	if got := h.nomad.stopCalls; len(got) != 1 || got[0] != "b-0a1b2c3d" {
		t.Fatalf("stopped %v, want b after a's failure", got)
	}
	h.nomad.stopErr = map[string]error{}
	h.gc()
	if got := h.nomad.stopCalls; len(got) != 2 || got[1] != "a-0a1b2c3d" {
		t.Errorf("stopped %v, want a retried on the next pass", got)
	}

	h.nomad.listErr = errors.New("nomad is down")
	h.gc() // a listing failure neither panics nor fails
}

func TestGCStoreFailureIsReturned(t *testing.T) {
	h := newHarness(t)
	liveHook(h, "a-0a1b2c3d")
	h.engine.store = failingRevisions{Store: h.engine.store}

	if err := h.engine.gcHookRevisions(context.Background()); err == nil {
		t.Fatal("a store failure must be returned")
	}
	if len(h.nomad.stopCalls) != 0 {
		t.Errorf("stopped %v without knowing what is in use", h.nomad.stopCalls)
	}
}

type failingRevisions struct{ Store }

func (failingRevisions) HookRevisionsInUse(context.Context, string) ([]string, error) {
	return nil, errors.New("disk on fire")
}

func TestDetectCycleRunsTheGC(t *testing.T) {
	h := newHarness(t)
	liveHook(h, "web-migrate-0a1b2c3d")
	h.snap.set("c1")

	h.detect()

	if len(h.nomad.stopCalls) != 1 {
		t.Fatalf("stopped %v, want the unused revision deregistered by the cycle", h.nomad.stopCalls)
	}
}
