//go:build integration

package integration

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/secret"
	"github.com/music-gang/nops/internal/store"
)

// TestE2EACLBootstrapTokenAdministersAndClientTokenIsLimited: Nops started with
// -acl and a bootstrap token. The bootstrap token, from the built binary,
// makes an ACL policy and a client token; the client token reads and cannot
// approve; the bootstrap token approves, and the event records its accessor ID.
func TestE2EACLBootstrapTokenAdministersAndClientTokenIsLimited(t *testing.T) {
	bootstrap := secret.New()
	bootFile := filepath.Join(t.TempDir(), "bootstrap-token")
	if err := os.WriteFile(bootFile, []byte(bootstrap+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newE2E(t, "NOPS_ACL=true", "NOPS_ACL_BOOTSTRAP_TOKEN_FILE="+bootFile)

	jobID := uniqueID(t, e.raw, "acl")
	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): e2eJob{id: jobID, policy: "approval", version: "1"}.hcl()})
	d := e.waitNew(jobID, "", store.StatePendingApproval)

	nops := func(token string, args ...string) (string, string, error) {
		cmd := exec.Command(buildNopsBinary(t), args...)
		cmd.Env = append(os.Environ(), "NOPS_ADDR="+e.dash.base, "NOPS_TOKEN="+token)
		cmd.Stdin = strings.NewReader("y\n")
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.String(), errOut.String(), err
	}

	rules := filepath.Join(t.TempDir(), "readers.hcl")
	if err := os.WriteFile(rules, []byte("namespace \"default\" {\n  policy = \"read\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, err := nops(bootstrap, "acl", "policy", "apply", "readers", rules); err != nil {
		t.Fatalf("acl policy apply: %v\n%s", err, errOut)
	}
	bad := filepath.Join(t.TempDir(), "bad.hcl")
	if err := os.WriteFile(bad, []byte("namespace {{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, err := nops(bootstrap, "acl", "policy", "apply", "bad", bad); err == nil || !strings.Contains(errOut, "line 1") {
		t.Errorf("rules that do not parse: err %v, stderr %q, want a refusal that names the line", err, errOut)
	}

	out, errOut, err := nops(bootstrap, "acl", "token", "create", "-name", "reader", "-policy", "readers")
	if err != nil {
		t.Fatalf("acl token create: %v\n%s", err, errOut)
	}
	client := apiSecret.FindString(out)
	if client == "" {
		t.Fatalf("acl token create printed no secret:\n%s", out)
	}

	if out, _, err := nops(client, "jobs"); err != nil || !strings.Contains(out, jobID) {
		t.Errorf("a client token reading: %v, stdout %q, want the job", err, out)
	}
	if _, errOut, err := nops(client, "approve", "-yes", d.ID); err == nil || !strings.Contains(errOut, "403") {
		t.Errorf("a client token approving: err %v, stderr %q, want a 403", err, errOut)
	}
	if status, _ := apiCall(t, e.dash.base, "GET", "/api/acl/tokens", client, ""); status != http.StatusForbidden {
		t.Errorf("a client token listing tokens: status %d, want 403", status)
	}
	if got := e.deployment(d.ID); got.State != store.StatePendingApproval {
		t.Fatalf("deployment = %s after a refused approval, want it still pending", got.State)
	}

	if _, errOut, err := nops(bootstrap, "approve", "-yes", d.ID); err != nil {
		t.Fatalf("approve with the bootstrap token: %v\n%s", err, errOut)
	}
	done := e.waitState(d.ID, store.StateCompleted)
	if done.DecidedBy != "bootstrap" {
		t.Errorf("decided_by = %q, want bootstrap", done.DecidedBy)
	}
	events, err := e.st.Events(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	var decided bool
	for _, ev := range events {
		if ev.From == store.StatePendingApproval && ev.To != store.StatePendingApproval {
			decided = true
			if ev.Actor != "bootstrap" || ev.AccessorID != "bootstrap" {
				t.Errorf("the decision = %+v, want the actor and the accessor ID bootstrap", ev)
			}
		}
	}
	if !decided {
		t.Errorf("no event for the decision in %+v", events)
	}
}

// TestE2EACLRefusesToStartWithoutABootstrapToken: -acl is not a switch to flip
// without the token that administers it.
func TestE2EACLRefusesToStartWithoutABootstrapToken(t *testing.T) {
	cmd := exec.Command(buildNopsBinary(t), "serve")
	cmd.Env = append(os.Environ(), "NOPS_ACL=true", "NOPS_GIT_URL=https://git.example.com/x.git", "NOPS_AUTH_MODE=basic", "NOPS_USERS_FILE="+writeUsersFile(t, e2eUser, e2ePassword))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "-acl needs a bootstrap token") {
		t.Errorf("err %v, output %q, want a refusal that names the bootstrap token", err, out)
	}
}
