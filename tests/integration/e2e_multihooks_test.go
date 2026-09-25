//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/store"
)

// logHook is a hook that appends its name to a shared log, so the order the
// hooks ran in can be read back.
func logHook(id, name, dir string) string {
	return hookCmdHCL(id, "echo "+name+" >> "+filepath.Join(dir, "order.log"), true)
}

func TestE2ESeveralHooksRunInOrderAroundTheApply(t *testing.T) {
	e := newE2E(t)
	dir := t.TempDir()
	jobID := uniqueID(t, e.raw, "multi")
	backup := uniqueID(t, e.raw, "backup")
	migrate := uniqueID(t, e.raw, "migrate")
	smoke := uniqueID(t, e.raw, "smoke")
	notify := uniqueID(t, e.raw, "notify")

	e.repo.commit(t, "job "+jobID, map[string]string{
		file(jobID): e2eJob{
			id: jobID, policy: "auto", version: "1",
			preHook: backup + ", " + migrate, postHook: smoke + "," + notify,
			cmd: "echo target >> " + filepath.Join(dir, "order.log"),
		}.hcl(),
		file(backup):  logHook(backup, "backup", dir),
		file(migrate): logHook(migrate, "migrate", dir),
		file(smoke):   logHook(smoke, "smoke", dir),
		file(notify):  logHook(notify, "notify", dir),
	})
	d := e.waitNew(jobID, "", store.StateCompleted)

	got, err := os.ReadFile(filepath.Join(dir, "order.log"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "backup\nmigrate\ntarget\nsmoke\nnotify\n"; string(got) != want {
		t.Errorf("order.log = %q, want %q: the pre-hooks in the order they are listed, then the job, then the post-hooks", got, want)
	}
	if states := e.states(d.ID); !slices.Equal(states, []store.State{store.StateDetected, store.StatePreHook, store.StateApplying, store.StatePostHook, store.StateCompleted}) {
		t.Errorf("states = %v", states)
	}

	// One run per hook, by phase and position, each on its own revision.
	runs, err := e.st.ListHookRuns(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, r := range runs {
		if r.State != store.HookSucceeded {
			t.Errorf("run %s/%d = %s", r.Phase, r.Position, r.State)
		}
		seen = append(seen, r.Phase+"/"+strings.Split(r.HookJobID, "-")[2])
	}
	if want := []string{"pre/backup", "pre/migrate", "post/smoke", "post/notify"}; !slices.Equal(seen, want) {
		t.Errorf("runs = %v, want %v", seen, want)
	}
	for _, r := range runs {
		if !strings.HasPrefix(r.DispatchedJobID, r.HookJobID+"/dispatch-") {
			t.Errorf("run %s/%d dispatched %q, want a run of its revision %s", r.Phase, r.Position, r.DispatchedJobID, r.HookJobID)
		}
	}
}

func TestE2EAFailingFirstPreHookKeepsTheSecondFromRunning(t *testing.T) {
	e := newE2E(t)
	dir := t.TempDir()
	jobID := uniqueID(t, e.raw, "multifail")
	backup := uniqueID(t, e.raw, "failbackup")
	migrate := uniqueID(t, e.raw, "failmigrate")

	e.repo.commit(t, "job "+jobID, map[string]string{
		file(jobID):   e2eJob{id: jobID, policy: "auto", version: "1", preHook: backup + "," + migrate}.hcl(),
		file(backup):  hookCmdHCL(backup, "exit 1", true),
		file(migrate): logHook(migrate, "migrate", dir),
	})
	d := e.waitNew(jobID, "", store.StateFailed)

	if !strings.Contains(d.Error, "pre-hook "+backup) || !strings.Contains(d.Error, "live job left untouched") {
		t.Errorf("error = %q, want it to name the backup hook and that the job was left alone", d.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "order.log")); err == nil {
		t.Error("the second hook ran although the first failed")
	}
	// It was not even registered in Nomad, let alone dispatched.
	if jobs := e.hookJobs(migrate); len(jobs) != 0 {
		t.Errorf("%d jobs for the second hook in Nomad, want none", len(jobs))
	}
	if got := e.liveIndex(jobID); got != 0 {
		t.Errorf("job %s was registered (index %d) although its first pre-hook failed", jobID, got)
	}
	runs, err := e.st.ListHookRuns(context.Background(), d.ID)
	if err != nil || len(runs) != 1 || runs[0].Position != 0 || runs[0].State != store.HookFailed {
		t.Errorf("runs = %+v, err %v; want only the failed first one", runs, err)
	}
}
