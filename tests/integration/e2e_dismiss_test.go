//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// A dismissed orphan is no longer reported and keeps running untouched; the
// undo reports it again, and a new deployment of the job ends the dismissal.
func TestE2EDismissedOrphanRunsUnmanaged(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "dismiss")
	push := func(version string) {
		e.repo.commit(t, "job "+jobID+" v"+version, map[string]string{
			file(jobID): e2eJob{id: jobID, policy: "auto", version: version, cmd: "sleep 300", service: true}.hcl(),
		})
	}
	page := "/jobs/default/" + jobID

	push("1")
	d := e.waitNew(jobID, "", store.StateCompleted)
	e.repo.remove(t, "remove "+jobID, file(jobID))
	e.dash.waitBody(t, "/jobs", "Not in git")

	indexBefore := e.liveIndex(jobID)
	if status, _ := e.dash.post(t, page+"/dismiss", url.Values{"deployment_id": {d.ID}}); status != http.StatusSeeOther {
		t.Fatalf("dismiss: status %d, want 303", status)
	}
	e.dash.waitNoBody(t, "/jobs", "Not in git")
	e.dash.waitNoBody(t, "/", "Removed from git, still running in Nomad")
	if _, body := e.dash.get(t, page); !strings.Contains(body, "Dismissed by") {
		t.Errorf("the job's page does not show the dismissal")
	}
	time.Sleep(2 * time.Second) // several detection cycles
	if _, body := e.dash.get(t, "/jobs"); strings.Contains(body, "Not in git") {
		t.Error("a detection cycle brought the dismissed orphan back")
	}
	live, err := e.nomad.Job(context.Background(), "default", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if (live.Stop != nil && *live.Stop) || e.liveIndex(jobID) != indexBefore {
		t.Fatal("the dismissal touched the job in Nomad")
	}

	// The undo reports it again.
	if status, _ := e.dash.post(t, page+"/undismiss", nil); status != http.StatusSeeOther {
		t.Fatalf("undismiss: status %d, want 303", status)
	}
	e.dash.waitBody(t, "/jobs", "Not in git")

	// Dismissed again, then deployed again from git and removed: reported.
	if status, _ := e.dash.post(t, page+"/dismiss", url.Values{"deployment_id": {d.ID}}); status != http.StatusSeeOther {
		t.Fatalf("second dismiss: status %d, want 303", status)
	}
	e.dash.waitNoBody(t, "/jobs", "Not in git")
	push("2")
	e.waitNew(jobID, d.ID, store.StateCompleted)
	e.repo.remove(t, "remove "+jobID+" again", file(jobID))
	e.dash.waitBody(t, "/jobs", "Not in git")
}
