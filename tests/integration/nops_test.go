//go:build integration

package integration

import "testing"

// TestNopsBinaryStartsAndShutsDown is the wiring task's smoke test: build the
// real nops binary, run it against a real Nomad and a scratch git
// repository (basic auth, so no OIDC provider is needed here), confirm the
// dashboard answers, then send SIGTERM and confirm it exits cleanly. What the
// binary does once it runs (detection, approval, apply, hooks) is covered by
// the end-to-end tests in e2e_test.go and e2e_hooks_test.go.
func TestNopsBinaryStartsAndShutsDown(t *testing.T) {
	p := startNops(t, newScratchRepo(t).url)
	p.stop(t)
}
