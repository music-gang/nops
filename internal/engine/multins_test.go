package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// Several managed namespaces: a job is (namespace, ID), so the same ID in two
// namespaces is two jobs, and every read and write of Nomad and of the store
// carries the namespace of the job it is about.

func inNS(j *api.Job, ns string) *api.Job { return withNamespace(j, ns) }

// liveIn puts a job in the fake cluster, in namespace ns, at a modify index.
func (h *harness) liveIn(ns, id string, index uint64, meta map[string]string) {
	h.nomad.setLive(&api.Job{ID: &id, Namespace: &ns, JobModifyIndex: &index, Meta: meta})
}

func (h *harness) latestIn(ns, jobID string) *store.Deployment {
	h.t.Helper()
	d, err := h.store.LatestDeployment(context.Background(), ns, jobID)
	if err != nil {
		h.t.Fatalf("LatestDeployment(%s/%s): %v", ns, jobID, err)
	}
	return d
}

func TestSameJobIDInTwoNamespacesIsTwoJobs(t *testing.T) {
	h := newHarnessIn(t, "apps", "infra")
	h.nomad.setFile("web-apps", inNS(managed("web", "auto", nil), "apps"))
	h.nomad.setFile("web-infra", inNS(managed("web", "approval", nil), "infra"))
	h.nomad.setDrift("apps/web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.nomad.setDrift("infra/web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.liveIn("apps", "web", 5, nil) // only apps/web is live: the two must not be mixed up
	h.snap.set("c1",
		gitwatch.File{Path: "apps/web.nomad.hcl", Content: "web-apps"},
		gitwatch.File{Path: "infra/web.nomad.hcl", Content: "web-infra"})

	h.detect()

	obs := h.engine.Observations()
	if len(obs) != 2 || obs[0].Namespace != "apps" || obs[1].Namespace != "infra" {
		t.Fatalf("observations = %+v, want apps/web then infra/web", obs)
	}
	if !obs[0].Drift || !obs[1].Drift {
		t.Errorf("drift = %v, %v, want both", obs[0].Drift, obs[1].Drift)
	}
	apps, infra := h.latestIn("apps", "web"), h.latestIn("infra", "web")
	if apps.State != store.StateDetected || apps.CASIndex != 5 {
		t.Errorf("apps/web = %s at index %d, want detected at 5", apps.State, apps.CASIndex)
	}
	if infra.State != store.StatePendingApproval || infra.CASIndex != 0 {
		t.Errorf("infra/web = %s at index %d, want pending_approval at 0", infra.State, infra.CASIndex)
	}
	if st := h.engine.Status(); st.Managed != 2 || st.Unparsed != 0 {
		t.Errorf("Status = %+v, want 2 managed and none unparsed", st)
	}
	if apps.Namespace != "apps" || infra.Namespace != "infra" {
		t.Errorf("namespaces = %s, %s", apps.Namespace, infra.Namespace)
	}
}

// Two files parsing to the same (namespace, ID) are ignored, as before; the
// same ID in another namespace is no clash.
func TestDuplicateJobIDClashesOnlyInsideANamespace(t *testing.T) {
	h := newHarnessIn(t, "apps", "infra")
	h.nomad.setFile("a1", inNS(managed("web", "auto", nil), "apps"))
	h.nomad.setFile("a2", inNS(managed("web", "auto", map[string]string{"x": "y"}), "apps"))
	h.nomad.setFile("i1", inNS(managed("web", "auto", nil), "infra"))
	h.snap.set("c1",
		gitwatch.File{Path: "a1.nomad.hcl", Content: "a1"},
		gitwatch.File{Path: "a2.nomad.hcl", Content: "a2"},
		gitwatch.File{Path: "i1.nomad.hcl", Content: "i1"})

	h.detect()

	obs := h.engine.Observations()
	if len(obs) != 1 || obs[0].Namespace != "infra" {
		t.Fatalf("observations = %+v, want only infra/web", obs)
	}
}

func TestUnlistedNamespaceIsAnError(t *testing.T) {
	var buf bytes.Buffer
	h := newHarnessIn(t, "apps")
	h.engine.log = slog.New(slog.NewTextHandler(&buf, nil))
	h.nomad.setFile("infra-v1", inNS(managed("web", "auto", nil), "infra"))
	// No namespace in the HCL: Nomad's canonical one is "default", not listed.
	bare := managed("api", "auto", nil)
	bare.Namespace = nil
	h.nomad.setFile("api-v1", bare)
	h.nomad.setFile("ok-v1", inNS(managed("db", "auto", nil), "apps"))
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "infra-v1"},
		gitwatch.File{Path: "api.nomad.hcl", Content: "api-v1"},
		gitwatch.File{Path: "db.nomad.hcl", Content: "ok-v1"})

	h.detect()

	out := buf.String()
	for _, want := range []string{"level=ERROR", "does not manage", "declared_namespace=infra", "declared_namespace=default"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	obs := h.engine.Observations()
	if len(obs) != 1 || obs[0].JobID != "db" || obs[0].Namespace != "apps" {
		t.Errorf("observations = %+v, want only apps/db", obs)
	}
	if st := h.engine.Status(); st.Unparsed != 2 || !st.OrphanCheckSkipped {
		t.Errorf("Status = %+v, want 2 unparsed and the orphan check skipped", st)
	}
}

