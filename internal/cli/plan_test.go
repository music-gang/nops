package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const planSHA = "0123456789abcdef0123456789abcdef01234567"

// checkout writes files under a new directory.
func checkout(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The plan sends the job files and var-files of the directory, and nothing
// else: a file of another kind may hold anything.
func TestPlanSendsTheJobFiles(t *testing.T) {
	s, addr := newStub(t, map[string]answer{"POST /api/plan": {200, `{"jobs":[],"errors":[],"removed":[]}`}})
	dir := checkout(t, map[string]string{
		"nomad/web.nomad.hcl": "job web",
		"nomad/web.vars.hcl":  "tag = 1",
		"api.nomad":           "job api",
		".env":                "SECRET=x",
		"README.md":           "hi",
		".git/x.nomad":        "not a checkout file",
	})

	code, out, errOut := run(t, envFor(addr), "", "plan", "-repository", "infra", "-commit", planSHA, dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != "No jobs.\n" {
		t.Errorf("stdout = %q", out)
	}
	var sent struct {
		Repository, Commit string
		Files              []planFile
	}
	if err := json.Unmarshal([]byte(s.seen[0].body), &sent); err != nil {
		t.Fatal(err)
	}
	want := []planFile{{"api.nomad", "job api"}, {"nomad/web.nomad.hcl", "job web"}, {"nomad/web.vars.hcl", "tag = 1"}}
	if sent.Repository != "infra" || sent.Commit != planSHA || !reflect.DeepEqual(sent.Files, want) {
		t.Errorf("sent %+v, want infra at %s with %+v", sent, planSHA, want)
	}
}

func TestPlanNeedsTheRepositoryAndTheCommit(t *testing.T) {
	_, addr := newStub(t, nil)
	dir := checkout(t, nil)
	for _, args := range [][]string{
		{"plan", "-commit", planSHA, dir},
		{"plan", "-repository", "infra", dir},
	} {
		if code, _, errOut := run(t, envFor(addr), "", args...); code != exitUsage || !strings.Contains(errOut, "required") {
			t.Errorf("%v: exit %d %q, want a usage error", args, code, errOut)
		}
	}
}

// The text says what happens to each job; an error in the files fails the
// command, which a pipeline reads, and -json prints the answer all the same.
func TestPlanPrintsAndFailsOnAnError(t *testing.T) {
	body := `{"jobs":[` +
		`{"namespace":"default","job":"web","policy":"approval","sync":"pending","drift":true,"repository":"default","file":"web.nomad.hcl","pre_hooks":["migrate"],"diff":` + diffJSON(t) + `},` +
		`{"namespace":"default","job":"api","policy":"none","sync":"invalid","drift":false,"repository":"apps","file":"api.nomad.hcl","error":"invalid meta: nops_policy: unknown"}` +
		`],"errors":[{"file":"dup.nomad.hcl","namespace":"default","job":"dup","error":"two files parse to this job"}],` +
		`"removed":[{"namespace":"default","job":"old","superseded":"d1","orphan":true}]}`
	_, addr := newStub(t, map[string]answer{"POST /api/plan": {200, body}})
	dir := checkout(t, nil)

	code, out, errOut := run(t, envFor(addr), "", "plan", "-repository", "default", "-commit", planSHA, dir)
	if code != exitError || !strings.Contains(errOut, "2 errors would keep a job from deploying") {
		t.Errorf("exit %d %q, want 1 for the two errors", code, errOut)
	}
	for _, want := range []string{
		"Job:         default/web", "Sync:        pending", "Pre-hooks:   migrate", "image: web:1 => web:2",
		"Error:       invalid meta: nops_policy: unknown", "Repository:  apps",
		"default/old  supersedes d1, becomes an orphan",
		"dup.nomad.hcl  default/dup  two files parse to this job",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}

	code, out, _ = run(t, envFor(addr), "", "plan", "-json", "-repository", "default", "-commit", planSHA, dir)
	if code != exitError || !json.Valid([]byte(out)) {
		t.Errorf("-json: exit %d, stdout %q: want the answer and 1", code, out)
	}
}
