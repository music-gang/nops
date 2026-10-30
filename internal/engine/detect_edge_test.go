package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

func withNamespace(j *api.Job, ns string) *api.Job {
	j.Namespace = &ns
	return j
}

func TestLogIssue(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	logIssue(context.Background(), log, "web", parsedFile{path: "web.nomad.hcl", commit: commitRef{repo: "default", sha: "c1"}}, meta.Issue{
		Severity: meta.SeverityError, Key: "nops_policy", Message: "invalid value",
	})
	if out := buf.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "nops_policy") {
		t.Errorf("error issue not logged as ERROR: %s", out)
	}

	buf.Reset()
	logIssue(context.Background(), log, "web", parsedFile{path: "web.nomad.hcl", commit: commitRef{repo: "default", sha: "c1"}}, meta.Issue{
		Severity: meta.SeverityWarn, Key: "nops_typo", Message: "unknown key",
	})
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "nops_typo") {
		t.Errorf("warn issue not logged as WARN: %s", out)
	}
}

// TestMetaIssuesAreLoggedDuringDetection exercises logIssue through a full
// cycle: an unknown nops_ key produces a WARN but does not stop the job from
// being managed normally.
func TestMetaIssuesAreLoggedDuringDetection(t *testing.T) {
	var buf bytes.Buffer
	h := newHarness(t)
	h.engine.log = slog.New(slog.NewTextHandler(&buf, nil))
	h.nomad.setFile("web-v1", managed("web", "auto", map[string]string{"nops_typo": "x"}))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "nops_typo") {
		t.Errorf("unknown key was not logged: %s", out)
	}
	d := h.active("web")
	if d.State != store.StateDetected {
		t.Fatalf("deployment = %+v, an unrelated unknown key must not block it", d)
	}
}

func TestNamespaceMismatchIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", withNamespace(managed("web", "auto", nil), "other"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	h.noActive("web")
	if obs := h.engine.Observations(); len(obs) != 0 {
		t.Errorf("observations = %+v, want none for a job in another namespace", obs)
	}
}

func TestParseErrorSkipsOnlyThatFile(t *testing.T) {
	h := newHarness(t)
	h.nomad.setParseErr("broken", errors.New("invalid HCL"))
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1",
		gitwatch.File{Path: "broken.nomad.hcl", Content: "broken"},
		gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})

	h.detect()

	d := h.active("web")
	if d.State != store.StateDetected {
		t.Fatalf("the good file must still be processed: %+v", d)
	}
}

func TestParsedJobWithoutIDIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("noid", &api.Job{Meta: map[string]string{"nops_managed": "true", "nops_policy": "auto"}})
	h.snap.set("c1", gitwatch.File{Path: "noid.nomad.hcl", Content: "noid"})

	if err := h.engine.Detect(context.Background()); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if obs := h.engine.Observations(); len(obs) != 0 {
		t.Errorf("observations = %+v, want none for a job with no ID", obs)
	}
}

// A Nomad error on one job (reading it, or planning it) keeps the job observed
// with what git says and the error: it can be paused, and nothing that leads to
// a register is taken until a cycle plans it again. Its pending deployment is
// left as it was.
func TestANomadFailureOnOneJobKeepsItObserved(t *testing.T) {
	for _, tc := range []struct {
		name, wantErr string
		fail          func(f *fakeNomad, err error)
	}{
		{"plan fails", "plan job: nomad is unreachable", func(f *fakeNomad, err error) { f.planErr["web"] = err }},
		{"get live job fails", "get live job: nomad is unreachable", func(f *fakeNomad, err error) { f.jobErr["web"] = err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			h.nomad.setFile("web-v1", managed("web", "approval", nil))
			h.nomad.setFile("api-v1", managed("api", "approval", nil))
			h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
			h.snap.set("c1",
				gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"},
				gitwatch.File{Path: "api.nomad.hcl", Content: "api-v1"},
			)
			h.detect()
			d := h.active("web")
			if d.State != store.StatePendingApproval {
				t.Fatalf("setup: deployment = %s, want pending_approval", d.State)
			}

			tc.fail(h.nomad, errors.New("nomad is unreachable"))
			h.detect()

			obs, ok := h.engine.observation(jobKey{testNamespace, "web"})
			if !ok {
				t.Fatal("web has no observation: a Nomad error leaves it out of the cycle")
			}
			if obs.NomadError != tc.wantErr || obs.SpecHash != d.SpecHash || obs.FilePath != "web.nomad.hcl" || obs.Drift || obs.PlanDiff != "" {
				t.Errorf("observation = error %q, spec %q, file %q, drift %v, diff %q; want error %q, the spec and file from git, no drift",
					obs.NomadError, obs.SpecHash, obs.FilePath, obs.Drift, obs.PlanDiff, tc.wantErr)
			}
			if st := h.engine.Status(); st.Skipped != 1 || st.Unparsed != 0 {
				t.Errorf("status skipped = %d, unparsed = %d, want 1 and 0", st.Skipped, st.Unparsed)
			}
			if got := h.get(d.ID); got.State != store.StatePendingApproval {
				t.Errorf("deployment = %s, want it still pending_approval", got.State)
			}
			if DeployNowable(obs, h.get(d.ID)) {
				t.Error("DeployNowable = true for a job Nomad couldn't plan")
			}
			err := h.engine.Approve(ctx, d.ID, d.SpecHash, "alice")
			if !errors.Is(err, ErrNomadError) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Approve = %v, want ErrNomadError naming %q", err, tc.wantErr)
			}
			if err := h.engine.Pause(ctx, testNamespace, "web", "alice", ""); err != nil {
				t.Errorf("Pause = %v, want it to work", err)
			}
			if err := h.engine.Resume(ctx, testNamespace, "web", "alice"); err != nil {
				t.Errorf("Resume = %v, want it to work", err)
			}

			// The next cycle that plans the job makes it approvable again.
			h.nomad.planErr, h.nomad.jobErr = map[string]error{}, map[string]error{}
			h.detect()
			if err := h.engine.Approve(ctx, d.ID, d.SpecHash, "alice"); err != nil {
				t.Errorf("Approve after a cycle that planned the job = %v, want it to work", err)
			}
		})
	}
}

// A failed deployment can't be retried while Nomad can't plan its job: what a
// retry runs depends on the drift, which the cycle couldn't read.
func TestRetryWaitsForACycleThatPlansTheJob(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.detect()
	d := h.active("web")
	if err := h.engine.Reject(context.Background(), d.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	h.detect()
	rejected := h.get(d.ID)
	if !h.engine.Retryable(rejected, rejected) {
		t.Fatal("setup: the rejected deployment is not retryable")
	}

	h.nomad.planErr["web"] = errors.New("nomad is unreachable")
	h.detect()
	if h.engine.Retryable(rejected, rejected) {
		t.Error("Retryable = true while Nomad can't plan the job")
	}
}
