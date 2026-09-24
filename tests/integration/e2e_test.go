//go:build integration

package integration

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/store"
)

// TestE2EApprovalFlow runs the approval flow the way a person would: push to
// git, decide on the dashboard, edit the job by hand behind nops's back. It
// covers approve, reject, superseding, a stale approval and an outside edit
// under policy approval, against the real binary and a real Nomad.
func TestE2EApprovalFlow(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "appr")
	hookID := uniqueID(t, e.raw, "apprhook")

	job := func(version string) e2eJob {
		return e2eJob{id: jobID, policy: "approval", version: version, postHook: hookID}
	}
	push := func(version string) {
		e.repo.commit(t, "job "+jobID+" v"+version, map[string]string{
			file(jobID):  job(version).hcl(),
			file(hookID): hookCmdHCL(hookID, "exit 0", true),
		})
	}

	// 1. Create, approve: pending_approval → applying → post_hook → completed.
	push("1")
	d1 := e.waitNew(jobID, "", store.StatePendingApproval)
	if d1.DecidedBy != "" {
		t.Errorf("a pending deployment already has a decision by %q", d1.DecidedBy)
	}
	if got := e.liveIndex(jobID); got != 0 {
		t.Fatalf("job %s is live (index %d) before anyone approved it", jobID, got)
	}
	// What a reviewer sees: it is listed as waiting, and the approval form
	// carries the whole spec_hash it is valid for (invariant 3), whatever the
	// page shows short.
	if status, body := e.dash.get(t, "/"); status != http.StatusOK || !strings.Contains(body, "Needs approval") || !strings.Contains(body, jobID) {
		t.Errorf("/ with a pending deployment: status %d, lists it as waiting: %v", status, strings.Contains(body, "Needs approval"))
	}
	if status, body := e.dash.get(t, "/deployments/"+d1.ID); status != http.StatusOK ||
		!strings.Contains(body, `name="spec_hash" value="`+d1.SpecHash+`"`) || !strings.Contains(body, "Run the post-hook") {
		t.Errorf("review page: status %d, full spec_hash in the form and the steps shown: %v", status, strings.Contains(body, d1.SpecHash))
	}
	if status := e.dash.approve(t, d1.ID, d1.SpecHash); status != http.StatusSeeOther {
		t.Fatalf("approve: status %d, want 303", status)
	}
	done := e.waitState(d1.ID, store.StateCompleted)
	if done.DecidedBy != e2eUser {
		t.Errorf("decided_by = %q, want %q", done.DecidedBy, e2eUser)
	}
	if got := e.liveVersion(jobID); got != "1" {
		t.Errorf("live version = %q, want 1", got)
	}
	wantPath := []store.State{store.StateDetected, store.StatePendingApproval, store.StateApplying, store.StatePostHook, store.StateCompleted}
	if got := e.states(d1.ID); !slices.Equal(got, wantPath) {
		t.Errorf("states = %v, want %v", got, wantPath)
	}
	if status, body := e.dash.get(t, "/history"); status != http.StatusOK || !strings.Contains(body, jobID) {
		t.Errorf("/history: status %d, lists %s: %v", status, jobID, strings.Contains(body, jobID))
	}

	// 2. Reject: nothing is applied, and the change stays visible as drift.
	push("2")
	d2 := e.waitNew(jobID, d1.ID, store.StatePendingApproval)
	indexBefore := e.liveIndex(jobID)
	if status := e.dash.reject(t, d2.ID); status != http.StatusSeeOther {
		t.Fatalf("reject: status %d, want 303", status)
	}
	rejected := e.waitState(d2.ID, store.StateRejected)
	if rejected.DecidedBy != e2eUser {
		t.Errorf("rejected decided_by = %q, want %q", rejected.DecidedBy, e2eUser)
	}
	if got := e.liveIndex(jobID); got != indexBefore {
		t.Errorf("live index moved %d → %d after a reject: something was applied", indexBefore, got)
	}
	if got := e.liveVersion(jobID); got != "1" {
		t.Errorf("live version = %q after a reject, want 1", got)
	}
	if status, body := e.dash.get(t, "/jobs"); status != http.StatusOK || !strings.Contains(body, jobID) {
		t.Errorf("/jobs after a reject: status %d, still lists %s: %v", status, jobID, strings.Contains(body, jobID))
	}

	// 3. Supersede: a newer commit replaces the pending deployment before
	// anyone decided on it.
	push("3")
	d3 := e.waitNew(jobID, d2.ID, store.StatePendingApproval)
	push("4")
	e.waitState(d3.ID, store.StateSuperseded)
	d4 := e.waitNew(jobID, d3.ID, store.StatePendingApproval)
	if d4.SpecHash == d3.SpecHash {
		t.Errorf("the new deployment has the superseded one's spec_hash %s", d4.SpecHash)
	}
	if got := e.deployment(d3.ID).DecidedBy; got != "" {
		t.Errorf("superseded deployment has a decision by %q: nobody decided", got)
	}

	// 4. An approval is bound to its spec_hash: the stale one is refused.
	if status := e.dash.approve(t, d4.ID, d3.SpecHash); status != http.StatusConflict {
		t.Errorf("approve with a stale spec_hash: status %d, want 409", status)
	}
	if got := e.deployment(d4.ID).State; got != store.StatePendingApproval {
		t.Errorf("state after a refused approval = %s, want pending_approval", got)
	}
	if got := e.liveVersion(jobID); got != "1" {
		t.Errorf("live version = %q after a refused approval, want 1", got)
	}

	// 5. Outside edit while pending: the pending deployment is revalidated
	// against the new live job and superseded by one that brings the job back
	// to what git says, never the other way around.
	e.editOutsideNops(jobID, job("hand-edited").hcl())
	e.waitState(d4.ID, store.StateSuperseded)
	d5 := e.waitNew(jobID, d4.ID, store.StatePendingApproval)
	if got := e.liveVersion(jobID); got != "hand-edited" {
		t.Fatalf("live version = %q, want the hand edit to be untouched until approval", got)
	}
	if status := e.dash.approve(t, d5.ID, d5.SpecHash); status != http.StatusSeeOther {
		t.Fatalf("approve: status %d, want 303", status)
	}
	e.waitState(d5.ID, store.StateCompleted)
	if got := e.liveVersion(jobID); got != "4" {
		t.Errorf("live version = %q after approval, want git's 4", got)
	}
}

// TestE2EAutoRevertsOutsideEdit is the self-heal case: under policy auto,
// nops puts back what git says without anyone approving it.
func TestE2EAutoRevertsOutsideEdit(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "auto")
	job := e2eJob{id: jobID, policy: "auto", version: "git"}

	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): job.hcl()})
	d1 := e.waitNew(jobID, "", store.StateCompleted)
	if got := e.liveVersion(jobID); got != "git" {
		t.Fatalf("live version = %q after the first deployment, want git", got)
	}

	e.editOutsideNops(jobID, e2eJob{id: jobID, policy: "auto", version: "hand-edited"}.hcl())
	d2 := e.waitNew(jobID, d1.ID, store.StateCompleted)
	if got := e.liveVersion(jobID); got != "git" {
		t.Errorf("live version = %q, want nops to have put git's back", got)
	}
	if d2.DecidedBy != "" {
		t.Errorf("an auto deployment has a human decision by %q", d2.DecidedBy)
	}
}
