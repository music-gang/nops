package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/redact"
)

const token = "nops_secret-token"

// call is one request the stub saw.
type seen struct{ method, path, auth, body string }

// stub answers the API of docs/api.md with canned bodies, by "METHOD path".
type stub struct {
	t       *testing.T
	answers map[string]answer
	seen    []seen
}

type answer struct {
	status int
	body   string
}

func newStub(t *testing.T, answers map[string]answer) (*stub, string) {
	t.Helper()
	s := &stub{t: t, answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.seen = append(s.seen, seen{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), string(b)})
		a, ok := s.answers[r.Method+" "+r.URL.EscapedPath()]
		if !ok {
			a = answer{404, `{"error":"no such endpoint"}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

// run runs the client with the given environment and standard input.
func run(t *testing.T, env map[string]string, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, func(k string) string { return env[k] }, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func envFor(addr string) map[string]string {
	return map[string]string{"NOPS_ADDR": addr, "NOPS_TOKEN": token}
}

// diffJSON is the redacted plan diff of a job whose image, count and a secret
// change, as the API sends it.
func diffJSON(t *testing.T) string {
	t.Helper()
	d := api.JobDiff{
		Type: "Edited", ID: "web",
		TaskGroups: []*api.TaskGroupDiff{{
			Type: "Edited", Name: "web",
			Fields: []*api.FieldDiff{{Type: "Edited", Name: "Count", Old: "1", New: "2"}},
			Tasks: []*api.TaskDiff{
				{Type: "None", Name: "sidecar"},
				{Type: "Edited", Name: "server",
					Objects: []*api.ObjectDiff{{Type: "Edited", Name: "Config",
						Fields: []*api.FieldDiff{
							{Type: "Edited", Name: "image", Old: "web:1", New: "web:2"},
							{Type: "None", Name: "port", Old: "80", New: "80"},
						}}},
					Fields: []*api.FieldDiff{
						{Type: "Added", Name: "Env[DB_PASSWORD]", New: redact.Marker},
						{Type: "Deleted", Name: "Env[OLD]", Old: "x"},
					}},
			},
		}},
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const deploymentBody = `{"id":"d1","namespace":"default","job":"web","state":"pending_approval","policy":"approval",` +
	`"spec_hash":"hash-1","commit_sha":"0123456789abcdef","commit_subject":"bump web","created_at":"2026-10-05T10:00:00Z",` +
	`"events":[{"time":"2026-10-05T10:00:01Z","from":"detected","to":"pending_approval","actor":"nops"}],"hook_runs":[]`

func TestApproveShowsTheDiffAndSendsItsSpecHash(t *testing.T) {
	s, addr := newStub(t, nil)
	s.answers = map[string]answer{
		"GET /api/deployments/d1":          {200, deploymentBody + `,"diff":` + diffJSON(t) + `}`},
		"POST /api/deployments/d1/approve": {204, ""},
	}

	code, out, errOut := run(t, envFor(addr), "y\n", "approve", "d1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != "approved d1\n" {
		t.Errorf("stdout = %q, want only the answer", out)
	}
	for _, want := range []string{
		"Approve deployment d1 of default/web? [y/N]",
		"Spec hash:   hash-1",
		"~ image: web:1 => web:2",
		"+ Env[DB_PASSWORD]: " + redact.Marker,
		"- Env[OLD]: x",
		"~ Count: 1 => 2",
		`Task "server" (Edited)`,
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "sidecar") || strings.Contains(errOut, "port") {
		t.Errorf("the diff shows what does not change:\n%s", errOut)
	}

	post := s.seen[len(s.seen)-1]
	if post.method != "POST" || post.body != `{"spec_hash":"hash-1"}` || post.auth != "Bearer "+token {
		t.Errorf("approve request = %+v, want a POST with the hash it showed and the token", post)
	}
}

func TestApproveAsksBeforeActing(t *testing.T) {
	for name, stdin := range map[string]string{"no": "n\n", "empty": "\n", "closed stdin": ""} {
		t.Run(name, func(t *testing.T) {
			s, addr := newStub(t, nil)
			s.answers = map[string]answer{"GET /api/deployments/d1": {200, deploymentBody + `}`}}
			code, _, errOut := run(t, envFor(addr), stdin, "approve", "d1")
			if code != 1 || !strings.Contains(errOut, "aborted") {
				t.Errorf("exit %d, stderr %q, want 1 and aborted", code, errOut)
			}
			for _, r := range s.seen {
				if r.method != "GET" {
					t.Errorf("sent %s %s after a no", r.method, r.path)
				}
			}
		})
	}

	t.Run("-yes", func(t *testing.T) {
		s, addr := newStub(t, nil)
		s.answers = map[string]answer{
			"GET /api/deployments/d1":          {200, deploymentBody + `}`},
			"POST /api/deployments/d1/approve": {204, ""},
		}
		if code, _, errOut := run(t, envFor(addr), "", "approve", "-yes", "d1"); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
	})
}

func TestApproveRefusedByTheServerIsAnError(t *testing.T) {
	s, addr := newStub(t, nil)
	s.answers = map[string]answer{
		"GET /api/deployments/d1":          {200, deploymentBody + `}`},
		"POST /api/deployments/d1/approve": {409, `{"error":"the spec changed since you read it"}`},
	}
	code, out, errOut := run(t, envFor(addr), "", "approve", "-yes", "d1")
	if code != 1 || out != "" || !strings.Contains(errOut, "nops answered 409: the spec changed since you read it") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestDeployNowSendsTheSpecHashOfTheJob(t *testing.T) {
	s, addr := newStub(t, nil)
	s.answers = map[string]answer{
		"GET /api/jobs/default/web": {200, `{"namespace":"default","job":"web","policy":"auto","sync":"held","spec_hash":"hash-9",` +
			`"hold":{"kind":"sync_window","reason":"closed"},"diff":` + diffJSON(t) + `,"deployments":[]}`},
		"POST /api/jobs/default/web/deploy-now": {200, `{"deployment_id":"d2"}`},
	}
	code, out, errOut := run(t, envFor(addr), "y\n", "deploy-now", "web")
	if code != 0 || out != "deployment d2\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got := s.seen[len(s.seen)-1].body; got != `{"spec_hash":"hash-9"}` {
		t.Errorf("body = %s", got)
	}
}

func TestNamespaceComesFromTheFlagThenTheVariable(t *testing.T) {
	s, addr := newStub(t, nil)
	s.answers = map[string]answer{
		"GET /api/jobs/prod/web":    {200, `{"namespace":"prod","job":"web","deployments":[]}`},
		"GET /api/jobs/staging/web": {200, `{"namespace":"staging","job":"web","deployments":[]}`},
	}
	env := envFor(addr)
	env["NOPS_NAMESPACE"] = "prod"
	if code, out, _ := run(t, env, "", "job", "web"); code != 0 || !strings.Contains(out, "prod/web") {
		t.Errorf("the variable: exit %d, stdout %q", code, out)
	}
	if code, out, _ := run(t, env, "", "job", "-namespace", "staging", "web"); code != 0 || !strings.Contains(out, "staging/web") {
		t.Errorf("the flag: exit %d, stdout %q", code, out)
	}
	if code, _, _ := run(t, envFor(addr), "", "job", "web"); code != 1 || s.seen[len(s.seen)-1].path != "/api/jobs/default/web" {
		t.Errorf("default: exit %d, last request %+v, want /api/jobs/default/web", code, s.seen[len(s.seen)-1])
	}
}

func TestAJobIDWithASlashIsOneSegment(t *testing.T) {
	s, addr := newStub(t, nil)
	run(t, envFor(addr), "", "job", "a/b")
	if got := s.seen[0].path; got != "/api/jobs/default/a%2Fb" {
		t.Errorf("path = %s, want the slash escaped", got)
	}
}

func TestActionsPostToTheirEndpoint(t *testing.T) {
	tests := []struct {
		args       []string
		path, body string
		answer     answer
		out        string
	}{
		{[]string{"reject", "d1"}, "/api/deployments/d1/reject", "", answer{204, ""}, "rejected d1\n"},
		{[]string{"promote", "d1"}, "/api/deployments/d1/promote", "", answer{204, ""}, "promoted d1\n"},
		{[]string{"retry", "d1"}, "/api/deployments/d1/retry", "", answer{200, `{"deployment_id":"d3"}`}, "deployment d3\n"},
		{[]string{"pause", "-reason", "db move", "web"}, "/api/jobs/default/web/pause", `{"reason":"db move"}`, answer{204, ""}, "paused web\n"},
		{[]string{"pause", "web"}, "/api/jobs/default/web/pause", "", answer{204, ""}, "paused web\n"},
		{[]string{"resume", "web"}, "/api/jobs/default/web/resume", "", answer{204, ""}, "resumed web\n"},
		{[]string{"fetch"}, "/api/fetch", "", answer{202, ""}, "asked for a poll\n"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			s, addr := newStub(t, nil)
			s.answers = map[string]answer{"POST " + tt.path: tt.answer}
			code, out, errOut := run(t, envFor(addr), "", tt.args...)
			if code != 0 || out != tt.out {
				t.Fatalf("exit %d, stdout %q, stderr %q; want %q", code, out, errOut, tt.out)
			}
			if got := s.seen[0]; got.body != tt.body || got.auth != "Bearer "+token {
				t.Errorf("request = %+v, want body %q and the token", got, tt.body)
			}
		})
	}
}

func TestReadsPrintTextOrJSON(t *testing.T) {
	list := `{"deployments":[{"id":"d1","namespace":"default","job":"web","state":"pending_approval","policy":"approval","commit_sha":"0123456789","created_at":"2026-10-05T10:00:00Z"}]}`
	jobs := `{"jobs":[{"namespace":"default","job":"web","policy":"approval","sync":"pending","last_deployment":{"id":"d1","state":"pending_approval"}}]}`
	s, addr := newStub(t, nil)
	s.answers = map[string]answer{
		"GET /api/deployments": {200, list},
		"GET /api/jobs":        {200, jobs},
	}

	_, out, _ := run(t, envFor(addr), "", "deployments")
	for _, want := range []string{"ID", "d1", "pending_approval", "0123456"} {
		if !strings.Contains(out, want) {
			t.Errorf("deployments lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = run(t, envFor(addr), "", "jobs")
	for _, want := range []string{"NAMESPACE", "web", "pending", "d1 pending_approval"} {
		if !strings.Contains(out, want) {
			t.Errorf("jobs lacks %q:\n%s", want, out)
		}
	}

	_, out, _ = run(t, envFor(addr), "", "jobs", "-json")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["jobs"] == nil {
		t.Errorf("jobs -json = %q (%v), want the API's JSON", out, err)
	}
}

func TestAnErrorOfTheAPIIsPrintedAndExits1(t *testing.T) {
	for status, want := range map[int]string{401: "the API token is not valid", 404: "this deployment does not exist"} {
		s, addr := newStub(t, nil)
		s.answers = map[string]answer{"GET /api/deployments/d1": {status, `{"error":"` + want + `"}`}}
		code, out, errOut := run(t, envFor(addr), "", "deployment", "d1")
		if code != 1 || out != "" || !strings.Contains(errOut, want) {
			t.Errorf("%d: exit %d, stdout %q, stderr %q", status, code, out, errOut)
		}
	}

	// A server that is not Nops answers something else.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>") }))
	defer html.Close()
	if code, _, errOut := run(t, envFor(html.URL), "", "jobs"); code != 1 || !strings.Contains(errOut, "is the URL the one of Nops") {
		t.Errorf("not Nops: exit %d, stderr %q", code, errOut)
	}
}

func TestConfiguration(t *testing.T) {
	s, addr := newStub(t, nil)
	s.answers = map[string]answer{"GET /api/jobs": {200, `{"jobs":[]}`}}
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The URL and the token come from flags too, and a file wins over NOPS_TOKEN.
	if code, _, errOut := run(t, map[string]string{"NOPS_TOKEN": "from-env"}, "", "jobs", "-addr", addr, "-token-file", file); code != 0 {
		t.Fatalf("flags: exit %d: %s", code, errOut)
	}
	if got := s.seen[0].auth; got != "Bearer from-file" {
		t.Errorf("auth = %q, want the file's token, trimmed", got)
	}
	// A flag wins over the variable.
	other, otherAddr := newStub(t, map[string]answer{"GET /api/jobs": {200, `{"jobs":[]}`}})
	env := envFor(addr)
	run(t, env, "", "jobs", "-addr", otherAddr)
	if len(other.seen) != 1 {
		t.Errorf("-addr did not win over NOPS_ADDR")
	}
	// A sub path of the URL stays in front of /api.
	run(t, envFor(addr+"/nops/"), "", "jobs")
	if got := s.seen[len(s.seen)-1].path; got != "/nops/api/jobs" {
		t.Errorf("path = %s, want /nops/api/jobs", got)
	}

	for name, tt := range map[string]struct {
		env  map[string]string
		args []string
		code int
		want string
	}{
		"no URL":          {map[string]string{"NOPS_TOKEN": "t"}, []string{"jobs"}, 2, "NOPS_ADDR"},
		"no token":        {map[string]string{"NOPS_ADDR": addr}, []string{"jobs"}, 2, "NOPS_TOKEN_FILE"},
		"a bad URL":       {map[string]string{"NOPS_ADDR": "nops.example.com", "NOPS_TOKEN": "t"}, []string{"jobs"}, 2, "http:// or https://"},
		"a missing file":  {map[string]string{"NOPS_ADDR": addr}, []string{"jobs", "-token-file", filepath.Join(dir, "none")}, 1, "read the token file"},
		"an empty file":   {map[string]string{"NOPS_ADDR": addr}, []string{"jobs", "-token-file", writeFile(t, dir, "empty", "\n")}, 1, "is empty"},
		"no argument":     {envFor(addr), []string{"approve"}, 2, "takes one argument"},
		"flag after args": {envFor(addr), []string{"approve", "d1", "-yes"}, 2, "put the flags before the argument"},
		"extra argument":  {envFor(addr), []string{"jobs", "x"}, 2, "takes no argument"},
	} {
		code, _, errOut := run(t, tt.env, "", tt.args...)
		if code != tt.code || !strings.Contains(errOut, tt.want) {
			t.Errorf("%s: exit %d, stderr %q, want exit %d saying %q", name, code, errOut, tt.code, tt.want)
		}
		if strings.Contains(errOut, "from-file") || strings.Contains(errOut, token) {
			t.Errorf("%s: an error shows a token: %q", name, errOut)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheRootCommand(t *testing.T) {
	code, out, _ := run(t, nil, "", "-version")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Errorf("-version: exit %d, stdout %q, want the version", code, out)
	}
	code, out, _ = run(t, nil, "", "-h")
	if code != 0 {
		t.Errorf("-h: exit %d", code)
	}
	for _, c := range commands {
		if !strings.Contains(out, c.name) {
			t.Errorf("usage does not list %s", c.name)
		}
	}
	if code, _, errOut := run(t, nil, ""); code != 2 || !strings.Contains(errOut, "Usage") {
		t.Errorf("no command: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := run(t, nil, "", "frobnicate"); code != 2 || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Errorf("unknown command: exit %d, stderr %q", code, errOut)
	}
	// The old form of the server's command line says where the flags went.
	if code, _, errOut := run(t, nil, "", "-git-url", "https://x"); code != 2 || !strings.Contains(errOut, "nops serve") {
		t.Errorf("a server flag: exit %d, stderr %q, want a hint to nops serve", code, errOut)
	}
	if code, _, _ := run(t, nil, "", "approve", "-h"); code != 0 {
		t.Errorf("approve -h: exit %d, want 0", code)
	}
}

func TestEveryCommandIsDocumented(t *testing.T) {
	b, err := os.ReadFile("../../docs/cli.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commands {
		if !strings.Contains(string(b), "`nops "+strings.TrimRight(c.name+" "+c.arg, " ")+"`") {
			t.Errorf("docs/cli.md has no row for nops %s %s", c.name, c.arg)
		}
	}
	for _, v := range Variables {
		if !strings.Contains(string(b), "`"+v+"`") {
			t.Errorf("docs/cli.md does not mention %s", v)
		}
	}
}

func TestWriteDiffOfNoChanges(t *testing.T) {
	var b bytes.Buffer
	if err := writeDiff(&b, nil); err != nil || b.String() != "No changes.\n" {
		t.Errorf("writeDiff(nil) = %q, %v", b.String(), err)
	}
}
