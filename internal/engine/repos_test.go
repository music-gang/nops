package engine

import (
	"context"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

func TestJobDeclaredInTwoRepositoriesIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web-a", managed("web", "auto", nil))
	h.nomad.setFile("web-b", managed("web", "auto", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "web.nomad.hcl", Content: "web-a"})
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web-b"}}})

	h.detect()

	h.noActive("web")
	if obs := h.engine.Observations(); len(obs) != 0 {
		t.Errorf("observations = %+v, want none for a job in two repositories", obs)
	}
}

func TestDeploymentRecordsItsRepositoryAndCommit(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("api", managed("api", "auto", nil))
	h.nomad.setFile("web", managed("web", "auto", nil))
	h.nomad.setDrift("api", &api.JobDiff{Type: "Edited", ID: "api"})
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.setCommit("c1", "bump api", "alice", gitwatch.File{Path: "api.nomad.hcl", Content: "api"})
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Subject: "bump web", Author: "bob",
		Files: []gitwatch.File{{Path: "nomad/web.nomad.hcl", Content: "web"}}})

	h.detect()

	for job, want := range map[string]store.Deployment{
		"api": {Repository: "default", CommitSHA: "c1", CommitSubject: "bump api", CommitAuthor: "alice"},
		"web": {Repository: "apps", CommitSHA: "a1", CommitSubject: "bump web", CommitAuthor: "bob"},
	} {
		d := h.active(job)
		if d.Repository != want.Repository || d.CommitSHA != want.CommitSHA || d.CommitSubject != want.CommitSubject || d.CommitAuthor != want.CommitAuthor {
			t.Errorf("deployment of %s = %s %s %q %q, want %s %s %q %q", job,
				d.Repository, d.CommitSHA, d.CommitSubject, d.CommitAuthor,
				want.Repository, want.CommitSHA, want.CommitSubject, want.CommitAuthor)
		}
	}
	for _, o := range h.engine.Observations() {
		if o.JobID == "web" && (o.Repository != "apps" || o.FilePath != "nomad/web.nomad.hcl") {
			t.Errorf("observation of web = %s:%s, want apps:nomad/web.nomad.hcl", o.Repository, o.FilePath)
		}
	}
}

func TestHookFromAnotherRepository(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web", managed("web", "auto", map[string]string{"nops_pre_hook": "web-migrate"}))
	h.nomad.setFile("migrate", hookJob("web-migrate"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "hooks/web-migrate.nomad.hcl", Content: "migrate"})
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web"}}})

	h.detect()

	d := h.active("web")
	if d.State != store.StateDetected {
		t.Fatalf("state = %s (%s), want detected: the hook is in the default repository", d.State, d.Error)
	}
	frozen, err := h.store.DeploymentHooks(context.Background(), d.ID)
	if err != nil || len(frozen) != 1 || frozen[0].HookID != "web-migrate" {
		t.Errorf("frozen hooks = %+v, %v; want web-migrate", frozen, err)
	}
}

func TestCommitInAnotherRepositoryKeepsTheDeployment(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("api", managed("api", "auto", nil))
	h.nomad.setFile("web", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "api.nomad.hcl", Content: "api"})
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web"}}})
	h.detect()
	before := h.active("web")

	h.nomad.setFile("api-v2", managed("api", "auto", nil))
	h.snap.set("c2", gitwatch.File{Path: "api.nomad.hcl", Content: "api-v2"})
	h.detect()

	if after := h.active("web"); after.ID != before.ID || after.CommitSHA != "a1" {
		t.Errorf("deployment of web = %s at %s, want %s at a1 still waiting", after.ID, after.CommitSHA, before.ID)
	}
}
