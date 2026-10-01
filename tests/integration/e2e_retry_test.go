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

// retry is the "Retry" button of a failed or rejected deployment.
func (d *dashboard) retry(t *testing.T, deploymentID string) int {
	t.Helper()
	status, _ := d.post(t, "/deployments/"+deploymentID+"/retry", nil)
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
		!strings.Contains(body, "/deployments/"+d1.ID+"/retry") {
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
	if status := e.dash.retry(t, d1.ID); status != http.StatusSeeOther {
		t.Fatalf("retry: status %d, want 303", status)
	}
	d2 := e.waitNew(jobID, d1.ID, store.StateFailed)
	if d2.RetryOf != d1.ID {
		t.Errorf("the new deployment retries %q, want %q", d2.RetryOf, d1.ID)
	}
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
	if status := e.dash.retry(t, d2.ID); status != http.StatusSeeOther {
		t.Fatalf("second retry: status %d, want 303", status)
	}
	e.waitNew(jobID, d2.ID, store.StateCompleted)
	if got := e.liveVersion(jobID); got != "1" {
		t.Errorf("live version = %q, want 1", got)
	}

	// Already retried: another click is refused, not silently ignored.
	if status := e.dash.retry(t, d2.ID); status != http.StatusConflict {
		t.Errorf("retry on a deployment that was already retried: status %d, want 409", status)
	}
	if status := e.dash.retry(t, "no-such-deployment-"+jobID); status != http.StatusNotFound {
		t.Errorf("retry on an unknown deployment: status %d, want 404", status)
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

	if status := e.dash.retry(t, d1.ID); status != http.StatusSeeOther {
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

// What the pages say about a job comes from the last detection cycle, which is
// from before the apply. With the drift tick set to an hour, only the cycle
// that a deployment asks for when it ends can bring them up to date: In sync
// after a completed one, Blocked after a failed one.
func TestE2EJobPageFollowsADeploymentThatEnds(t *testing.T) {
	e := newE2E(t, "NOPS_DRIFT_INTERVAL=1h")
	dir := t.TempDir()
	okJob := uniqueID(t, e.raw, "ends")
	failJob := uniqueID(t, e.raw, "endsfail")
	hookID := uniqueID(t, e.raw, "endshook")

	e.repo.commit(t, "two jobs", map[string]string{
		file(okJob):   e2eJob{id: okJob, policy: "approval", version: "1"}.hcl(),
		file(failJob): e2eJob{id: failJob, policy: "auto", version: "1", preHook: hookID}.hcl(),
		file(hookID):  hookCmdHCL(hookID, "test -f "+filepath.Join(dir, "never"), true),
	})

	d := e.waitNew(okJob, "", store.StatePendingApproval)
	e.dash.waitBody(t, "/jobs/default/"+okJob, "Awaiting approval")
	if status := e.dash.approve(t, d.ID, d.SpecHash); status != http.StatusSeeOther {
		t.Fatalf("approve: status %d", status)
	}
	e.waitState(d.ID, store.StateCompleted)
	e.dash.waitBody(t, "/jobs/default/"+okJob, "In sync with git.")

	e.waitNew(failJob, "", store.StateFailed)
	e.dash.waitBody(t, "/jobs/default/"+failJob, "Blocked.")
}

// A post-hook that fails after the apply leaves no drift, so nothing blocks the
// job and only a new commit used to run it again. The retry runs the
// post-hooks again against the job as it is, and registers nothing.
func TestE2ERetryAfterAFailedPostHook(t *testing.T) {
	e := newE2E(t)
	dir := t.TempDir()
	jobID := uniqueID(t, e.raw, "retrypost")
	hookID := uniqueID(t, e.raw, "retryposthook")
	ok := filepath.Join(dir, "ok")

	e.repo.commit(t, "job "+jobID+" v1", map[string]string{
		file(jobID):  e2eJob{id: jobID, policy: "auto", version: "1", postHook: hookID}.hcl(),
		file(hookID): hookCmdHCL(hookID, "test -f "+ok, true),
	})
	d1 := e.waitNew(jobID, "", store.StateFailed)
	if d1.AppliedIndex == 0 {
		t.Fatalf("the post-hook failed before the apply: %+v", d1)
	}
	index := e.liveIndex(jobID)

	// Nothing blocks it, but it can be retried where it is read.
	e.dash.waitBody(t, "/deployments/"+d1.ID, "This deployment can be retried.")
	time.Sleep(time.Second)
	if got := e.deploymentsOf(jobID); got != 1 {
		t.Fatalf("%d deployments for %s, want 1 (nothing blocks it, nothing retries it)", got, jobID)
	}

	// The cause is fixed; the page the click lands on shows the retry already.
	if err := os.WriteFile(ok, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	status, _ := e.dash.post(t, "/deployments/"+d1.ID+"/retry", nil)
	if status != http.StatusSeeOther {
		t.Fatalf("retry: status %d, want 303", status)
	}
	d2 := e.waitNew(jobID, d1.ID, store.StateCompleted)
	if d2.RetryOf != d1.ID {
		t.Errorf("the new deployment retries %q, want %q", d2.RetryOf, d1.ID)
	}
	if got := e.liveIndex(jobID); got != index {
		t.Errorf("job index = %d, want %d: the retry registered something", got, index)
	}
	e.dash.waitBody(t, "/deployments/"+d1.ID, "/deployments/"+d2.ID)
	e.dash.waitBody(t, "/deployments/"+d2.ID, "Retry of")
}
