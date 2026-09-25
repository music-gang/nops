package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// deployed records that nops put a job in production: a deployment that ended
// completed, as the orphan check reads it.
func (h *harness) deployed(jobID string, policy store.Policy) *store.Deployment {
	h.t.Helper()
	ctx := context.Background()
	d := &store.Deployment{
		JobID: jobID, Namespace: testNamespace, CommitSHA: "c0", SpecHash: "h-" + jobID,
		JobSpec: `{"ID":"` + jobID + `"}`, Policy: policy,
	}
	if err := h.store.CreateDeployment(ctx, d); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.Transition(ctx, d.ID, store.StateCompleted, store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		h.t.Fatal(err)
	}
	return d
}

// runningInNomad puts the job in the fake cluster with a status.
func (h *harness) runningInNomad(jobID, status string) {
	j := job(jobID, nil)
	j.Status = &status
	h.nomad.setLive(j)
}

func orphanIDs(orphans []Orphan) string {
	ids := make([]string, len(orphans))
	for i, o := range orphans {
		ids[i] = o.JobID
	}
	return strings.Join(ids, ",")
}

func TestOrphanIsAJobDeployedThenRemovedFromGit(t *testing.T) {
	h := newHarness(t)
	d := h.deployed("web", store.PolicyApproval)
	h.runningInNomad("web", "running")
	h.snap.set("c2", gitwatch.File{Path: "README.nomad.hcl", Content: "other"})
	h.nomad.setFile("other", managed("other", "auto", nil))

	h.detect()

	got := h.engine.Orphans()
	if len(got) != 1 {
		t.Fatalf("orphans = %+v, want web", got)
	}
	o := got[0]
	if o.JobID != "web" || o.Namespace != testNamespace || o.Policy != store.PolicyApproval ||
		o.LastDeploymentID != d.ID || o.NomadStatus != "running" || o.ObservedAt.IsZero() {
		t.Errorf("orphan = %+v", o)
	}
	if st := h.engine.Status(); st.Orphans != 1 || st.OrphanCheckSkipped {
		t.Errorf("Status = %+v, want 1 orphan and the check done", st)
	}
	// Reported only: nothing is stopped, registered or created.
	if len(h.nomad.registerCalls) != 0 || h.nomad.callCount() > 3 {
		t.Errorf("the check touched Nomad beyond reading: %d register calls", len(h.nomad.registerCalls))
	}
}

func TestOrphanSetIsSortedAndACopy(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"zeta", "alpha", "mid"} {
		h.deployed(id, store.PolicyAuto)
		h.runningInNomad(id, "pending")
	}
	h.detect()

	got := h.engine.Orphans()
	if orphanIDs(got) != "alpha,mid,zeta" {
		t.Fatalf("orphans = %s, want sorted", orphanIDs(got))
	}
	got[0].JobID = "tampered"
	if orphanIDs(h.engine.Orphans()) != "alpha,mid,zeta" {
		t.Error("Orphans() hands out the engine's own slice")
	}
}

func TestOrphanIsNotAJobStillInTheRepository(t *testing.T) {
	cases := map[string]func(h *harness){
		"managed": func(h *harness) {
			h.nomad.setFile("web", managed("web", "auto", nil))
			h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web"})
		},
		// nops_managed removed: "hands off", not "removed".
		"in the repository without nops_managed": func(h *harness) {
			h.nomad.setFile("web", job("web", nil))
			h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web"})
		},
		"policy none": func(h *harness) {
			h.nomad.setFile("web", managed("web", "none", nil))
			h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web"})
		},
		"invalid meta": func(h *harness) {
			h.nomad.setFile("web", managed("web", "sometimes", nil))
			h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "web"})
		},
		// Both files are ignored by classification, but the job is still in git.
		"two files with the same ID": func(h *harness) {
			h.nomad.setFile("a", managed("web", "auto", nil))
			h.nomad.setFile("b", managed("web", "auto", nil))
			h.snap.set("c2", gitwatch.File{Path: "a.nomad.hcl", Content: "a"}, gitwatch.File{Path: "b.nomad.hcl", Content: "b"})
		},
		"renamed file": func(h *harness) {
			h.nomad.setFile("web", managed("web", "auto", nil))
			h.snap.set("c2", gitwatch.File{Path: "apps/web/main.nomad.hcl", Content: "web"})
		},
	}
	for name, inRepo := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.deployed("web", store.PolicyAuto)
			h.runningInNomad("web", "running")
			inRepo(h)
			h.detect()
			if got := h.engine.Orphans(); len(got) != 0 {
				t.Errorf("orphans = %s, want none: the job is still in the repository", orphanIDs(got))
			}
		})
	}
}

