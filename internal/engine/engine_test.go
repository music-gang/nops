package engine

import (
	"context"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/gitwatch"
)

// waitUntil polls cond until it is true or the deadline passes.
func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func runDetectionInBackground(t *testing.T, e *Engine) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.RunDetection(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunDetection did not stop after ctx was cancelled")
		}
	}
}

// TestRunDetectionInitialAndTicker checks that RunDetection runs a cycle
// right away and again on every DriftInterval tick.
func TestRunDetectionInitialAndTicker(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.engine.driftInterval = 20 * time.Millisecond

	stop := runDetectionInBackground(t, h.engine)
	defer stop()

	waitUntil(t, func() bool { return h.nomad.callCount() > 0 }, "RunDetection did not run an initial cycle")
	after := h.nomad.callCount()
	waitUntil(t, func() bool { return h.nomad.callCount() > after }, "RunDetection did not run on the drift ticker")
}

// TestRunDetectionOnChanged checks that a Changed signal runs a cycle
// promptly, without waiting for the (here, very long) drift interval.
func TestRunDetectionOnChanged(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-v1", managed("web", "auto", nil))
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-v1"})
	h.engine.driftInterval = time.Hour

	stop := runDetectionInBackground(t, h.engine)
	defer stop()

	waitUntil(t, func() bool { return h.nomad.callCount() > 0 }, "RunDetection did not run the initial cycle")
	before := h.nomad.callCount()
	h.snap.changed <- struct{}{}
	waitUntil(t, func() bool { return h.nomad.callCount() > before }, "RunDetection did not run on a Changed signal")
}
