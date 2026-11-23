package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// endedDeployment stores a failed deployment of job, with its spec.
func endedDeployment(h *harness, job string) *store.Deployment {
	h.t.Helper()
	d := &store.Deployment{
		JobID: job, Namespace: testNamespace, CommitSHA: "c0", SpecHash: "hash-" + job,
		JobSpec: `{"ID":"` + job + `"}`, Policy: store.PolicyAuto, State: store.StateFailed, Error: "x",
	}
	if err := h.store.CreateDeployment(context.Background(), d); err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) specKept(d *store.Deployment) bool {
	h.t.Helper()
	got, err := h.store.GetDeployment(context.Background(), d.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return got.JobSpec != ""
}

func TestDetectCycleDropsEndedSpecs(t *testing.T) {
	h := newHarness(t)
	h.engine.specRetention = time.Hour
	old := endedDeployment(h, "old")
	h.clock.Advance(2 * time.Hour)
	recent := endedDeployment(h, "recent")
	h.snap.set("c1")

	h.detect()

	if h.specKept(old) {
		t.Error("a deployment that ended before the retention period kept its spec")
	}
	if !h.specKept(recent) {
		t.Error("a deployment that ended within the retention period lost its spec")
	}
}

func TestDetectCycleKeepsSpecsWithNoRetention(t *testing.T) {
	h := newHarness(t)
	old := endedDeployment(h, "old")
	h.clock.Advance(24 * 365 * time.Hour)
	h.snap.set("c1")

	h.detect()

	if !h.specKept(old) {
		t.Error("with spec_retention 0, a deployment lost its spec")
	}
}

type failingDrop struct{ Store }

func (failingDrop) DropEndedSpecs(context.Context, time.Time) (int, error) {
	return 0, errors.New("disk on fire")
}

func TestDropEndedSpecsStoreFailureFailsTheCycle(t *testing.T) {
	h := newHarness(t)
	h.engine.specRetention = time.Hour
	h.engine.store = failingDrop{Store: h.engine.store}
	h.snap.set("c1")

	if err := h.engine.Detect(context.Background()); err == nil {
		t.Fatal("a store failure must fail the cycle")
	}
}
