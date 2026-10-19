package engine

import (
	"context"
	"reflect"
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

// allow limits the jobs a repository may declare, as Options.RepositoryJobs does.
func (h *harness) allow(repo string, patterns ...string) {
	h.t.Helper()
	if h.engine.repoJobs == nil {
		h.engine.repoJobs = map[string][]string{}
	}
	h.engine.repoJobs[repo] = patterns
}

func TestRefusedFileCannotTakeAnotherRepositorysJob(t *testing.T) {
	h := newHarness(t)
	h.allow("apps", "default/web-*")
	h.nomad.setFile("crm", managed("crm", "auto", nil))
	h.nomad.setFile("crm-evil", managed("crm", "auto", map[string]string{"x": "evil"}))
	h.nomad.setFile("web", managed("web-api", "auto", nil))
	h.nomad.setDrift("crm", &api.JobDiff{Type: "Edited", ID: "crm"})
	h.nomad.setDrift("web-api", &api.JobDiff{Type: "Edited", ID: "web-api"})
	h.snap.set("c1", gitwatch.File{Path: "crm.nomad.hcl", Content: "crm"})
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{
		{Path: "crm.nomad.hcl", Content: "crm-evil"}, {Path: "web.nomad.hcl", Content: "web"},
	}})

	h.detect()

	if d := h.active("crm"); d.Repository != "default" {
		t.Errorf("crm comes from %s, want default: the file of apps is refused, not a duplicate", d.Repository)
	}
	if d := h.active("web-api"); d.Repository != "apps" {
		t.Errorf("web-api comes from %s, want apps: it matches", d.Repository)
	}
	st := h.engine.Status()
	want := []RefusedFile{{Repository: "apps", Path: "crm.nomad.hcl", Namespace: "default", JobID: "crm"}}
	if !reflect.DeepEqual(st.Refused, want) {
		t.Errorf("refused = %+v, want %+v", st.Refused, want)
	}
	if st.Unparsed != 0 || st.OrphanCheckSkipped {
		t.Errorf("unparsed = %d, orphan check skipped = %v, want a refused file to be neither", st.Unparsed, st.OrphanCheckSkipped)
	}
}

func TestRefusedHookDoesNotBlockTheHookOfAnotherRepository(t *testing.T) {
	h := newHarness(t)
	h.allow("apps", "default/web")
	h.nomad.setFile("web", managed("web", "auto", map[string]string{"nops_pre_hook": "wait-for-image"}))
	h.nomad.setFile("hook", hookJob("wait-for-image"))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.set("c1", gitwatch.File{Path: "hooks/wait-for-image.nomad.hcl", Content: "hook"})
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{
		{Path: "web.nomad.hcl", Content: "web"}, {Path: "wait-for-image.nomad.hcl", Content: "hook"},
	}})

	h.detect()

	if d := h.active("web"); d.State != store.StateDetected {
		t.Errorf("state = %s (%s), want detected: the hook of apps is refused, so it is no duplicate", d.State, d.Error)
	}
	if got := h.engine.Status().Refused; len(got) != 1 || got[0].JobID != "wait-for-image" {
		t.Errorf("refused = %+v, want the hook of apps", got)
	}
}

func TestJobThatBecomesRefusedIsARemovedJob(t *testing.T) {
	h := newHarness(t)
	h.nomad.setFile("web", managed("web", "approval", nil))
	h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web"}}})
	h.detect()
	waiting := h.active("web")

	h.allow("apps", "default/api")
	h.detect()

	got, _ := h.store.GetDeployment(context.Background(), waiting.ID)
	if got.State != store.StateSuperseded {
		t.Fatalf("deployment = %s, want superseded: its job is no longer allowed", got.State)
	}
	if evs, _ := h.store.Events(context.Background(), waiting.ID); evs[len(evs)-1].Message != "job removed from repo" {
		t.Errorf("last event message = %q, want job removed from repo", evs[len(evs)-1].Message)
	}
	h.noActive("web")
	if len(h.engine.Observations()) != 0 {
		t.Errorf("observations = %+v, want none", h.engine.Observations())
	}
}

func TestRunningJobThatBecomesRefusedIsAnOrphan(t *testing.T) {
	h := newHarness(t)
	h.allow("apps", "default/api")
	h.deployed("web", store.PolicyAuto)
	h.runningInNomad("web", "running")
	h.nomad.setFile("web", managed("web", "auto", nil))
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web"}}})

	h.detect()

	if got := orphanIDs(h.engine.Orphans()); got != "web" {
		t.Errorf("orphans = %q, want web", got)
	}
}

func TestJobsPatternWithoutNamespaceIsTheDefaultNamespace(t *testing.T) {
	h := newHarnessIn(t, "default", "labs")
	h.allow("apps", "default/web")
	web := managed("web", "auto", nil)
	web.Namespace = ptr("labs")
	h.nomad.setFile("web", web)
	h.snap.setRepo(gitwatch.Snapshot{Repository: "apps", Commit: "a1", Files: []gitwatch.File{{Path: "web.nomad.hcl", Content: "web"}}})

	h.detect()

	want := []RefusedFile{{Repository: "apps", Path: "web.nomad.hcl", Namespace: "labs", JobID: "web"}}
	if got := h.engine.Status().Refused; !reflect.DeepEqual(got, want) {
		t.Errorf("refused = %+v, want %+v: web in labs is not default/web", got, want)
	}
}

func TestJobAllowed(t *testing.T) {
	tests := []struct {
		patterns []string
		ns, id   string
		want     bool
	}{
		{[]string{"labs/billing-api"}, "labs", "billing-api", true},
		{[]string{"labs/billing-api-*"}, "labs", "billing-api", false},
		{[]string{"labs/billing-api-*"}, "labs", "billing-api-worker", true},
		{[]string{"labs/*"}, "labs", "a/b", false},
		{[]string{"*/api"}, "labs", "api", true},
		{[]string{"lab*/api"}, "labs", "api", true},
		{[]string{"labs/a", "default/b"}, "default", "b", true},
		{[]string{"labs/a"}, "default", "a", false},
	}
	for _, tt := range tests {
		if got := jobAllowed(tt.patterns, tt.ns, tt.id); got != tt.want {
			t.Errorf("jobAllowed(%q, %s, %s) = %v, want %v", tt.patterns, tt.ns, tt.id, got, tt.want)
		}
	}
}
