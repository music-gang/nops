package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const policyBody = `{"name":"readers","description":"see all","rules":"namespace \"*\" {\n  policy = \"read\"\n}\n","created_at":"2026-10-05T10:00:00Z","modified_at":"2026-10-05T10:00:00Z"}`

const tokenBodyJSON = `{"accessor_id":"01ABC","name":"ci","type":"client","policies":["readers"],"created_at":"2026-10-05T10:00:00Z","expires_at":"2026-11-05T10:00:00Z","creator_name":"bootstrap"}`

func TestACLReadsPrintTextOrJSON(t *testing.T) {
	answers := map[string]answer{
		"GET /api/acl/policies":         {200, `{"acl_policies":[` + policyBody + `]}`},
		"GET /api/acl/policies/readers": {200, policyBody},
		"GET /api/acl/tokens":           {200, `{"tokens":[` + tokenBodyJSON + `]}`},
		"GET /api/acl/tokens/01ABC":     {200, tokenBodyJSON},
		"GET /api/acl/token/self":       {200, tokenBodyJSON},
	}
	for _, tt := range []struct {
		args []string
		want []string
	}{
		{[]string{"acl", "policy", "list"}, []string{"NAME", "readers", "see all"}},
		{[]string{"acl", "policy", "info", "readers"}, []string{"Name:        readers", `policy = "read"`}},
		{[]string{"acl", "token", "list"}, []string{"ACCESSOR ID", "01ABC", "ci", "client", "readers"}},
		{[]string{"acl", "token", "info", "01ABC"}, []string{"Accessor ID:  01ABC", "Created by:   bootstrap"}},
		{[]string{"acl", "token", "self"}, []string{"Type:         client", "ACL policies: readers"}},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			_, addr := newStub(t, answers)
			code, out, errOut := run(t, envFor(addr), "", tt.args...)
			if code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("stdout %q lacks %q", out, w)
				}
			}
			// The flags go after the three words of the command and before its argument.
			asJSON := append(append(append([]string{}, tt.args[:3]...), "-json"), tt.args[3:]...)
			code, out, _ = run(t, envFor(addr), "", asJSON...)
			if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "{") {
				t.Errorf("-json: exit %d, stdout %q, want the JSON of the API", code, out)
			}
		})
	}
}

func TestACLListsSayWhenEmpty(t *testing.T) {
	_, addr := newStub(t, map[string]answer{
		"GET /api/acl/policies": {200, `{"acl_policies":[]}`},
		"GET /api/acl/tokens":   {200, `{"tokens":[]}`},
	})
	if _, out, _ := run(t, envFor(addr), "", "acl", "policy", "list"); out != "No ACL policies.\n" {
		t.Errorf("policies: %q", out)
	}
	if _, out, _ := run(t, envFor(addr), "", "acl", "token", "list"); out != "No tokens.\n" {
		t.Errorf("tokens: %q", out)
	}
}

func TestPolicyApplySendsTheRulesOfTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "readers.hcl")
	rules := "namespace \"*\" {\n  policy = \"read\"\n}\n"
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	s, addr := newStub(t, map[string]answer{"PUT /api/acl/policies/readers": {200, policyBody}})

	code, out, errOut := run(t, envFor(addr), "", "acl", "policy", "apply", "-description", "see all", "readers", file)
	if code != 0 || out != "saved readers\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got := s.seen[0]; got.method != "PUT" || got.auth != "Bearer "+token ||
		got.body != `{"description":"see all","rules":"namespace \"*\" {\n  policy = \"read\"\n}\n"}` {
		t.Errorf("request = %+v", got)
	}

	if code, _, errOut := run(t, envFor(addr), "", "acl", "policy", "apply", "readers", filepath.Join(t.TempDir(), "absent")); code != 1 || !strings.Contains(errOut, "read the rules") {
		t.Errorf("a file that is absent: exit %d, stderr %q", code, errOut)
	}
	if code, _, _ := run(t, envFor(addr), "", "acl", "policy", "apply", "readers"); code != 2 {
		t.Errorf("without the file: exit %d, want 2", code)
	}
}

func TestPolicyApplyShowsWhyRulesAreRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bad.hcl")
	if err := os.WriteFile(file, []byte("namespace {{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, addr := newStub(t, map[string]answer{"PUT /api/acl/policies/bad": {400, `{"error":"line 1: Invalid block definition"}`}})
	code, out, errOut := run(t, envFor(addr), "", "acl", "policy", "apply", "bad", file)
	if code != 1 || out != "" || !strings.Contains(errOut, "line 1: Invalid block definition") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestTokenCreateSendsWhatItIsAskedAndPrintsTheSecret(t *testing.T) {
	s, addr := newStub(t, map[string]answer{"POST /api/acl/tokens": {201, strings.TrimSuffix(tokenBodyJSON, "}") + `,"secret":"nops_the-secret"}`}})
	code, out, errOut := run(t, envFor(addr), "", "acl", "token", "create", "-name", "ci", "-policy", "readers", "-policy", "ops", "-expires", "720h")
	if code != 0 || !strings.Contains(out, "Secret:       nops_the-secret") || !strings.Contains(out, "Accessor ID:  01ABC") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if !strings.Contains(errOut, "does not show it again") || strings.Contains(errOut, "nops_the-secret") {
		t.Errorf("stderr %q: it should warn, and not print the secret", errOut)
	}
	if got := s.seen[0].body; got != `{"expires_in":"720h","name":"ci","policies":["readers","ops"],"type":"client"}` {
		t.Errorf("body = %s", got)
	}

	s.seen = nil
	if code, _, _ := run(t, envFor(addr), "", "acl", "token", "create", "-name", "root", "-type", "management"); code != 0 {
		t.Fatalf("a management token: exit %d", code)
	}
	if got := s.seen[0].body; got != `{"name":"root","type":"management"}` {
		t.Errorf("body = %s, want no policies and no expiry", got)
	}

	code, out, _ = run(t, envFor(addr), "", "acl", "token", "create", "-json", "-name", "ci")
	if code != 0 || !strings.Contains(out, `"secret": "nops_the-secret"`) {
		t.Errorf("-json: exit %d, stdout %q", code, out)
	}
	if code, _, errOut := run(t, envFor(addr), "", "acl", "token", "create"); code != 2 || !strings.Contains(errOut, "-name is required") {
		t.Errorf("without a name: exit %d, stderr %q", code, errOut)
	}
}

func TestACLDeletesCallTheirEndpoint(t *testing.T) {
	s, addr := newStub(t, map[string]answer{
		"DELETE /api/acl/policies/readers": {204, ""},
		"DELETE /api/acl/tokens/01ABC":     {204, ""},
	})
	if code, out, _ := run(t, envFor(addr), "", "acl", "policy", "delete", "readers"); code != 0 || out != "deleted readers\n" {
		t.Errorf("policy delete: exit %d, stdout %q", code, out)
	}
	if code, out, _ := run(t, envFor(addr), "", "acl", "token", "delete", "01ABC"); code != 0 || out != "revoked 01ABC\n" {
		t.Errorf("token delete: exit %d, stdout %q", code, out)
	}
	if len(s.seen) != 2 || s.seen[0].method != "DELETE" {
		t.Errorf("requests = %+v", s.seen)
	}
}

func TestAPermissionErrorIsPrinted(t *testing.T) {
	_, addr := newStub(t, map[string]answer{"GET /api/acl/tokens": {403, `{"error":"this token does not allow that"}`}})
	code, _, errOut := run(t, envFor(addr), "", "acl", "token", "list")
	if code != 1 || !strings.Contains(errOut, "nops answered 403: this token does not allow that") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestACLGroupsNameTheirSubcommands(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"acl"}, "acl takes a subcommand: policy, token"},
		{[]string{"acl", "frobnicate"}, "acl takes a subcommand: policy, token"},
		{[]string{"acl", "policy"}, "acl policy takes a subcommand: list, info, apply, delete"},
		{[]string{"acl", "token"}, "acl token takes a subcommand: list, info, create, delete, self"},
		{[]string{"acl", "token", "frobnicate"}, "acl token takes a subcommand: list, info, create, delete, self"},
	} {
		code, _, errOut := run(t, nil, "", tt.args...)
		if code != 2 || !strings.Contains(errOut, tt.want) {
			t.Errorf("%v: exit %d, stderr %q, want %q", tt.args, code, errOut, tt.want)
		}
	}
}