func TestOrphanIsNotAJobNomadHasNothingRunningFor(t *testing.T) {
	h := newHarness(t)
	h.deployed("stopped", store.PolicyAuto)
	stopped := job("stopped", nil)
	yes, running := true, "running"
	stopped.Stop, stopped.Status = &yes, &running
	h.nomad.setLive(stopped)

	h.deployed("dead", store.PolicyAuto)
	h.runningInNomad("dead", "dead")

	h.deployed("purged", store.PolicyAuto) // not in Nomad at all

	h.deployed("alive", store.PolicyAuto)
	h.runningInNomad("alive", "running")

	h.detect()

	if got := orphanIDs(h.engine.Orphans()); got != "alive" {
		t.Errorf("orphans = %q, want only alive (stopped, dead and purged are not)", got)
	}
}

func TestOrphanNeedsACompletedDeployment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, end := range []store.State{store.StateFailed, store.StateSuperseded} {
		id := "never-" + string(end)
		d := &store.Deployment{JobID: id, Namespace: testNamespace, CommitSHA: "c0", SpecHash: "h", JobSpec: "{}", Policy: store.PolicyAuto}
		if err := h.store.CreateDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
		if err := h.store.Transition(ctx, d.ID, end, store.Transition{From: store.StateDetected, Actor: "nops", Error: "x"}); err != nil {
			t.Fatal(err)
		}
		h.runningInNomad(id, "running") // someone else's job with the same ID, say
	}
	pending := &store.Deployment{JobID: "waiting", Namespace: testNamespace, CommitSHA: "c0", SpecHash: "h", JobSpec: "{}", Policy: store.PolicyApproval}
	if err := h.store.CreateDeployment(ctx, pending); err != nil {
		t.Fatal(err)
	}
	h.runningInNomad("waiting", "running")

	h.detect()

	if got := h.engine.Orphans(); len(got) != 0 {
		t.Errorf("orphans = %s, want none: nops never put these in production", orphanIDs(got))
	}
}

func TestOrphanUsesTheLastCompletedDeployment(t *testing.T) {
	h := newHarness(t)
	h.deployed("web", store.PolicyAuto)
	last := h.deployed("web", store.PolicyApproval)
	h.runningInNomad("web", "running")
	h.detect()

	got := h.engine.Orphans()
	if len(got) != 1 || got[0].LastDeploymentID != last.ID || got[0].Policy != store.PolicyApproval {
		t.Errorf("orphans = %+v, want the last completed deployment %s", got, last.ID)
	}
}

// The alert clears on its own, without nops writing anything: the operator
// stops the job, or puts the file back.
func TestOrphanClearsWhenStoppedOrRestored(t *testing.T) {
	h := newHarness(t)
	h.deployed("web", store.PolicyAuto)
	h.runningInNomad("web", "running")
	h.detect()
	if len(h.engine.Orphans()) != 1 {
		t.Fatal("setup: no orphan")
	}

	// Stopped in Nomad.
	yes, running := true, "running"
	stopped := job("web", nil)
	stopped.Stop, stopped.Status = &yes, &running
	h.nomad.setLive(stopped)
	h.detect()
	if got := h.engine.Orphans(); len(got) != 0 {
		t.Fatalf("orphans after a stop = %s, want none", orphanIDs(got))
	}
	if st := h.engine.Status(); st.Orphans != 0 {
		t.Errorf("Status.Orphans = %d, want 0", st.Orphans)
	}

	// Running again (someone restarted it), then the file is put back.
	h.runningInNomad("web", "running")
	h.detect()
	if len(h.engine.Orphans()) != 1 {
		t.Fatal("a restarted job that is not in git is an orphan again")
	}
	h.nomad.setFile("web", managed("web", "auto", nil))
	h.snap.set("c3", gitwatch.File{Path: "web.nomad.hcl", Content: "web"})
	h.detect()
	if got := h.engine.Orphans(); len(got) != 0 {
		t.Errorf("orphans after the file came back = %s, want none", orphanIDs(got))
	}
}

