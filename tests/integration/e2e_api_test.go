//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

var apiSecret = regexp.MustCompile(`nops_[A-Za-z0-9_-]{20,}`)

// apiCall is a request to the JSON API with a bearer token and no session.
func apiCall(t *testing.T, base, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, string(b)
}

// e2eToken is the name of the token apiToken makes.
const e2eToken = "e2e"

// apiToken makes a token on the Administration page, as its logged-in user: with
// the ACL off, a login can do everything, and so can the token.
func (e *e2eEnv) apiToken(t *testing.T) string {
	t.Helper()
	status, page := e.dash.post(t, "/admin/tokens", url.Values{"name": {e2eToken}, "expires": {"7"}})
	if status != http.StatusOK {
		t.Fatalf("create a token: status %d, want 200", status)
	}
	token := apiSecret.FindString(page)
	if token == "" {
		t.Fatalf("the Administration page shows no token:\n%s", page)
	}
	return token
}

// TestE2EAPITokenApprovesAsItself: a token made on the dashboard moves a
// deployment past pending_approval without a session, under the same rule: the
// spec hash the caller saw. Nops records the token as the one who decided.
func TestE2EAPITokenApprovesAsItself(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "api")
	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): e2eJob{id: jobID, policy: "approval", version: "1"}.hcl()})
	d := e.waitNew(jobID, "", store.StatePendingApproval)

	token := e.apiToken(t)

	if status, _ := apiCall(t, e.dash.base, "GET", "/api/deployments/"+d.ID, "nops_not-a-token", ""); status != http.StatusUnauthorized {
		t.Errorf("a token that does not exist: status %d, want 401", status)
	}
	if status, body := apiCall(t, e.dash.base, "GET", "/api/deployments/"+d.ID, token, ""); status != http.StatusOK || !strings.Contains(body, d.SpecHash) {
		t.Fatalf("GET the deployment: status %d, body %s, want 200 with its spec hash", status, body)
	}
	if status, _ := apiCall(t, e.dash.base, "POST", "/api/deployments/"+d.ID+"/approve", token, `{"spec_hash":"not-the-spec-i-saw"}`); status != http.StatusConflict {
		t.Fatalf("approve with another spec hash: status %d, want 409", status)
	}
	if got := e.deployment(d.ID); got.State != store.StatePendingApproval {
		t.Fatalf("deployment = %s after a refused approval, want it still pending", got.State)
	}

	if status, body := apiCall(t, e.dash.base, "POST", "/api/deployments/"+d.ID+"/approve", token, `{"spec_hash":"`+d.SpecHash+`"}`); status != http.StatusNoContent {
		t.Fatalf("approve: status %d, body %s, want 204", status, body)
	}
	done := e.waitState(d.ID, store.StateCompleted)
	if done.DecidedBy != e2eToken {
		t.Errorf("decided_by = %q, want the token %q", done.DecidedBy, e2eToken)
	}
	if got := e.liveVersion(jobID); got != "1" {
		t.Errorf("live version = %q after the approval, want 1", got)
	}
}

