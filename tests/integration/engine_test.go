//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// staticSnapshot is a fixed gitwatch.Snapshot: these tests drive Detect
// directly, so there is nothing to poll.
type staticSnapshot struct {
	snap    gitwatch.Snapshot
	changed chan struct{}
}

func (s staticSnapshot) Snapshot() gitwatch.Snapshot { return s.snap }
func (s staticSnapshot) Changed() <-chan struct{}    { return s.changed }

type noopNotifier struct{}

func (noopNotifier) Notify(context.Context, *store.Deployment) {}

// managedEnvHCL is a managed batch job with a pre-hook and a secret
// environment variable, to exercise redaction end to end.
func managedEnvHCL(id, hookID, secret string) string {
	return fmt.Sprintf(`
job %q {
  type = "batch"
  meta {
    nops_managed  = "true"
    nops_policy   = "approval"
    nops_pre_hook = %q
  }
  group "g" {
    task "t" {
      driver = "raw_exec"
      env {
        DB_PASSWORD = %q
      }
      config {
        command = "/bin/sh"
        args    = ["-c", "exit 0"]
      }
    }
  }
}`, id, hookID, secret)
}

func newEngine(t *testing.T, snap staticSnapshot) (*engine.Engine, *store.Store) {
	t.Helper()
	c, _ := newClient(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "nops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	e := engine.New(engine.Options{
		Store: st, Nomad: c, Snapshots: snap, Notifier: noopNotifier{},
		Namespace: "default", DriftInterval: time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return e, st
}

// TestEngineDetectionAgainstRealNomad drives one detection cycle against a
// real Nomad: the hook gets synced, a deployment is created for the managed
// job with its diff redacted (but the full, unredacted spec kept for apply),
// and a second cycle revalidates it once the live job changes outside nops.
func TestEngineDetectionAgainstRealNomad(t *testing.T) {
	c, raw := newClient(t)
	ctx := context.Background()
	jobID := uniqueID(t, raw, "detect")
	hookID := uniqueID(t, raw, "detecthook")
	const secret = "s3cr3t-value"

	snap := staticSnapshot{changed: make(chan struct{}, 1), snap: gitwatch.Snapshot{
		Commit: "c1",
		Files: []gitwatch.File{
			{Path: "job.nomad.hcl", Content: managedEnvHCL(jobID, hookID, secret)},
			{Path: "hook.nomad.hcl", Content: hookHCL(hookID)},
		},
	}}
	e, st := newEngine(t, snap)

	if err := e.Detect(ctx); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	// The hook was registered in Nomad, ready to be dispatched later.
	if _, err := c.Job(ctx, hookID); err != nil {
		t.Fatalf("hook job was not synced: %v", err)
	}

	d, err := st.ActiveDeployment(ctx, "default", jobID)
	if err != nil {
		t.Fatalf("ActiveDeployment: %v", err)
	}
	if d.State != store.StatePendingApproval {
		t.Fatalf("deployment = %+v, want pending_approval", d)
	}
	if strings.Contains(d.PlanDiff, secret) {
		t.Errorf("secret leaked into the stored plan diff: %s", d.PlanDiff)
	}
	if !strings.Contains(d.PlanDiff, "<redacted>") {
		t.Errorf("plan diff was not redacted: %s", d.PlanDiff)
	}
	if !strings.Contains(d.JobSpec, secret) {
		t.Errorf("job_spec must keep the full spec to apply later, got %s", d.JobSpec)
	}
	if d.CASIndex != 0 {
		t.Errorf("cas_index = %d, want 0 (the job did not exist yet)", d.CASIndex)
	}

	// Apply it out of band (standing in for engine-apply, which does not
	// exist yet): the live job's modify index moves.
	parsed, err := c.ParseHCL(ctx, managedEnvHCL(jobID, hookID, secret), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, parsed, d.CASIndex, false); err != nil {
		t.Fatalf("apply out of band: %v", err)
	}

	if err := e.Detect(ctx); err != nil {
		t.Fatalf("second Detect: %v", err)
	}
	got, err := st.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateSuperseded {
		t.Fatalf("deployment after an out-of-band apply = %+v, want superseded", got)
	}
	// The live job now matches git: no new deployment (there is no more drift).
	if _, err := st.ActiveDeployment(ctx, "default", jobID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ActiveDeployment: err = %v, want ErrNotFound (in sync, nothing to approve)", err)
	}
}
