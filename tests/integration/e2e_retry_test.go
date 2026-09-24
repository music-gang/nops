//go:build integration

package integration

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// A failed deployment blocks its job's drift until something changes (a new
// commit, or a human asking to retry). These tests drive the retry through the
// dashboard, against the real binary and a real Nomad: what it must do, and
// what it must never do (skip the policy).

// blockedText is what the dashboard says of a job held back by a failure.
const blockedText = "push a new commit or retry it"

// retry is the "Retry" button of a blocked job.
func (d *dashboard) retry(t *testing.T, jobID string) int {
	t.Helper()
	status, _ := d.post(t, "/jobs/default/"+jobID+"/retry", nil)
	return status
}

// waitBody waits until GET path answers 200 with a body containing want.
func (d *dashboard) waitBody(t *testing.T, path, want string) {
	t.Helper()
	var last string
	deadline := time.Now().Add(e2eWait)
	for time.Now().Before(deadline) {
		status, body := d.get(t, path)
		if status == http.StatusOK && strings.Contains(body, want) {
			return
		}
		last = body
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("GET %s never showed %q within %s; last body:\n%s", path, want, e2eWait, last)
}

func TestE2ERetryAfterAFailedPreHook(t *testing.T) {
	e := newE2E(t)
	dir := t.TempDir()
	jobID := uniqueID(t, e.raw, "retry")
	hookID := uniqueID(t, e.raw, "retryhook")
	ok := filepath.Join(dir, "ok")

	// The hook fails until the file exists: the operator fixes the cause
	// (a full disk, a down database) without touching git.
	e.repo.commit(t, "job "+jobID+" v1", map[string]string{
		file(jobID):  e2eJob{id: jobID, policy: "auto", version: "1", preHook: hookID}.hcl(),
		file(hookID): hookCmdHCL(hookID, "test -f "+ok, true),
	})
	d1 := e.waitNew(jobID, "", store.StateFailed)
	if d1.CommitSubject != "job "+jobID+" v1" || d1.CommitAuthor != "test" {
		t.Errorf("the deployment records commit %q by %q, want the pushed commit's message and author",
			d1.CommitSubject, d1.CommitAuthor)
	}

	// Blocked: visible in the dashboard, and no retry loop on its own.
	e.dash.waitBody(t, "/jobs", blockedText)
	time.Sleep(time.Second)
	if got := e.deploymentsOf(jobID); got != 1 {
		t.Fatalf("%d deployments for %s, want 1 (blocked, no retry loop)", got, jobID)
	}

	// Where a person finds it: on the Overview with its button, on the job's
	// page, and on the failed deployment itself.
	if status, body := e.dash.get(t, "/"); status != http.StatusOK || !strings.Contains(body, "Needs attention") ||
		!strings.Contains(body, "/jobs/default/"+jobID+"/retry") {
		t.Errorf("/ for a blocked job: status %d, offers its retry: %v", status, strings.Contains(body, "/retry"))
	}
	if status, body := e.dash.get(t, "/jobs/default/"+jobID); status != http.StatusOK || !strings.Contains(body, "Blocked.") {
		t.Errorf("the job page of a blocked job: status %d, says it is blocked: %v", status, strings.Contains(body, "Blocked."))
	}
	if status, body := e.dash.get(t, "/deployments/"+d1.ID); status != http.StatusOK || !strings.Contains(body, "This deployment blocks its job.") {
		t.Errorf("the failed deployment's page: status %d, says it blocks its job: %v", status, strings.Contains(body, "blocks its job"))
	}

	// One retry is one more attempt, not a promise: the hook still fails, the
	// new deployment fails, and the job is blocked again.
	if status := e.dash.retry(t, jobID); status != http.StatusSeeOther {
		t.Fatalf("retry: status %d, want 303", status)
	}
	d2 := e.waitNew(jobID, d1.ID, store.StateFailed)
	if got := e.deployment(d1.ID); got.RetriedBy != e2eUser || got.RetriedAt.IsZero() {
		t.Errorf("the first failure is retried by %q at %v, want %q and a time", got.RetriedBy, got.RetriedAt, e2eUser)
	}
	e.dash.waitBody(t, "/jobs", blockedText)
	time.Sleep(time.Second)
	if got := e.deploymentsOf(jobID); got != 2 {
		t.Fatalf("%d deployments for %s, want 2 (one retry, one attempt)", got, jobID)
	}

	// The cause is fixed: the next retry goes through.
	if err := os.WriteFile(ok, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The observation behind the button is refreshed every cycle: give it a
	// moment to show d2 as the blocker instead of asking too early.
	var status int
	deadline := time.Now().Add(e2eWait)
	for time.Now().Before(deadline) {
		if status = e.dash.retry(t, jobID); status == http.StatusSeeOther {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status != http.StatusSeeOther {
		t.Fatalf("second retry: status %d, want 303", status)
	}
	e.waitNew(jobID, d2.ID, store.StateCompleted)
	if got := e.liveVersion(jobID); got != "1" {
		t.Errorf("live version = %q, want 1", got)
	}

	// Nothing is blocked any more: another click is refused, not silently ignored.
	if status := e.dash.retry(t, jobID); status != http.StatusConflict {
		t.Errorf("retry on a job that is not blocked: status %d, want 409", status)
	}
	if status := e.dash.retry(t, "no-such-job-"+jobID); status != http.StatusConflict {
		t.Errorf("retry on an unknown job: status %d, want 409 (not blocked)", status)
	}
}

// Retrying unblocks; it never approves. Under policy approval the new
// deployment waits for a human like any other (invariant 3).
func TestE2ERetryUnderApprovalStillNeedsApproval(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "retryappr")

	e.repo.commit(t, "job "+jobID+" v1", map[string]string{
		file(jobID): e2eJob{id: jobID, policy: "approval", version: "1"}.hcl(),
	})
	d1 := e.waitNew(jobID, "", store.StatePendingApproval)
	if status := e.dash.reject(t, d1.ID); status != http.StatusSeeOther {
		t.Fatalf("reject: status %d, want 303", status)
	}
	e.waitState(d1.ID, store.StateRejected)
	e.dash.waitBody(t, "/jobs", blockedText)

	if status := e.dash.retry(t, jobID); status != http.StatusSeeOther {
		t.Fatalf("retry: status %d, want 303", status)
	}
	d2 := e.waitNew(jobID, d1.ID, store.StatePendingApproval)
	if d2.DecidedBy != "" || d2.SpecHash != d1.SpecHash {
		t.Errorf("the new deployment is decided by %q with spec %s, want undecided with the rejected spec %s",
			d2.DecidedBy, d2.SpecHash, d1.SpecHash)
	}
	// The old address of the drift page still works.
	if status, _ := e.dash.get(t, "/drift"); status != http.StatusMovedPermanently {
		t.Errorf("GET /drift: status %d, want 301 to /jobs", status)
	}
	// Give it time to be wrongly applied.
	time.Sleep(time.Second)
	if got := e.liveIndex(jobID); got != 0 {
		t.Fatalf("job %s is live (index %d) although nobody approved the retry", jobID, got)
	}
	if got := e.deployment(d2.ID).State; got != store.StatePendingApproval {
		t.Fatalf("state = %s, want pending_approval", got)
	}

	if status := e.dash.approve(t, d2.ID, d2.SpecHash); status != http.StatusSeeOther {
		t.Fatalf("approve: status %d, want 303", status)
	}
	e.waitState(d2.ID, store.StateCompleted)
}

// The "fetch now" button asks for a poll: a commit pushed just before is picked
// up without waiting for the (deliberately long) poll interval.
func TestE2EFetchNow(t *testing.T) {
	e := newE2E(t, "NOPS_GIT_POLL_INTERVAL=1h")
	jobID := uniqueID(t, e.raw, "fetch")

	e.repo.commit(t, "job "+jobID, map[string]string{
		file(jobID): e2eJob{id: jobID, policy: "auto", version: "1"}.hcl(),
	})
	time.Sleep(time.Second)
	if e.latest(jobID) != nil {
		t.Fatalf("nops saw the commit with a 1h poll interval, before anyone asked")
	}

	status, _ := e.dash.post(t, "/fetch", nil)
	if status != http.StatusSeeOther {
		t.Fatalf("fetch: status %d, want 303", status)
	}
	e.waitNew(jobID, "", store.StateCompleted)
}