// TestE2ECLIApprovesWhatItShowed: the built binary, with a token and the URL in
// its environment, shows a deployment and approves it. A deployment it has not
// shown is not approved without the answer, and Nops records the token.
func TestE2ECLIApprovesWhatItShowed(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "cli")
	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): e2eJob{id: jobID, policy: "approval", version: "1"}.hcl()})
	d := e.waitNew(jobID, "", store.StatePendingApproval)

	token := e.apiToken(t)
	nops := func(stdin string, args ...string) (string, string, error) {
		cmd := exec.Command(buildNopsBinary(t), args...)
		cmd.Env = append(os.Environ(), "NOPS_ADDR="+e.dash.base, "NOPS_TOKEN="+token)
		cmd.Stdin = strings.NewReader(stdin)
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.String(), errOut.String(), err
	}

	out, _, err := nops("", "deployment", d.ID)
	if err != nil || !strings.Contains(out, d.SpecHash) {
		t.Fatalf("nops deployment: %v, stdout %q, want it to show the spec hash", err, out)
	}
	if out, _, err := nops("", "jobs"); err != nil || !strings.Contains(out, jobID) {
		t.Fatalf("nops jobs: %v, stdout %q, want the job", err, out)
	}
	// The search of the real store finds it, and the filters keep it out.
	if out, errOut, err := nops("", "deployments", "-search", jobID, "-state", "pending_approval", "-per-page", "25"); err != nil || !strings.Contains(out, d.ID) {
		t.Fatalf("nops deployments -search: %v, stdout %q, stderr %q, want the deployment", err, out, errOut)
	}
	if out, _, err := nops("", "deployments", "-search", jobID, "-state", "completed"); err != nil || strings.Contains(out, d.ID) {
		t.Fatalf("nops deployments -state completed: %v, stdout %q, want no pending deployment", err, out)
	}

	if _, _, err := nops("n\n", "approve", d.ID); err == nil {
		t.Fatal("approve answered no: want a non-zero exit")
	}
	if got := e.deployment(d.ID); got.State != store.StatePendingApproval {
		t.Fatalf("deployment = %s after a no, want it still pending", got.State)
	}

	if _, errOut, err := nops("y\n", "approve", d.ID); err != nil {
		t.Fatalf("nops approve: %v\n%s", err, errOut)
	}
	done := e.waitState(d.ID, store.StateCompleted)
	if done.DecidedBy != e2eToken {
		t.Errorf("decided_by = %q, want the token %q", done.DecidedBy, e2eToken)
	}
}

// TestE2ECLILoginAndLogout: the built binary logs in with the username and the
// password of the dashboard, uses the session it saved with no token anywhere,
// and logs out: the session is revoked in Nops, not only forgotten.
func TestE2ECLILoginAndLogout(t *testing.T) {
	e := newE2E(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	nops := func(stdin string, args ...string) (string, string, error) {
		cmd := exec.Command(buildNopsBinary(t), args...)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "NOPS_") { // a token of the shell must not hide the session
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "NOPS_ADDR="+e.dash.base)
		cmd.Stdin = strings.NewReader(stdin)
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		return out.String(), errOut.String(), err
	}

	if _, _, err := nops("", "jobs"); err == nil {
		t.Fatal("nops jobs with no token and no session: want a refusal")
	}
	if out, errOut, err := nops(e2eUser+"\n"+e2ePassword+"\n", "login"); err != nil || !strings.Contains(out, "Logged in to "+e.dash.base+" as "+e2eUser) {
		t.Fatalf("nops login: %v, stdout %q\n%s", err, out, errOut)
	}
	if out, errOut, err := nops("", "acl", "token", "self"); err != nil || !strings.Contains(out, "basic:"+e2eUser) {
		t.Fatalf("nops acl token self: %v, stdout %q\n%s", err, out, errOut)
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "nops", "session"))
	if err != nil {
		t.Fatalf("the session file: %v", err)
	}
	var saved struct{ Secret string }
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Secret == "" {
		t.Fatalf("the session file = %q: %v", raw, err)
	}
	if status, _ := apiCall(t, e.dash.base, "GET", "/api/jobs", saved.Secret, ""); status != http.StatusOK {
		t.Fatalf("the saved session on the API: status %d, want 200", status)
	}

	if out, errOut, err := nops("", "logout"); err != nil || !strings.Contains(out, "Logged out of") {
		t.Fatalf("nops logout: %v, stdout %q\n%s", err, out, errOut)
	}
	if status, _ := apiCall(t, e.dash.base, "GET", "/api/jobs", saved.Secret, ""); status != http.StatusUnauthorized {
		t.Errorf("the session after the logout: status %d, want 401: it was forgotten, not revoked", status)
	}
	if _, errOut, err := nops("", "jobs"); err == nil || !strings.Contains(errOut, "nops login") {
		t.Errorf("nops jobs after the logout: %v, stderr %q, want a hint to log in", err, errOut)
	}
}
