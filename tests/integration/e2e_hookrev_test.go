//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/store"
)

// A hook is frozen with the deployment and registered in Nomad only right
// before it is dispatched, under an ID made of its own and the hash of its
// spec (a hook revision). These check it with the real binary: nothing of a
// hook is in Nomad before approval, what runs is what was approved, and a
// revision nothing needs any more is deregistered without being purged.

// hookJobs lists the jobs in Nomad whose ID starts with hookID: the hook's
// revisions, and the children dispatched from them.
func (e *e2eEnv) hookJobs(hookID string) []*api.JobListStub {
	e.t.Helper()
	stubs, _, err := e.raw.Jobs().PrefixList(hookID)
	if err != nil {
		e.t.Fatal(err)
	}
	return stubs
}

func (e *e2eEnv) frozenHooks(id string) []store.DeploymentHook {
	e.t.Helper()
	got, err := e.st.DeploymentHooks(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// waitStopped waits until the job exists in Nomad and is stopped.
func (e *e2eEnv) waitStopped(id string) *api.Job {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		j, _, err := e.raw.Jobs().Info(id, nil)
		if err == nil && j.Stop != nil && *j.Stop {
			return j
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %s was not stopped in time (err %v)", id, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestE2EHookRevisionLifecycle(t *testing.T) {
	e := newE2E(t)
	dir := t.TempDir()
	jobID := uniqueID(t, e.raw, "rev")
	hookID := uniqueID(t, e.raw, "revhook")

	e.repo.commit(t, "job "+jobID, map[string]string{
		file(jobID):  e2eJob{id: jobID, policy: "approval", version: "1", preHook: hookID}.hcl(),
		file(hookID): traceHook(hookID, dir),
	})
	d := e.waitNew(jobID, "", store.StatePendingApproval)

	// Frozen with the deployment, not in Nomad while it waits for a human.
	frozen := e.frozenHooks(d.ID)
	if len(frozen) != 1 || frozen[0].HookID != hookID || !strings.HasPrefix(frozen[0].Revision, hookID+"-") {
		t.Fatalf("frozen hooks = %+v, want the hook at a revision", frozen)
	}
	revision := frozen[0].Revision
	if jobs := e.hookJobs(hookID); len(jobs) != 0 {
		t.Fatalf("%d jobs for the hook in Nomad before approval, want none", len(jobs))
	}

	if status := e.dash.approve(t, d.ID, d.SpecHash); status != 303 {
		t.Fatalf("approve: status %d, want 303", status)
	}
	e.waitState(d.ID, store.StateCompleted)

	// The revision was registered under its own ID, with the hook's name, and
	// the hook that ran is the one dispatched from it.
	rev, _, err := e.raw.Jobs().Info(revision, nil)
	if err != nil {
		t.Fatalf("the revision %s was not registered: %v", revision, err)
	}
	if rev.Name == nil || *rev.Name != hookID {
		t.Errorf("revision name = %v, want the hook's own %q", rev.Name, hookID)
	}
	run, err := e.st.GetHookRun(context.Background(), d.ID, "pre")
	if err != nil || run.HookJobID != revision || !strings.HasPrefix(run.DispatchedJobID, revision+"/dispatch-") {
		t.Fatalf("hook run = %+v, err %v; want it dispatched from the revision", run, err)
	}
	if _, err := os.Stat(filepath.Join(dir, d.ID+".dump")); err != nil {
		t.Errorf("the hook did not run for this deployment: %v", err)
	}
	// Nothing was ever registered under the hook's plain ID.
	if _, _, err := e.raw.Jobs().Info(hookID, nil); err == nil {
		t.Errorf("the hook was registered under its plain ID %s", hookID)
	}

	// Nothing needs the revision now: it is deregistered, not purged, and the
	// run it dispatched can still be read.
	stopped := e.waitStopped(revision)
	if stopped.Meta["nops_role"] != "hook" {
		t.Errorf("stopped revision meta = %v", stopped.Meta)
	}
	if _, _, err := e.raw.Jobs().Info(run.DispatchedJobID, nil); err != nil {
		t.Errorf("the dispatched run %s is gone once its revision was stopped: %v", run.DispatchedJobID, err)
	}
}

func TestE2EChangedHookMustBeApprovedAgain(t *testing.T) {
	e := newE2E(t)
	dir := t.TempDir()
	jobID := uniqueID(t, e.raw, "revchg")
	hookID := uniqueID(t, e.raw, "revchghook")

	push := func(msg, cmd string) {
		e.repo.commit(t, msg, map[string]string{
			file(jobID):  e2eJob{id: jobID, policy: "approval", version: "1", preHook: hookID}.hcl(),
			file(hookID): hookCmdHCL(hookID, cmd, true),
		})
	}
	logFile := filepath.Join(dir, "ran.log")

	push("hook v1", "echo v1 >> "+logFile)
	d1 := e.waitNew(jobID, "", store.StatePendingApproval)
	rev1 := e.frozenHooks(d1.ID)[0].Revision

	// Only the hook changes: the job's file is byte for byte the same. What
	// was to be approved is not what will run any more.
	push("hook v2", "echo v2 >> "+logFile)
	e.waitState(d1.ID, store.StateSuperseded)
	d2 := e.waitNew(jobID, d1.ID, store.StatePendingApproval)
	rev2 := e.frozenHooks(d2.ID)[0].Revision
	if rev1 == rev2 || d1.SpecHash == d2.SpecHash {
		t.Fatalf("revisions %s / %s, hashes %s / %s; want both to differ", rev1, rev2, d1.SpecHash, d2.SpecHash)
	}
	// The approval of the superseded one no longer means anything.
	if status := e.dash.approve(t, d1.ID, d1.SpecHash); status == 303 && e.deployment(d1.ID).State != store.StateSuperseded {
		t.Errorf("approving the superseded deployment moved it to %s", e.deployment(d1.ID).State)
	}

	if status := e.dash.approve(t, d2.ID, d2.SpecHash); status != 303 {
		t.Fatalf("approve: status %d, want 303", status)
	}
	e.waitState(d2.ID, store.StateCompleted)

	got, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2\n" {
		t.Errorf("ran.log = %q, want only the approved v2", got)
	}
	// The old revision never reached Nomad.
	if _, _, err := e.raw.Jobs().Info(rev1, nil); err == nil {
		t.Errorf("the superseded revision %s was registered", rev1)
	}
}
