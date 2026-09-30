//go:build integration

package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// waitPage waits until the dashboard page at path satisfies ok, and returns it.
func (e *e2eEnv) waitPage(path, what string, ok func(body string) bool) string {
	e.t.Helper()
	var body string
	deadline := time.Now().Add(e2eWait)
	for time.Now().Before(deadline) {
		var status int
		status, body = e.dash.get(e.t, path)
		if status == http.StatusOK && ok(body) {
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("GET %s never showed %s within %s; last page:\n%s", path, what, e2eWait, body)
	return ""
}

// quiet is how long a test watches nops do nothing: around ten detection and
// apply cycles at the test intervals of 200ms.
const quiet = 2 * time.Second

// TestE2EPausedJobIsLeftAloneUnderAuto is the incident case: someone hand-edits
// a job under auto (or reverts it), and nops, which would put git's version
// back on its next cycle, is told to leave it alone until they say otherwise.
func TestE2EPausedJobIsLeftAloneUnderAuto(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "pause")
	job := e2eJob{id: jobID, policy: "auto", version: "git"}
	page := "/jobs/default/" + jobID

	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): job.hcl()})
	d1 := e.waitNew(jobID, "", store.StateCompleted)

	if status, _ := e.dash.post(t, page+"/pause", url.Values{"reason": {"hand fix in progress"}}); status != http.StatusSeeOther {
		t.Fatalf("pause: status %d, want 303", status)
	}
	e.editOutsideNops(jobID, e2eJob{id: jobID, policy: "auto", version: "hand-edited"}.hcl())

	// Drift is still detected and shown, and says who paused the job and why.
	e.waitPage(page, "the drift of a paused job", func(b string) bool {
		return strings.Contains(b, "Paused by "+e2eUser) && strings.Contains(b, "hand fix in progress") &&
			!strings.Contains(b, "In sync with git.")
	})

	time.Sleep(quiet)
	if n := e.deploymentsOf(jobID); n != 1 {
		t.Errorf("%d deployments of a paused job, want only the first one", n)
	}
	if got := e.liveVersion(jobID); got != "hand-edited" {
		t.Errorf("live version = %q while paused, want the hand edit to stand", got)
	}

	if status, _ := e.dash.post(t, page+"/resume", nil); status != http.StatusSeeOther {
		t.Fatalf("resume: status %d, want 303", status)
	}
	d2 := e.waitNew(jobID, d1.ID, store.StateCompleted)
	if got := e.liveVersion(jobID); got != "git" {
		t.Errorf("live version = %q after the resume, want git's", got)
	}
	if d2.DecidedBy != "" {
		t.Errorf("an auto deployment has a human decision by %q", d2.DecidedBy)
	}
	e.waitPage(page, "the job in sync again", func(b string) bool { return !strings.Contains(b, "Paused by ") })
}

// TestE2EPausedJobCannotBeApproved: under approval the pause is a brake on the
// whole job. A pending deployment stays pending, refuses Approve, and is
// approvable again once the job is resumed.
func TestE2EPausedJobCannotBeApproved(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "pauseappr")
	job := e2eJob{id: jobID, policy: "approval", version: "1"}
	page := "/jobs/default/" + jobID

	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): job.hcl()})
	d := e.waitNew(jobID, "", store.StatePendingApproval)

	if status, _ := e.dash.post(t, page+"/pause", nil); status != http.StatusSeeOther {
		t.Fatalf("pause: status %d, want 303", status)
	}
	e.waitPage("/deployments/"+d.ID, "the pause on the deployment", func(b string) bool {
		return strings.Contains(b, "The job is paused")
	})

	if status := e.dash.approve(t, d.ID, d.SpecHash); status != http.StatusConflict {
		t.Fatalf("approve of a paused job: status %d, want 409", status)
	}
	time.Sleep(quiet)
	if got := e.deployment(d.ID); got.State != store.StatePendingApproval || got.DecidedBy != "" {
		t.Fatalf("deployment = %s decided by %q, want it still pending and undecided", got.State, got.DecidedBy)
	}
	if got := e.liveVersion(jobID); got != "" {
		t.Errorf("live version = %q, want the job not registered while paused", got)
	}

	if status, _ := e.dash.post(t, page+"/resume", nil); status != http.StatusSeeOther {
		t.Fatalf("resume: status %d, want 303", status)
	}
	if status := e.dash.approve(t, d.ID, d.SpecHash); status != http.StatusSeeOther {
		t.Fatalf("approve after the resume: status %d, want 303", status)
	}
	e.waitState(d.ID, store.StateCompleted)
	if got := e.liveVersion(jobID); got != "1" {
		t.Errorf("live version = %q after approval, want 1", got)
	}
}
