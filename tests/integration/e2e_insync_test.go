//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// scaledAutoHCL is a managed service job under auto whose group has a scaling
// block, with no rolling update (see scalableHCL).
func scaledAutoHCL(id string) string {
	return fmt.Sprintf(`
job %q {
  type = "service"
  update {
    max_parallel = 0
  }
  meta {
    nops_managed = "true"
    nops_policy  = "auto"
  }
  group "g" {
    count = 1
    scaling {
      min     = 1
      max     = 3
      enabled = true
    }
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}
`, id)
}

// TestE2EAJobWithAScalingBlockIsRegisteredOnce: under auto, nops registers a
// new job with a scaling block once, then leaves it alone. A second register
// of the unchanged job would make a new job version.
func TestE2EAJobWithAScalingBlockIsRegisteredOnce(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "insync")

	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): scaledAutoHCL(jobID)})
	e.waitNew(jobID, "", store.StateCompleted)
	index := e.liveIndex(jobID)

	time.Sleep(2 * time.Second) // ten detection cycles
	if n := e.deploymentsOf(jobID); n != 1 {
		t.Errorf("deployments of an unchanged job = %d, want 1; latest: %s", n, describe(e.latest(jobID)))
	}
	if got := e.liveIndex(jobID); got != index {
		t.Errorf("live index moved %d -> %d with nothing to deploy", index, got)
	}
}
