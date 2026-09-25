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
	"github.com/music-gang/nops/internal/hooks"
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

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := engine.New(engine.Options{
		Store: st, Nomad: c, Snapshots: snap, Notifier: noopNotifier{},
		Hooks:     hooks.New(c, st, log, 200*time.Millisecond),
		Namespace: "default", DriftInterval: time.Hour, EngineInterval: 200 * time.Millisecond, ApplyTimeout: time.Minute,
		Log: log,
	})
	return e, st
}

// TestEngineDetectionAgainstRealNomad drives one detection cycle against a
// real Nomad: a deployment is created for the managed job, freezing the hook
// it runs without registering anything of it, with its diff redacted (but the full, unredacted spec kept for apply),
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

	d, err := st.ActiveDeployment(ctx, "default", jobID)
	if err != nil {
		t.Fatalf("ActiveDeployment: %v", err)
	}
	// The hook is frozen with the deployment, and nothing of it is in Nomad:
	// it is registered right before it is dispatched, once approved.
	frozen, err := st.DeploymentHooks(ctx, d.ID)
	if err != nil || len(frozen) != 1 || frozen[0].HookID != hookID || !strings.HasPrefix(frozen[0].Revision, hookID+"-") {
		t.Fatalf("frozen hooks = %+v, err %v; want the hook and a revision id-<hash>", frozen, err)
	}
	if stubs, _, err := raw.Jobs().PrefixList(hookID); err != nil || len(stubs) != 0 {
		t.Fatalf("Nomad has %d jobs for the hook before approval (err %v), want none", len(stubs), err)
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

	// Apply it out of band, standing in for a human approving it and
	// engine-apply carrying it out (this test only exercises detection): the
	// live job's modify index moves.
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

// managedBatchHCL is a lightweight raw_exec batch job, managed with policy
// auto and no hooks. A batch job produces no Nomad deployment, so applying it
// exercises engine-apply's allocations-based health path.
func managedBatchHCL(id string) string {
	return fmt.Sprintf(`
job %q {
  type = "batch"
  meta {
    nops_managed = "true"
    nops_policy  = "auto"
  }
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "exit 0"]
      }
    }
  }
}`, id)
}

// TestEngineApplyAgainstRealNomad drives a managed job with no hooks from
// detection through a real apply to completed: detected -> applying
// (a fresh plan and CAS register) -> healthy, via the allocations of the
// applied version since a batch job has no Nomad deployment -> completed.
func TestEngineApplyAgainstRealNomad(t *testing.T) {
	ctx := context.Background()
	c, raw := newClient(t)
	jobID := uniqueID(t, raw, "apply")

	snap := staticSnapshot{changed: make(chan struct{}, 1), snap: gitwatch.Snapshot{
		Commit: "c1",
		Files:  []gitwatch.File{{Path: "job.nomad.hcl", Content: managedBatchHCL(jobID)}},
	}}
	e, st := newEngine(t, snap)

	if err := e.Detect(ctx); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	d, err := st.ActiveDeployment(ctx, "default", jobID)
	if err != nil {
		t.Fatalf("ActiveDeployment: %v", err)
	}
	if d.State != store.StateDetected || d.Policy != store.PolicyAuto {
		t.Fatalf("deployment = %+v, want detected/auto", d)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.RunApply(runCtx)

	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := st.GetDeployment(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == store.StateCompleted {
			if got.AppliedIndex == 0 {
				t.Errorf("completed with no applied_index recorded")
			}
			break
		}
		if got.State == store.StateFailed {
			t.Fatalf("deployment failed: %s", got.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not reach completed in time, last state = %+v", got)
		}
		time.Sleep(100 * time.Millisecond)
	}

	live, err := c.Job(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if live.JobModifyIndex == nil || *live.JobModifyIndex == 0 {
		t.Errorf("live job was not registered")
	}
}
