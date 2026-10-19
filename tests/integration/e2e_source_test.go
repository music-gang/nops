//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/store"
)

// TestE2EDeployKeepsTheFileFromGit: a job first registered with HCL keeps the
// source of each version nops deploys, the file from git with the variable
// nops_commit, and not the source of its first version.
func TestE2EDeployKeepsTheFileFromGit(t *testing.T) {
	e := newE2E(t)
	jobID := uniqueID(t, e.raw, "src")
	ctx := context.Background()

	seed := e2eJob{id: jobID, policy: "auto", version: "1"}.hcl()
	job, err := e.nomad.ParseHCL(ctx, "default", seed, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.nomad.RegisterCAS(ctx, job, 0, false, &api.JobSubmission{Source: seed, Format: "hcl2"}); err != nil {
		t.Fatal(err)
	}

	want := e2eJob{id: jobID, policy: "auto", version: "2"}.hcl()
	e.repo.commit(t, "job "+jobID+" v2", map[string]string{file(jobID): want})
	d := e.waitNew(jobID, "", store.StateCompleted)

	if d.AppliedVersion == nil {
		t.Fatalf("deployment %s has no applied version", describe(d))
	}
	sub := submissionOf(t, e.raw, jobID, *d.AppliedVersion)
	if sub == nil {
		t.Fatalf("v%d has no source", *d.AppliedVersion)
	}
	if sub.Source != want || sub.Format != "hcl2" {
		t.Errorf("source of v%d = %q (%s), want the file from git", *d.AppliedVersion, sub.Source, sub.Format)
	}
	if !strings.Contains(sub.Variables, `nops_commit = "`+d.CommitSHA+`"`) {
		t.Errorf("variables of v%d = %q, want nops_commit = %q", *d.AppliedVersion, sub.Variables, d.CommitSHA)
	}
}
