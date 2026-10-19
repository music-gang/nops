//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// TestE2EJobFromASecondRepository runs nops on two repositories: the default
// one of NOPS_GIT_URL, and "apps" from a repository block of NOPS_CONFIG. A job
// in apps runs a hook kept in the default repository, and its deployment
// records the repository it came from.
func TestE2EJobFromASecondRepository(t *testing.T) {
	apps := newScratchRepo(t)
	apps.commit(t, "the jobs directory", map[string]string{"nomad/README.md": "jobs\n"})
	config := filepath.Join(t.TempDir(), "nops.hcl")
	if err := os.WriteFile(config, []byte(`
repository "apps" {
  url  = "`+apps.url+`"
  path = "nomad"
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newE2E(t, "NOPS_CONFIG="+config)
	dir := t.TempDir()
	jobID := uniqueID(t, e.raw, "repos")
	hookID := uniqueID(t, e.raw, "reposhook")

	// The hook first, and seen, so the job's deployment finds it.
	e.repo.commit(t, "hook "+hookID, map[string]string{file(hookID): traceHook(hookID, dir)})
	deadline := time.Now().Add(e2eWait)
	for {
		if _, page := e.dash.get(t, "/repositories"); strings.Contains(page, "hook "+hookID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the repositories page never showed the hook's commit")
		}
		time.Sleep(100 * time.Millisecond)
	}

	j := e2eJob{id: jobID, policy: "auto", version: "1", preHook: hookID}
	apps.commit(t, "job "+jobID, map[string]string{"nomad/" + file(jobID): j.hcl()})
	d := e.waitNew(jobID, "", store.StateCompleted)

	if d.Repository != "apps" || d.CommitSubject != "job "+jobID {
		t.Errorf("deployment = %s at %q, want apps at the job's commit", d.Repository, d.CommitSubject)
	}
	if _, err := os.Stat(filepath.Join(dir, d.ID+".dump")); err != nil {
		t.Errorf("the hook of the default repository did not run for the job of apps: %v", err)
	}
	if _, page := e.dash.get(t, "/deployments/"+d.ID); !strings.Contains(page, "<dt>Repository</dt><dd>apps</dd>") {
		t.Errorf("the deployment page does not name the repository apps")
	}
}

// TestE2ERepositoryMayDeclareOnlyItsJobs gives the repository apps the jobs
// attribute: the job it may declare is deployed, the other is refused, never
// deployed, and shown on the repositories page and in the metrics.
func TestE2ERepositoryMayDeclareOnlyItsJobs(t *testing.T) {
	apps := newScratchRepo(t)
	apps.commit(t, "the jobs directory", map[string]string{"nomad/README.md": "jobs\n"})
	config := filepath.Join(t.TempDir(), "nops.hcl")
	_, raw := newClient(t)
	allowed, refused := uniqueID(t, raw, "mine"), uniqueID(t, raw, "theirs")
	if err := os.WriteFile(config, []byte(`
repository "apps" {
  url  = "`+apps.url+`"
  path = "nomad"
  jobs = ["`+allowed+`"]
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newE2E(t, "NOPS_CONFIG="+config)

	apps.commit(t, "two jobs", map[string]string{
		"nomad/" + file(allowed): e2eJob{id: allowed, policy: "auto", version: "1"}.hcl(),
		"nomad/" + file(refused): e2eJob{id: refused, policy: "auto", version: "1"}.hcl(),
	})
	d := e.waitNew(allowed, "", store.StateCompleted)
	if d.Repository != "apps" {
		t.Errorf("deployment of %s comes from %s, want apps", allowed, d.Repository)
	}

	want := []string{
		`nops_refused_files{repo="apps"} 1`,
		`nops_refused_files{repo="default"} 0`,
	}
	var body string
	deadline := time.Now().Add(e2eWait)
	for {
		_, body = scrape(t, e.proc.baseURL, "")
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(body, "\n"+w)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/metrics misses %q after %s:\n%s", want, e2eWait, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, page := e.dash.get(t, "/repositories"); !strings.Contains(page, "nomad/"+file(refused)) {
		t.Errorf("the repositories page does not list the refused file nomad/%s", file(refused))
	}
	if n := e.deploymentsOf(refused); n != 0 {
		t.Errorf("%d deployments of %s, want none: its file is refused", n, refused)
	}
	if _, _, err := e.raw.Jobs().Info(refused, nil); err == nil {
		t.Errorf("%s is in Nomad, want it never registered", refused)
	}
}
