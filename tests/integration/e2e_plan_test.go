//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/store"
)

// planCommit is the commit a plan names: any full SHA.
const planCommit = "0123456789abcdef0123456789abcdef01234567"

// TestE2ECLIPlansAChangeBeforeTheMerge: the built binary plans a checkout of
// the repository with a newer version. Nomad parses and plans it for real:
// the answer says the job would deploy and shows the change, and nothing moves
// in Nops or in Nomad. A checkout with invalid meta fails the command.
func TestE2ECLIPlansAChangeBeforeTheMerge(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "plan")
	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): e2eJob{id: jobID, policy: "auto", version: "1"}.hcl()})
	e.waitNew(jobID, "", store.StateCompleted)
	token := e.apiToken(t)

	plan := func(j e2eJob) (string, string, error) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, file(jobID)), []byte(j.hcl()), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(buildNopsBinary(t), "plan", "-repository", "default", "-commit", planCommit, dir)
		cmd.Env = append(os.Environ(), "NOPS_ADDR="+e.dash.base, "NOPS_TOKEN="+token)
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.String(), errOut.String(), err
	}
	deployments := e.deploymentsOf(jobID)

	out, errOut, err := plan(e2eJob{id: jobID, policy: "auto", version: "2"})
	if err != nil {
		t.Fatalf("nops plan: %v\nstdout %s\nstderr %s", err, out, errOut)
	}
	for _, want := range []string{"Job:         default/" + jobID, "Sync:        deploying", "1 => 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan lacks %q:\n%s", want, out)
		}
	}

	out, _, err = plan(e2eJob{id: jobID, policy: "bogus", version: "2"})
	if err == nil || !strings.Contains(out, "invalid meta") {
		t.Errorf("a plan with invalid meta: %v, stdout %s: want it to fail on the meta", err, out)
	}

	if got := e.deploymentsOf(jobID); got != deployments {
		t.Errorf("deployments of %s = %d after the plans, want %d", jobID, got, deployments)
	}
	if got := e.liveVersion(jobID); got != "1" {
		t.Errorf("live version = %q after the plans, want 1", got)
	}
}
