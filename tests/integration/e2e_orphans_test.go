//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// waitNoBody waits until GET path answers 200 without unwanted in its body.
func (d *dashboard) waitNoBody(t *testing.T, path, unwanted string) {
	t.Helper()
	var last string
	deadline := time.Now().Add(e2eWait)
	for time.Now().Before(deadline) {
		status, body := d.get(t, path)
		if status == http.StatusOK && !strings.Contains(body, unwanted) {
			return
		}
		last = body
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GET %s still showed %q after %s; last body:\n%s", path, unwanted, e2eWait, last)
}

// A job nops deployed, removed from git and still running in Nomad is shown
// and never touched: the operator stops it (the alert then goes away by
// itself) or puts the file back.
func TestE2EOrphanJobIsShownAndNeverStopped(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "orphan")

	// A job that keeps running, deployed by nops.
	e.repo.commit(t, "job "+jobID, map[string]string{
		file(jobID): e2eJob{id: jobID, policy: "auto", version: "1", cmd: "sleep 300"}.hcl(),
	})
	d := e.waitNew(jobID, "", store.StateCompleted)

	// While it is in the repository it is nobody's orphan.
	time.Sleep(time.Second)
	if _, body := e.dash.get(t, "/jobs"); strings.Contains(body, "Not in git") {
		t.Fatalf("/jobs shows an orphan while %s is still in the repository", jobID)
	}

	// Removed from git.
	e.repo.remove(t, "remove "+jobID, file(jobID))
	e.dash.waitBody(t, "/jobs", "Not in git")

	_, jobs := e.dash.get(t, "/jobs")
	_, overview := e.dash.get(t, "/")
	_, page := e.dash.get(t, "/jobs/default/"+jobID)
	for name, c := range map[string]struct{ body, want string }{
		"Jobs":     {jobs, "/jobs/default/" + jobID},
		"Overview": {overview, "Removed from git, still running in Nomad"},
		"Job":      {page, "nomad job stop -namespace default " + jobID},
	} {
		if !strings.Contains(c.body, c.want) {
			t.Errorf("%s does not show the orphan: missing %q", name, c.want)
		}
	}

	// Reported, never acted on: give detection several cycles, then check that
	// nothing was created and the job is exactly as it was.
	indexBefore := e.liveIndex(jobID)
	time.Sleep(2 * time.Second)
	if got := e.deploymentsOf(jobID); got != 1 {
		t.Errorf("%d deployments for %s, want 1: an orphan creates none", got, jobID)
	}
	if got := e.liveIndex(jobID); got != indexBefore {
		t.Errorf("live index moved %d → %d: nops touched an orphan", indexBefore, got)
	}
	live, err := e.nomad.Job(context.Background(), "default", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if live.Stop != nil && *live.Stop {
		t.Fatalf("job %s is stopped: nops must never stop an orphan", jobID)
	}
	if got := e.deployment(d.ID).State; got != store.StateCompleted {
		t.Errorf("the last deployment moved to %s", got)
	}

	// The operator stops it: the alert goes away by itself.
	if err := e.nomad.StopJob(context.Background(), "default", jobID); err != nil {
		t.Fatal(err)
	}
	e.dash.waitNoBody(t, "/jobs", "Not in git")
	e.dash.waitNoBody(t, "/", "Removed from git, still running in Nomad")
}

func TestE2EOrphanGoesAwayWhenTheFileComesBack(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "restore")
	push := func() {
		e.repo.commit(t, "job "+jobID, map[string]string{
			file(jobID): e2eJob{id: jobID, policy: "auto", version: "1", cmd: "sleep 300"}.hcl(),
		})
	}

	push()
	d := e.waitNew(jobID, "", store.StateCompleted)
	e.repo.remove(t, "remove "+jobID, file(jobID))
	e.dash.waitBody(t, "/jobs", "Not in git")

	// Put back, identical to what is live: managed again, in sync, no new deployment.
	push()
	e.dash.waitNoBody(t, "/jobs", "Not in git")
	time.Sleep(time.Second)
	if got := e.deploymentsOf(jobID); got != 1 || e.latest(jobID).ID != d.ID {
		t.Errorf("%d deployments for %s after the file came back, want the original only", got, jobID)
	}
	if _, body := e.dash.get(t, "/jobs"); !strings.Contains(body, "In sync") {
		t.Error("the restored job is not shown as in sync")
	}
}
