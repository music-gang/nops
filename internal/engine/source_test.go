package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// applyingWithSource is createApplying for a deployment that kept the file and
// var-file its spec was parsed from.
func (h *harness) applyingWithSource(jobID string, job *api.Job, casIndex uint64, source, vars string) *store.Deployment {
	h.t.Helper()
	specJSON := mustMarshal(h.t, job)
	d := &store.Deployment{
		JobID: jobID, Namespace: testNamespace, CommitSHA: "c1", SpecHash: "hash-" + jobID,
		JobSpec: specJSON, JobSource: source, JobVars: vars, Hooks: frozenFromSpec(specJSON),
		Policy: store.PolicyAuto, CASIndex: casIndex,
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		h.t.Fatalf("CreateDeployment: %v", err)
	}
	if err := h.store.Transition(context.Background(), d.ID, store.StateApplying,
		store.Transition{From: store.StateDetected, Actor: "nops"}); err != nil {
		h.t.Fatalf("Transition to applying: %v", err)
	}
	return h.get(d.ID)
}

func TestDetectionKeepsTheFileAndVarsOfTheDeployment(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1", Vars: "count = 2"})

	h.detect()

	d := h.active("web")
	if d.JobSource != "web-v1" {
		t.Errorf("job source = %q, want the file as read from git", d.JobSource)
	}
	if !strings.Contains(d.JobVars, "count = 2") || !strings.Contains(d.JobVars, `nops_commit = "c1"`) {
		t.Errorf("job vars = %q, want the var-file with nops_commit", d.JobVars)
	}
}

func TestStepRegisterSendsTheSourceAtTheNextVersion(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	h.liveApplied("web", job, 5, 3)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	d := h.applyingWithSource("web", job, 5, "web-v2", "count = 2")

	h.step(d)

	if len(h.nomad.registerCalls) != 1 {
		t.Fatalf("register calls = %+v, want 1", h.nomad.registerCalls)
	}
	call := h.nomad.registerCalls[0]
	if call.version != 4 {
		t.Errorf("registered at version %d, want 4: the live one plus one", call.version)
	}
	if sub := call.sub; sub == nil || sub.Source != "web-v2" || sub.Format != "hcl2" || sub.Variables != "count = 2" {
		t.Errorf("submission = %+v, want the file as hcl2 with its var-file", sub)
	}
}

func TestStepRegisterOfADeploymentWithoutSourceSendsNone(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	h.liveApplied("web", job, 5, 3)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	d := h.createApplying("web", job, 5) // created before the source was recorded

	h.step(d)

	if len(h.nomad.registerCalls) != 1 {
		t.Fatalf("register calls = %+v, want 1", h.nomad.registerCalls)
	}
	call := h.nomad.registerCalls[0]
	if call.sub != nil {
		t.Errorf("submission = %+v, want none", call.sub)
	}
	if call.version != 4 {
		t.Errorf("registered at version %d, want 4: without a source it matters most", call.version)
	}
}

func TestStepRegisterOfANewJobStartsAtVersionZero(t *testing.T) {
	h := newHarness(t)
	job := managed("web", "auto", nil)
	h.nomad.setDrift("web", &api.JobDiff{Type: "Added", ID: "web"})
	d := h.applyingWithSource("web", job, 0, "web-v1", "")

	h.step(d)

	if len(h.nomad.registerCalls) != 1 || h.nomad.registerCalls[0].version != 0 {
		t.Fatalf("register calls = %+v, want one at version 0", h.nomad.registerCalls)
	}
}

func TestStepRegisterLogsTheWarningsOfNomad(t *testing.T) {
	h := newHarness(t)
	buf := h.jsonLog()
	h.nomad.registerWarnings = "job source is larger than 1 MB: dropped"
	h.nomad.setDrift("web", &api.JobDiff{Type: "Added", ID: "web"})
	d := h.applyingWithSource("web", managed("web", "auto", nil), 0, "web-v1", "")

	h.step(d)

	if !strings.Contains(buf.String(), `"level":"WARN","msg":"nomad warnings on the register"`) ||
		!strings.Contains(buf.String(), "dropped") {
		t.Errorf("no WARN line with the warnings of Nomad: %s", buf)
	}
}

func TestHookRevisionIsRegisteredWithoutSourceAtTheNextVersion(t *testing.T) {
	h := newHarness(t)
	rev := revisionOfPlainHook("web-migrate")
	idx, version, stop := uint64(9), uint64(2), true
	h.nomad.setLive(&api.Job{ID: &rev, JobModifyIndex: &idx, Version: &version, Stop: &stop, Meta: map[string]string{"nops_role": "hook"}}) // GC'd earlier
	d := h.preHooked()

	h.step(d)

	regs := h.revisionRegisters()
	if len(regs) != 1 {
		t.Fatalf("registered %+v, want the revision once", h.nomad.registerCalls)
	}
	if regs[0].version != 3 || regs[0].sub != nil {
		t.Errorf("revision registered at version %d with submission %+v, want version 3 and none", regs[0].version, regs[0].sub)
	}
}
