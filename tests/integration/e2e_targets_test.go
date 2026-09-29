//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

// A target job that never has an allocation of its own has nothing to wait for
// once it is registered: a periodic or parameterized job only spawns children
// (or waits to be dispatched), and a group whose count is 0 places nothing.
// apply must complete such a deployment instead of waiting for an allocation
// that never comes until -apply-timeout fails it.
func TestApplyOfATargetThatNeverHasAnAllocationCompletes(t *testing.T) {
	cases := map[string]string{
		"periodic": `
  type = "batch"
  periodic {
    cron             = "0 0 1 1 *"
    prohibit_overlap = true
  }`,
		"parameterized": `
  type = "batch"
  parameterized {}`,
		"zero count": `
  type = "service"`,
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, raw := newClient(t)
			jobID := uniqueID(t, raw, "notarget")

			count := "1"
			if name == "zero count" {
				count = "0"
			}
			hcl := fmt.Sprintf(`
job %q {
%s
  meta {
    nops_managed = "true"
    nops_policy  = "auto"
  }
  group "g" {
    count = %s
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "exit 0"]
      }
    }
  }
}`, jobID, header, count)

			snap := staticSnapshot{changed: make(chan struct{}, 1), snap: gitwatch.Snapshot{
				Commit: "c1", Files: []gitwatch.File{{Path: "job.nomad.hcl", Content: hcl}},
			}}
			e, st := newEngineWithApplyTimeout(t, snap, 4*time.Second)
			if err := e.Detect(ctx); err != nil {
				t.Fatalf("Detect: %v", err)
			}
			d, err := st.ActiveDeployment(ctx, "default", jobID)
			if err != nil {
				t.Fatalf("ActiveDeployment: %v", err)
			}

			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			go e.RunApply(runCtx)

			deadline := time.Now().Add(15 * time.Second)
			for {
				got, err := st.GetDeployment(ctx, d.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.State == store.StateCompleted {
					return
				}
				if got.State == store.StateFailed {
					t.Fatalf("deployment failed: %s", got.Error)
				}
				if time.Now().After(deadline) {
					t.Fatalf("deployment did not finish in time: %+v", got)
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}

// A system job runs on every eligible node and a sysbatch job runs once on each
// (Nomad 1.11+ tracks a system job with a Nomad deployment of its own). Nops
// does not look at the job type, so both are applied like any other job: the
// first version and an update of it, which replaces the allocations already on
// the node, must each reach completed.
func TestApplyOfASystemOrSysbatchJobCompletes(t *testing.T) {
	cases := map[string]string{
		"system":   "sleep 600",
		"sysbatch": "exit 0",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, raw := newClient(t)
			jobID := uniqueID(t, raw, name)

			for _, tag := range []string{"v1", "v2"} {
				hcl := fmt.Sprintf(`
job %q {
  type = %q
  meta {
    nops_managed = "true"
    nops_policy  = "auto"
    tag          = %q
  }
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", %q]
      }
    }
  }
}`, jobID, name, tag, script)

				snap := staticSnapshot{changed: make(chan struct{}, 1), snap: gitwatch.Snapshot{
					Commit: tag, Files: []gitwatch.File{{Path: "job.nomad.hcl", Content: hcl}},
				}}
				e, st := newEngineWithApplyTimeout(t, snap, 30*time.Second)
				if err := e.Detect(ctx); err != nil {
					t.Fatalf("%s: Detect: %v", tag, err)
				}
				d, err := st.ActiveDeployment(ctx, "default", jobID)
				if err != nil {
					t.Fatalf("%s: ActiveDeployment: %v", tag, err)
				}

				runCtx, cancel := context.WithCancel(ctx)
				go e.RunApply(runCtx)

				deadline := time.Now().Add(45 * time.Second)
				for {
					got, err := st.GetDeployment(ctx, d.ID)
					if err != nil {
						cancel()
						t.Fatal(err)
					}
					if got.State == store.StateCompleted {
						break
					}
					if got.State == store.StateFailed {
						cancel()
						t.Fatalf("%s: deployment failed: %s", tag, got.Error)
					}
					if time.Now().After(deadline) {
						cancel()
						t.Fatalf("%s: deployment did not finish in time: %+v", tag, got)
					}
					time.Sleep(200 * time.Millisecond)
				}
				cancel()
			}
		})
	}
}
