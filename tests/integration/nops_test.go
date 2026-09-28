//go:build integration

package integration

import (
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// TestNopsBinaryStartsAndShutsDown is the wiring task's smoke test: build the
// real nops binary, run it against a real Nomad and a scratch git
// repository (basic auth, so no OIDC provider is needed here), confirm the
// dashboard answers with the linked version, then send SIGTERM and confirm
// it exits cleanly. What the binary does once it runs (detection, approval,
// apply, hooks) is covered by the end-to-end tests in e2e_test.go and
// e2e_hooks_test.go.
func TestNopsBinaryStartsAndShutsDown(t *testing.T) {
	p := startNops(t, newScratchRepo(t).url)

	resp, err := http.Get(p.baseURL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "ok "+testVersion+"\n"; got != want {
		t.Errorf("healthz body = %q, want %q", got, want)
	}

	p.stop(t)
}

// TestNopsVersion checks that -version prints the version linked at build
// time and exits 0 with no other option set, as the release smoke test
// (docker run <image> -version) relies on.
func TestNopsVersion(t *testing.T) {
	cmd := exec.Command(buildNopsBinary(t), "-version")
	cmd.Env = []string{} // no NOPS_* at all: -version needs none
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("nops -version: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != testVersion {
		t.Errorf("nops -version = %q, want %q", got, testVersion)
	}
}
