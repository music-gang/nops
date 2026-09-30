//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// windowJob is e2eJob with a sync window: extra meta the e2e helper does not
// know, added to its HCL.
func windowJob(id, version, window, duration string) string {
	hcl := e2eJob{id: id, policy: "auto", version: version}.hcl()
	meta := fmt.Sprintf("    nops_sync_window          = %q\n    nops_sync_window_duration = %q\n", window, duration)
	return strings.Replace(hcl, "  meta {\n", "  meta {\n"+meta, 1)
}

// closedWindow is a window that opens twelve hours from now, for one hour: the
// instance reads windows in UTC unless told otherwise, and this is closed
// whatever time the test runs at.
func closedWindow() string { return fmt.Sprintf("0 %d * * *", (time.Now().UTC().Hour()+12)%24) }

// openWindow opens every minute and stays open for an hour: always open.
const openWindow = "* * * * *"

// TestE2ESyncWindowHoldsAnAutomaticDeployment is the 3 a.m. case: a change
// lands outside the job's window, and nothing is deployed until it opens. Drift
// is still shown, the job says it is held, and when the window opens what is
// applied is what git says then.
func TestE2ESyncWindowHoldsAnAutomaticDeployment(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "window")
	page := "/jobs/default/" + jobID

	// Outside the window from the start: nothing is deployed, the job is held.
	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): windowJob(jobID, "1", closedWindow(), "1h")})
	e.waitPage(page, "the job held outside its window", func(b string) bool {
		return strings.Contains(b, "Held:") && strings.Contains(b, "outside its sync window, next opens")
	})
	time.Sleep(quiet)
	if n := e.deploymentsOf(jobID); n != 0 {
		t.Errorf("%d deployments outside the window, want none", n)
	}
	if got := e.liveVersion(jobID); got != "" {
		t.Errorf("live version = %q outside the window, want the job not registered", got)
	}
	// The page says where the window stands and in which zone it is read.
	_, body := e.dash.get(t, page)
	for _, want := range []string{"closed until", "read in UTC"} {
		if !strings.Contains(body, want) {
			t.Errorf("the job page does not say %q", want)
		}
	}
	_, jobs := e.dash.get(t, "/jobs")
	if !strings.Contains(jobs, ">Held<") {
		t.Errorf("the Jobs page does not say the job is Held")
	}

	// The window opens (the commit changes it): detection plans afresh and the
	// job is deployed.
	e.repo.commit(t, "open the window", map[string]string{file(jobID): windowJob(jobID, "2", openWindow, "1h")})
	d1 := e.waitNew(jobID, "", store.StateCompleted)
	if got := e.liveVersion(jobID); got != "2" {
		t.Fatalf("live version = %q inside the window, want 2", got)
	}

	// Closed again: a new change waits, and what is live stays.
	e.repo.commit(t, "close it and change", map[string]string{file(jobID): windowJob(jobID, "3", closedWindow(), "1h")})
	e.waitPage(page, "the new change held", func(b string) bool { return strings.Contains(b, "Held:") })
	time.Sleep(quiet)
	if got := e.latest(jobID); got.ID != d1.ID {
		t.Errorf("a deployment %s appeared outside the window, want only %s", describe(got), d1.ID)
	}
	if got := e.liveVersion(jobID); got != "2" {
		t.Errorf("live version = %q outside the window, want the last one deployed, 2", got)
	}

	e.repo.commit(t, "open it again", map[string]string{file(jobID): windowJob(jobID, "4", openWindow, "1h")})
	e.waitNew(jobID, d1.ID, store.StateCompleted)
	if got := e.liveVersion(jobID); got != "4" {
		t.Errorf("live version = %q after the window opened again, want git's 4", got)
	}
}