// A file that does not parse looks exactly like a removed one, so with one the
// check does not run: the orphans of the last complete check stand, and Status
// says the check was skipped.
func TestOrphanCheckIsSuspendedWhileAFileDoesNotParse(t *testing.T) {
	h := newHarness(t)
	h.deployed("old", store.PolicyAuto)
	h.runningInNomad("old", "running")
	h.detect()
	if orphanIDs(h.engine.Orphans()) != "old" {
		t.Fatal("setup: no orphan")
	}

	// A new removal, and a broken file, in the same commit.
	h.deployed("newer", store.PolicyAuto)
	h.runningInNomad("newer", "running")
	h.nomad.setParseErr("broken", errors.New("syntax error"))
	h.snap.set("c2", gitwatch.File{Path: "web.nomad.hcl", Content: "broken"})
	h.detect()

	st := h.engine.Status()
	if !st.OrphanCheckSkipped || st.Unparsed != 1 || st.Orphans != 1 {
		t.Errorf("Status = %+v, want the check skipped with the previous orphan kept", st)
	}
	if got := orphanIDs(h.engine.Orphans()); got != "old" {
		t.Errorf("orphans = %q, want the last complete check's (old), not newer", got)
	}

	// The file is fixed: the check runs again and sees both.
	h.snap.set("c3")
	h.detect()
	if st := h.engine.Status(); st.OrphanCheckSkipped {
		t.Errorf("Status = %+v: the check must run again once every file parses", st)
	}
	if got := orphanIDs(h.engine.Orphans()); got != "newer,old" {
		t.Errorf("orphans = %q, want newer,old", got)
	}
}

func TestOrphanCheckSkipsAJobNomadFailsOn(t *testing.T) {
	h := newHarness(t)
	h.deployed("bad", store.PolicyAuto)
	h.nomad.jobErr["bad"] = errors.New("nomad is down")
	h.deployed("good", store.PolicyAuto)
	h.runningInNomad("good", "running")

	logs := new(bytes.Buffer)
	h.engine.log = slog.New(slog.NewTextHandler(logs, nil))
	h.detect()

	if got := orphanIDs(h.engine.Orphans()); got != "good" {
		t.Errorf("orphans = %q, want good: one failing job must not hide the others", got)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "nomad is down") {
		t.Errorf("a Nomad failure must be an ERROR, got: %s", logs)
	}
	if st := h.engine.Status(); st.Error != "" {
		t.Errorf("Status.Error = %q: a failure scoped to one job does not stop the cycle", st.Error)
	}
}

func TestOrphanCheckStoreFailureStopsTheCycle(t *testing.T) {
	h := newHarness(t)
	h.engine.store = failingLatestCompleted{Store: h.engine.store}

	err := h.engine.Detect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "list deployed jobs") {
		t.Fatalf("Detect err = %v, want the store failure", err)
	}
	if st := h.engine.Status(); st.Error == "" {
		t.Error("Status.Error is empty after a failed cycle")
	}
}

type failingLatestCompleted struct{ Store }

func (failingLatestCompleted) LatestCompletedPerJob(context.Context, string) ([]*store.Deployment, error) {
	return nil, fmt.Errorf("disk on fire")
}

func TestOrphanCheckIgnoresOtherNamespaces(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := &store.Deployment{JobID: "web", Namespace: "staging", CommitSHA: "c0", SpecHash: "h", JobSpec: "{}", Policy: store.PolicyAuto}
	if err := h.store.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(ctx, d.ID, store.StateCompleted, store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	h.runningInNomad("web", "running")
	h.detect()
	if got := h.engine.Orphans(); len(got) != 0 {
		t.Errorf("orphans = %s, want none: nops manages only %q", orphanIDs(got), testNamespace)
	}
}

func TestOrphanNeverTouchesNomad(t *testing.T) {
	h := newHarness(t)
	h.deployed("web", store.PolicyAuto)
	h.runningInNomad("web", "running")
	before := *h.nomad.live["web"]
	h.detect()
	h.detect()
	if len(h.nomad.registerCalls) != 0 {
		t.Errorf("register calls = %+v: an orphan is reported, never changed", h.nomad.registerCalls)
	}
	after := h.nomad.live["web"]
	if after.Stop != before.Stop || *after.Status != *before.Status {
		t.Error("the live job changed")
	}
}
