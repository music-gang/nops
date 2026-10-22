package cli

import (
	"net/url"
	"strings"
	"testing"
)

func TestListFlagsAreTheAPIParameters(t *testing.T) {
	s, addr := newStub(t, map[string]answer{
		"GET /api/deployments": {200, `{"deployments":[]}`},
		"GET /api/jobs":        {200, `{"jobs":[]}`},
	})
	env := envFor(addr)
	env["NOPS_NAMESPACE"] = "prod" // a list doesn't read it

	if code, _, errOut := run(t, env, "", "deployments", "-per-page", "25", "-search", "bump web", "-state", "failed", "-job", "web",
		"-repository", "infra", "-policy", "auto", "-decided-by", "alice", "-since", "2026-10-01", "-until", "2026-10-02", "-page-token", "tok"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got, _ := url.ParseQuery(s.seen[0].query)
	want := url.Values{
		"per_page": {"25"}, "q": {"bump web"}, "state": {"failed"}, "job": {"web"}, "repository": {"infra"}, "policy": {"auto"},
		"decided_by": {"alice"}, "since": {"2026-10-01"}, "until": {"2026-10-02"}, "next_token": {"tok"},
	}
	if got.Encode() != want.Encode() {
		t.Errorf("query = %s, want %s", got.Encode(), want.Encode())
	}

	if code, _, errOut := run(t, env, "", "jobs", "-namespace", "default", "-warnings"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if q := s.seen[1].query; q != "namespace=default&warnings=true" {
		t.Errorf("jobs query = %q", q)
	}
	if code, _, _ := run(t, env, "", "jobs"); code != 0 || s.seen[2].query != "" {
		t.Errorf("no flag: query %q, want none: NOPS_NAMESPACE is not a filter", s.seen[2].query)
	}
	if code, _, _ := run(t, env, "", "jobs", "-job", "web"); code != 2 {
		t.Errorf("-job on jobs: exit %d, want a usage error", code)
	}
}

// The command of the next page keeps the flags, on the error output; the last
// page prints none, whatever its number of rows.
func TestListPrintsTheCommandOfTheNextPage(t *testing.T) {
	s, addr := newStub(t, map[string]answer{"GET /api/deployments": {200, `{"deployments":[],"next_token":"abc-_1"}`}})
	code, out, errOut := run(t, envFor(addr), "", "deployments", "-state", "failed", "-search", "it's here", "-page-token", "old")
	if code != 0 || strings.Contains(out, "Next page") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if want := `Next page: nops deployments -search 'it'\''s here' -state failed -page-token abc-_1`; !strings.Contains(errOut, want) {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}

	s.answers["GET /api/deployments"] = answer{200, `{"deployments":[]}`}
	if _, _, errOut := run(t, envFor(addr), "", "deployments", "-per-page", "25"); strings.Contains(errOut, "Next page") {
		t.Errorf("the last page printed a next one: %q", errOut)
	}
	s.answers["GET /api/deployments"] = answer{200, `{"deployments":[],"next_token":"abc"}`}
	if _, out, errOut := run(t, envFor(addr), "", "deployments", "-json"); strings.Contains(out+errOut, "Next page") || !strings.Contains(out, `"next_token": "abc"`) {
		t.Errorf("-json: stdout %q, stderr %q, want the API's answer only", out, errOut)
	}
}
