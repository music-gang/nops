//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// newNamespace creates a Nomad namespace for the test and removes it, with
// every job in it, at cleanup. It is registered before nops starts so it runs
// after nops is stopped.
func newNamespace(t *testing.T, raw *api.Client, prefix string) string {
	t.Helper()
	ns := uniqueID(t, raw, prefix)
	if _, err := raw.Namespaces().Register(&api.Namespace{Name: ns}, nil); err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		q := &api.QueryOptions{Namespace: ns}
		stubs, _, _ := raw.Jobs().List(q)
		for _, s := range stubs {
			raw.Jobs().Deregister(s.ID, true, &api.WriteOptions{Namespace: ns})
		}
		raw.Namespaces().Delete(ns, nil)
	})
	return ns
}

// inNamespace puts a namespace stanza in a job's HCL.
func inNamespace(hcl, ns string) string {
	return strings.Replace(hcl, `type = "batch"`, "namespace = "+`"`+ns+`"`+"\n  type = \"batch\"", 1)
}

// liveIn is the job of namespace ns in Nomad, nil if it is not registered.
func (e *e2eEnv) liveIn(ns, jobID string) *api.Job {
	e.t.Helper()
	job, err := e.nomad.Job(context.Background(), ns, jobID)
	if errors.Is(err, nomadx.ErrJobNotFound) {
		return nil
	}
	if err != nil {
		e.t.Fatalf("Job(%s/%s): %v", ns, jobID, err)
	}
	return job
}

// One nops instance manages every namespace it is given: the same job ID in
// two of them is two jobs, each deployed by its own policy, with its hook in
// its own namespace; a job in a namespace nops is not given, or with none
// (Nomad's "default"), is refused and never registered.
func TestE2EOneInstanceManagesSeveralNamespaces(t *testing.T) {
	_, raw := newClient(t)
	nsA, nsB := newNamespace(t, raw, "nsa"), newNamespace(t, raw, "nsb")
	e := newE2E(t, "NOPS_NOMAD_NAMESPACES="+nsA+","+nsB)
	jobID := uniqueID(t, e.raw, "multins")
	hookID := uniqueID(t, e.raw, "multinshook")

	e.repo.commit(t, "the same job in three namespaces", map[string]string{
		"a/" + file(jobID):  e2eJob{id: jobID, namespace: nsA, policy: "auto", version: "1", postHook: hookID}.hcl(),
		"a/" + file(hookID): inNamespace(hookCmdHCL(hookID, "exit 0", true), nsA),
		"b/" + file(jobID):  e2eJob{id: jobID, namespace: nsB, policy: "approval", version: "1"}.hcl(),
		// No namespace: "default", which nops was not given.
		"c/" + file(jobID): e2eJob{id: jobID, policy: "auto", version: "1"}.hcl(),
	})

	// nsA is auto: applied, its post-hook run in nsA.
	dA := e.waitNewIn(nsA, jobID, "", store.StateCompleted)
	if got := e.liveIn(nsA, jobID); got == nil || got.Meta["version"] != "1" {
		t.Errorf("%s/%s live = %+v, want version 1", nsA, jobID, got)
	}
	if hooks := e.frozenHooks(dA.ID); len(hooks) != 1 {
		t.Fatalf("frozen hooks of %s = %+v, want the post-hook", dA.ID, hooks)
	}
	revs, _, err := e.raw.Jobs().PrefixList(hookID)
	if err != nil || len(revs) != 0 {
		t.Errorf("hook revisions in the default namespace = %v (%v), want none: the hook lives in %s", revs, err, nsA)
	}
	revs, _, err = e.raw.Jobs().List(&api.QueryOptions{Namespace: nsA, Prefix: hookID})
	if err != nil || len(revs) == 0 {
		t.Errorf("hook revisions in %s = %v (%v), want the revision that ran", nsA, revs, err)
	}

	// nsB is approval: waiting, and nothing of it is live before a decision.
	dB := e.waitNewIn(nsB, jobID, "", store.StatePendingApproval)
	if got := e.liveIn(nsB, jobID); got != nil {
		t.Fatalf("%s/%s is live before anyone approved it", nsB, jobID)
	}
	if status := e.dash.approve(t, dB.ID, dB.SpecHash); status != http.StatusSeeOther {
		t.Fatalf("approve: status %d, want 303", status)
	}
	e.waitState(dB.ID, store.StateCompleted)
	if got := e.liveIn(nsB, jobID); got == nil || got.Meta["version"] != "1" {
		t.Errorf("%s/%s live = %+v, want version 1", nsB, jobID, got)
	}

	// Each job has its own page; the one in the namespace nops was not given
	// has none, and was never registered.
	for _, ns := range []string{nsA, nsB} {
		if status, _ := e.dash.get(t, "/jobs/"+ns+"/"+jobID); status != http.StatusOK {
			t.Errorf("/jobs/%s/%s: status %d, want 200", ns, jobID, status)
		}
	}
	if status, _ := e.dash.get(t, "/jobs/default/"+jobID); status != http.StatusNotFound {
		t.Errorf("/jobs/default/%s: status %d, want 404", jobID, status)
	}
	if got := e.liveIn("default", jobID); got != nil {
		t.Errorf("default/%s was registered: nops does not manage that namespace", jobID)
	}
	if d := e.latest(jobID); d != nil {
		t.Errorf("default/%s has a deployment: %s", jobID, describe(d))
	}
}
