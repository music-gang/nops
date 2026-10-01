package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// TestStatusSucceededAtSurvivesAFailedCycle checks that a cycle aborted by a
// store failure moves At but leaves SucceededAt at the last cycle that ran to
// the end: that is what tells "alive but stuck" from "working".
func TestStatusSucceededAtSurvivesAFailedCycle(t *testing.T) {
	h := newHarness(t)
	if st := h.engine.Status(); !st.SucceededAt.IsZero() {
		t.Fatalf("SucceededAt = %v before any cycle, want zero", st.SucceededAt)
	}

	h.detect()
	ok := h.engine.Status()
	if ok.At.IsZero() || !ok.SucceededAt.Equal(ok.At) {
		t.Fatalf("after a good cycle: At = %v, SucceededAt = %v, want the same", ok.At, ok.SucceededAt)
	}

	h.clock.Advance(time.Minute)
	good := h.engine.store
	h.engine.store = failingLatestCompleted{Store: good}
	if err := h.engine.Detect(context.Background()); err == nil {
		t.Fatal("Detect with a failing store: want an error")
	}
	st := h.engine.Status()
	if !st.At.After(ok.At) {
		t.Errorf("At = %v, want the failed cycle's, after %v", st.At, ok.At)
	}
	if !st.SucceededAt.Equal(ok.At) {
		t.Errorf("SucceededAt = %v after a failed cycle, want the last good one's %v", st.SucceededAt, ok.At)
	}

	h.clock.Advance(time.Minute)
	h.engine.store = good
	h.detect()
	if st := h.engine.Status(); !st.SucceededAt.Equal(st.At) {
		t.Errorf("SucceededAt = %v after the store came back, want its At %v", st.SucceededAt, st.At)
	}
}

// TestApplySucceededAtOnlyWhenTheActiveDeploymentsAreRead checks that an apply
// cycle that cannot list the active deployments does not count as one that
// worked.
func TestApplySucceededAtOnlyWhenTheActiveDeploymentsAreRead(t *testing.T) {
	h := newHarness(t)
	good := h.engine.store
	h.engine.store = failingListActive{Store: good}
	h.engine.applyCycle(context.Background())
	if got := h.engine.ApplySucceededAt(); !got.IsZero() {
		t.Fatalf("ApplySucceededAt = %v after a cycle that could not list, want zero", got)
	}

	h.engine.store = good
	before := h.clock.Now()
	h.engine.applyCycle(context.Background())
	ok := h.engine.ApplySucceededAt()
	if !ok.After(before) || !ok.Before(h.clock.Now()) {
		t.Fatalf("ApplySucceededAt = %v, want the time of the cycle just run (after %v)", ok, before)
	}

	h.clock.Advance(time.Minute)
	h.engine.store = failingListActive{Store: good}
	h.engine.applyCycle(context.Background())
	if got := h.engine.ApplySucceededAt(); !got.Equal(ok) {
		t.Errorf("ApplySucceededAt = %v after a failed cycle, want the last good one's %v", got, ok)
	}
}

type failingListActive struct{ Store }

func (failingListActive) ListActive(context.Context) ([]*store.Deployment, error) {
	return nil, errors.New("disk on fire")
}