func TestJobWithoutNamespaceIsInDefault(t *testing.T) {
	h := newHarness(t)
	j := managed("web", "auto", nil)
	j.Namespace = nil
	h.nomad.setFile("web-v1", j)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	if d := h.active("web"); d.Namespace != "default" || !strings.Contains(d.JobSpec, `"Namespace":"default"`) {
		t.Errorf("deployment = %+v, want the default namespace, in the spec too", d)
	}
}

// A hook is found among the hooks of the job's own namespace.
func TestHookIsLookedUpInTheJobsNamespace(t *testing.T) {
	h := newHarnessIn(t, "apps", "infra")
	h.nomad.setFile("web-v1", inNS(managed("web", "auto", map[string]string{"nops_pre_hook": "migrate"}), "apps"))
	h.nomad.setFile("migrate-v1", inNS(hookJob("migrate"), "infra"))
	h.nomad.setDrift("apps/web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
		gitwatch.File{Path: "migrate.nomad.hcl", Content: "migrate-v1"})

	h.detect()

	d := h.latestIn("apps", "web")
	if d.State != store.StateFailed || !strings.Contains(d.Error, `pre-hook "migrate" not found in repo`) {
		t.Fatalf("deployment = %s (%q), want failed: the hook is in another namespace", d.State, d.Error)
	}
	if obs := h.engine.Observations(); len(obs) != 1 || obs[0].PreHooks[0].Timeout != 0 {
		t.Errorf("observations = %+v, want the hook shown as not in the repository", obs)
	}

	// The same hook, in the job's namespace: found and frozen.
	h.nomad.setFile("migrate-apps-v1", inNS(hookJob("migrate"), "apps"))
	h.nomad.setFile("web-v2", inNS(managed("web", "auto", map[string]string{"nops_pre_hook": "migrate", "v": "2"}), "apps"))
	h.snap.set("c2",
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v2"},
		gitwatch.File{Path: "migrate.nomad.hcl", Content: "migrate-v1"},
		gitwatch.File{Path: "migrate-apps.nomad.hcl", Content: "migrate-apps-v1"})
	h.detect()

	d = h.latestIn("apps", "web")
	frozen, err := h.store.DeploymentHooks(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != store.StateDetected || len(frozen) != 1 || frozen[0].HookID != "migrate" ||
		!strings.Contains(frozen[0].JobSpec, `"Namespace":"apps"`) {
		t.Fatalf("deployment = %s with hooks %+v, want detected with the apps hook frozen", d.State, frozen)
	}
}

// deployedIn records a completed deployment in namespace ns.
func (h *harness) deployedIn(ns, jobID string) {
	h.t.Helper()
	ctx := context.Background()
	d := &store.Deployment{
		JobID: jobID, Namespace: ns, CommitSHA: "c0", SpecHash: "h-" + ns + jobID,
		JobSpec: `{"ID":"` + jobID + `"}`, Policy: store.PolicyAuto,
	}
	if err := h.store.CreateDeployment(ctx, d); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.Transition(ctx, d.ID, store.StateCompleted, store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		h.t.Fatal(err)
	}
}

func TestOrphansAreFoundInEveryManagedNamespace(t *testing.T) {
	h := newHarnessIn(t, "apps", "infra")
	h.deployedIn("apps", "web")
	h.deployedIn("infra", "web")
	h.deployedIn("infra", "dns")
	h.liveIn("apps", "web", 3, nil)
	h.liveIn("infra", "web", 3, nil)
	h.liveIn("infra", "dns", 3, nil)
	// infra/web is still in git; that does not shield apps/web, the same ID.
	h.nomad.setFile("web-infra", inNS(managed("web", "auto", nil), "infra"))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-infra"})

	h.detect()

	got := h.engine.Orphans()
	if len(got) != 2 || got[0].Namespace != "apps" || got[0].JobID != "web" || got[1].Namespace != "infra" || got[1].JobID != "dns" {
		t.Fatalf("orphans = %+v, want apps/web then infra/dns", got)
	}
}

// A deployment of a namespace nops does not manage (it was taken off the list)
// is left where it is: not superseded when its job is not in git, not an orphan.
func TestNamespaceLeftOffTheListIsLeftAlone(t *testing.T) {
	h := newHarnessIn(t, "apps")
	ctx := context.Background()
	pending := &store.Deployment{
		JobID: "web", Namespace: "infra", CommitSHA: "c0", SpecHash: "h", JobSpec: "{}", Policy: store.PolicyApproval,
	}
	if err := h.store.CreateDeployment(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(ctx, pending.ID, store.StatePendingApproval, store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	h.deployedIn("infra", "dns")
	h.liveIn("infra", "dns", 3, nil)
	h.snap.set("c1")

	h.detect()

	if got := h.latestIn("infra", "web"); got.State != store.StatePendingApproval {
		t.Errorf("infra/web = %s, want it untouched", got.State)
	}
	if got := h.engine.Orphans(); len(got) != 0 {
		t.Errorf("orphans = %+v, want none in a namespace nops does not manage", got)
	}
}

func TestGCRunsInEveryManagedNamespace(t *testing.T) {
	h := newHarnessIn(t, "apps", "infra")
	hook := map[string]string{"nops_role": "hook"}
	h.liveIn("apps", "web-migrate-0a1b2c3d", 3, hook)
	h.liveIn("infra", "web-migrate-0a1b2c3d", 3, hook)
	h.liveIn("other", "web-migrate-0a1b2c3d", 3, hook) // not managed: not touched

	h.gc()

	got := h.nomad.stopCalls
	if len(got) != 2 || got[0] != "apps/web-migrate-0a1b2c3d" || got[1] != "infra/web-migrate-0a1b2c3d" {
		t.Errorf("stopped %v, want the revision of apps and of infra", got)
	}
}

// createDetectedIn creates a detected deployment of a job of namespace ns.
func (h *harness) createDetectedIn(ns, jobID, spec string, policy store.Policy) *store.Deployment {
	h.t.Helper()
	d := &store.Deployment{
		JobID: jobID, Namespace: ns, CommitSHA: "c1", SpecHash: "hash-" + ns + jobID,
		JobSpec: spec, Hooks: frozenFromSpec(spec), Policy: policy,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		h.t.Fatal(err)
	}
	return d
}

// Apply reads the live job and registers in the deployment's own namespace,
// even if the stored spec says nothing about it.
func TestApplyActsInTheDeploymentsNamespace(t *testing.T) {
	h := newHarnessIn(t, "apps", "infra")
	spec := mustMarshal(t, managed("web", "auto", nil)) // namespace "default" in the spec
	h.liveIn("apps", "web", 7, nil)                     // must not be read for the infra deployment
	h.nomad.setDrift("infra/web", &api.JobDiff{Type: "Edited", ID: "web"})
	d := h.createDetectedIn("infra", "web", spec, store.PolicyAuto)

	h.step(d)                                      // detected -> applying
	h.step(h.get(d.ID))                            // register
	if got := h.get(d.ID); got.AppliedIndex == 0 { // SetApplied ran: registered
		t.Fatalf("deployment = %+v, want it applied", got)
	}

	calls := h.nomad.registerCalls
	if len(calls) != 1 || calls[0].namespace != "infra" || calls[0].id != "web" || calls[0].index != 0 {
		t.Fatalf("register calls = %+v, want one in infra at index 0", calls)
	}
	if _, ok := h.nomad.live["infra/web"]; !ok {
		t.Error("infra/web is not registered")
	}
	if got := h.nomad.live["web"]; got != nil {
		t.Error("something was registered in the default namespace")
	}
}

func TestApplyCycleSkipsAnUnlistedNamespace(t *testing.T) {
	h := newHarnessIn(t, "apps")
	spec := mustMarshal(t, managed("web", "auto", nil))
	listed := h.createDetectedIn("apps", "web", spec, store.PolicyAuto)
	unlisted := h.createDetectedIn("infra", "web", spec, store.PolicyAuto)

	h.engine.applyCycle(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for h.get(listed.ID).State == store.StateDetected {
		if time.Now().After(deadline) {
			t.Fatal("the deployment of the listed namespace was not advanced")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The goroutines are done once nothing is in flight.
	for {
		h.engine.applyMu.Lock()
		busy := len(h.engine.inFlight)
		h.engine.applyMu.Unlock()
		if busy == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.get(unlisted.ID); got.State != store.StateDetected {
		t.Errorf("infra/web = %s, want it left alone", got.State)
	}
}

func TestRetryInASecondNamespace(t *testing.T) {
	h := newHarnessIn(t, "apps", "infra")
	ctx := context.Background()
	j := inNS(managed("web", "auto", nil), "infra")
	h.nomad.setFile("web-v1", j)
	h.nomad.setDrift("infra/web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	hash, err := specHash(j)
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Deployment{JobID: "web", Namespace: "infra", CommitSHA: "c1", SpecHash: hash, JobSpec: `{"ID":"web"}`, Policy: store.PolicyAuto}
	if err := h.store.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Transition(ctx, d.ID, store.StateFailed, store.Transition{From: store.StateDetected, Actor: "nops", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	h.detect()
	if obs := h.engine.Observations(); len(obs) != 1 || obs[0].BlockedBy != d.ID {
		t.Fatalf("setup: observations = %+v, want infra/web blocked by %s", obs, d.ID)
	}

	if err := h.engine.Retry(ctx, "apps", "web", "iacopo"); !errors.Is(err, ErrNotBlocked) {
		t.Errorf("apps/web: err = %v, want ErrNotBlocked: a job of the same ID in another namespace", err)
	}
	if err := h.engine.Retry(ctx, "other", "web", "iacopo"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("other/web: err = %v, want store.ErrNotFound", err)
	}
	if err := h.engine.Retry(ctx, "infra", "web", "iacopo"); err != nil {
		t.Fatalf("infra/web: %v", err)
	}
}
