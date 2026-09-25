//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// The pre-pull and backup scenarios differ from a real cluster only in what
// the hook does: here it appends to a log and writes a marker file instead of
// pulling an image or dumping a database. What nops has to guarantee is the
// same, and is checked with the real binary: the pre-hook runs before the
// register and gets the deployment ID, a failing or slow hook leaves the live
// job alone, and a failure is not retried in a loop. This relies on the Nomad
// agent running on the same host as the tests (true for `nomad agent -dev`),
// since the hooks write to a test directory.

// traceHook is a hook that records that it ran, in order, and leaves a file
// named after the deployment it was dispatched for (what a backup hook does
// with its dump).
func traceHook(id, dir string) string {
	return hookCmdHCL(id,
		"echo hook >> "+filepath.Join(dir, "order.log")+
			" && touch "+filepath.Join(dir, "$NOMAD_META_nops_deployment_id")+".dump",
		true)
}

func TestE2EPreHookRunsBeforeApply(t *testing.T) {
	e := newE2E(t)
	dir := t.TempDir()
	jobID := uniqueID(t, e.raw, "pre")
	hookID := uniqueID(t, e.raw, "prehook")

	push := func(version string) {
		j := e2eJob{
			id: jobID, policy: "auto", version: version, preHook: hookID,
			cmd: "echo target >> " + filepath.Join(dir, "order.log"),
		}
		e.repo.commit(t, "job "+jobID+" v"+version, map[string]string{
			file(jobID):  j.hcl(),
			file(hookID): traceHook(hookID, dir),
		})
	}

	// A create, then an update: both go through the pre-hook.
	push("1")
	d1 := e.waitNew(jobID, "", store.StateCompleted)
	push("2")
	d2 := e.waitNew(jobID, d1.ID, store.StateCompleted)

	got, err := os.ReadFile(filepath.Join(dir, "order.log"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "hook\ntarget\nhook\ntarget\n"; string(got) != want {
		t.Errorf("order.log = %q, want %q: the pre-hook must run before the job is registered, every time", got, want)
	}
	for _, d := range []*store.Deployment{d1, d2} {
		if _, err := os.Stat(filepath.Join(dir, d.ID+".dump")); err != nil {
			t.Errorf("no dump named after deployment %s: %v", d.ID, err)
		}
		states := e.states(d.ID)
		if !containsInOrder(states, store.StatePreHook, store.StateApplying, store.StateCompleted) {
			t.Errorf("states of %s = %v, want pre_hook → applying → completed", d.ID, states)
		}
	}
	if got := e.liveVersion(jobID); got != "2" {
		t.Errorf("live version = %q, want 2", got)
	}
}

func TestE2EPreHookFailureLeavesJobUntouched(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "prefail")
	hookID := uniqueID(t, e.raw, "prefailhook")

	push := func(version, hookCmd string) {
		e.repo.commit(t, "job "+jobID+" v"+version, map[string]string{
			file(jobID):  e2eJob{id: jobID, policy: "auto", version: version, preHook: hookID}.hcl(),
			file(hookID): hookCmdHCL(hookID, hookCmd, true),
		})
	}

	// A first deployment that goes through, so there is a live job to protect.
	push("1", "exit 0")
	d1 := e.waitNew(jobID, "", store.StateCompleted)
	indexBefore := e.liveIndex(jobID)

	// The next one has a failing backup: the job must stay as it is.
	push("2", "exit 1")
	d2 := e.waitNew(jobID, d1.ID, store.StateFailed)
	if d2.AppliedIndex != 0 {
		t.Errorf("applied_index = %d: the register was reached although the pre-hook failed", d2.AppliedIndex)
	}
	if got := e.liveIndex(jobID); got != indexBefore {
		t.Errorf("live index moved %d → %d: the job was touched", indexBefore, got)
	}
	if got := e.liveVersion(jobID); got != "1" {
		t.Errorf("live version = %q, want the previous 1", got)
	}

	// The drift is still there, but the same failure is not retried on every
	// cycle: give detection several cycles, then count the deployments.
	time.Sleep(2 * time.Second)
	if got := e.deploymentsOf(jobID); got != 2 {
		t.Errorf("%d deployments for %s, want 2 (the completed one and the failed one, no retry loop)", got, jobID)
	}
	if got := e.latest(jobID); got.ID != d2.ID {
		t.Errorf("latest deployment = %s, want the failed %s", got.ID, d2.ID)
	}
}

func TestE2EPreHookTimeout(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "pretimeout")
	hookID := uniqueID(t, e.raw, "pretimeouthook")

	e.repo.commit(t, "job "+jobID, map[string]string{
		file(jobID):  e2eJob{id: jobID, policy: "auto", version: "1", preHook: hookID}.hcl(),
		file(hookID): hookWithTimeoutHCL(hookID, "sleep 600", "2s"),
	})
	d := e.waitNew(jobID, "", store.StateFailed)

	run, err := e.st.GetHookRun(context.Background(), d.ID, "pre", 0)
	if err != nil {
		t.Fatalf("GetHookRun: %v", err)
	}
	if run.State != store.HookTimedOut {
		t.Errorf("hook run state = %s, want timed_out (error %q)", run.State, run.Error)
	}
	if got := e.liveIndex(jobID); got != 0 {
		t.Errorf("job %s was registered (index %d) although its pre-hook timed out", jobID, got)
	}

	// The dispatched job is stopped but still there to look at, not purged,
	// under the revision of the hook the deployment froze.
	frozen, err := e.st.DeploymentHooks(context.Background(), d.ID)
	if err != nil || len(frozen) != 1 {
		t.Fatalf("frozen hooks = %+v, err %v", frozen, err)
	}
	stubs, _, err := e.raw.Jobs().PrefixList(frozen[0].Revision + "/dispatch-")
	if err != nil {
		t.Fatal(err)
	}
	if len(stubs) != 1 {
		t.Fatalf("%d dispatched jobs for %s, want 1", len(stubs), frozen[0].Revision)
	}
	child, _, err := e.raw.Jobs().Info(stubs[0].ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if child.Stop == nil || !*child.Stop {
		t.Errorf("dispatched job %s was not stopped after the timeout (status %q)", stubs[0].ID, stubs[0].Status)
	}
}

// containsInOrder reports whether want appears in got as a subsequence.
func containsInOrder(got []store.State, want ...store.State) bool {
	i := 0
	for _, s := range got {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	return i == len(want)
}
