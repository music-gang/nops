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

	logIssue(context.Background(), log, "web", "web.nomad.hcl", "c1", meta.Issue{
		Severity: meta.SeverityError, Key: "nops_policy", Message: "invalid value",
	})
	if out := buf.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "nops_policy") {
		t.Errorf("error issue not logged as ERROR: %s", out)
	}

	buf.Reset()
	logIssue(context.Background(), log, "web", "web.nomad.hcl", "c1", meta.Issue{
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
