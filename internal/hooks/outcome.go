package hooks

import (
	"fmt"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/nomadx"
)

// verdict is what the state of a dispatched hook job says about its run.
type verdict int

const (
	// verdictRunning means there is no outcome yet: keep waiting (until the timeout).
	verdictRunning verdict = iota
	verdictSucceeded
	verdictFailed
	// verdictStopped means an allocation completed but the server wanted it
	// stopped: someone stopped the job. The runner decides between "failed" and
	// "timed out" (its own stop after the deadline looks the same).
	verdictStopped
)

// evaluate reads the outcome of a dispatched hook job from the child job and its
// allocations. The message explains a failed or stopped verdict.
//
//   - any failed or lost allocation → failed;
//   - a completed allocation the server wanted stopped → stopped;
//   - child dead with every allocation complete → succeeded;
//   - child dead with no allocation at all → failed (it never ran);
//   - anything else, including no allocation yet → running.
//
// Requiring the child to be dead (not just its allocations complete) keeps a
// multi-group hook with a group still pending from being read as a success.
func evaluate(child *api.Job, allocs []nomadx.Alloc) (verdict, string) {
	for _, a := range allocs {
		if a.ClientStatus == "failed" || a.ClientStatus == "lost" {
			msg := a.Failure
			if msg == "" {
				msg = "no details from Nomad"
			}
			return verdictFailed, fmt.Sprintf("allocation %s %s: %s", shortID(a.ID), a.ClientStatus, msg)
		}
	}
	for _, a := range allocs {
		if a.ClientStatus == "complete" && a.DesiredStatus != "run" {
			return verdictStopped, fmt.Sprintf("allocation %s was stopped from outside nops (desired status %q)", shortID(a.ID), a.DesiredStatus)
		}
	}
	if child.Status == nil || *child.Status != "dead" {
		return verdictRunning, ""
	}
	if len(allocs) == 0 {
		return verdictFailed, "hook job stopped before any allocation ran"
	}
	for _, a := range allocs {
		if a.ClientStatus != "complete" {
			return verdictRunning, ""
		}
	}
	return verdictSucceeded, ""
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
