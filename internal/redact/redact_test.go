package redact

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
)

// build places one field at the given level (job, group or task), inside the
// nested objects named in path.
func build(level string, path []string, f *api.FieldDiff) *api.JobDiff {
	var objs []*api.ObjectDiff
	fs := []*api.FieldDiff{f}
	for i := len(path) - 1; i >= 0; i-- {
		objs = []*api.ObjectDiff{{Type: "Edited", Name: path[i], Fields: fs, Objects: objs}}
		fs = nil
	}
	d := &api.JobDiff{Type: "Edited", ID: "web"}
	switch level {
	case "job":
		d.Fields, d.Objects = fs, objs
	case "group":
		d.TaskGroups = []*api.TaskGroupDiff{{Type: "Edited", Name: "g", Fields: fs, Objects: objs}}
	case "task":
		d.TaskGroups = []*api.TaskGroupDiff{{Type: "Edited", Name: "g",
			Tasks: []*api.TaskDiff{{Type: "Edited", Name: "t", Fields: fs, Objects: objs}}}}
	}
	return d
}

// find returns the field placed by build.
func find(t *testing.T, d *api.JobDiff, level string, path []string) *api.FieldDiff {
	t.Helper()
	fs, objs := d.Fields, d.Objects
	switch level {
	case "group":
		fs, objs = d.TaskGroups[0].Fields, d.TaskGroups[0].Objects
	case "task":
		fs, objs = d.TaskGroups[0].Tasks[0].Fields, d.TaskGroups[0].Tasks[0].Objects
	}
	for range path {
		fs, objs = objs[0].Fields, objs[0].Objects
	}
	if len(fs) != 1 {
		t.Fatalf("field not found at %s %v", level, path)
	}
	return fs[0]
}

func redacted(t *testing.T, d *api.JobDiff) *api.JobDiff {
	t.Helper()
	b, err := Diff(d)
	if err != nil {
		t.Fatal(err)
	}
	var out *api.JobDiff
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("output is not a JobDiff: %v\n%s", err, b)
	}
	return out
}

func TestDiffRules(t *testing.T) {
	tests := []struct {
		name     string
		path     []string // enclosing objects
		field    string
		old, new string
		wantOld  string
		wantNew  string
	}{
		// Secrets.
		{"env var", nil, "Env[DB_URL]", "a", "b", Marker, Marker},
		{"env var with a harmless name", nil, "Env[LOG_LEVEL]", "info", "debug", Marker, Marker},
		{"template body", []string{"Template"}, "EmbeddedTmpl", "PASS=a", "PASS=b", Marker, Marker},
		{"artifact header", []string{"Artifact"}, "GetterHeaders[X-Custom]", "a", "b", Marker, Marker},
		{"check header", []string{"Service", "Check", "Header"}, "X-Custom[0]", "a", "b", Marker, Marker},
		{"password in meta", nil, "Meta[db_password]", "a", "b", Marker, Marker},
		{"token", nil, "Meta[api_token]", "a", "b", Marker, Marker},
		{"secret", nil, "Meta[ClientSecret]", "a", "b", Marker, Marker},
		{"authorization", []string{"Service", "Check", "Header"}, "Authorization[0]", "Bearer a", "Bearer b", Marker, Marker},
		{"credential", nil, "Meta[aws_credentials]", "a", "b", Marker, Marker},
		{"private key with underscore", []string{"Config"}, "private_key", "a", "b", Marker, Marker},
		{"private key with dash", []string{"Config"}, "Private-Key", "a", "b", Marker, Marker},
		{"api key with dash", []string{"Artifact"}, "X-Api-Key", "a", "b", Marker, Marker},
		{"apikey", nil, "Meta[APIKEY]", "a", "b", Marker, Marker},
		{"docker auth, flattened", []string{"Config"}, "auth[0][password]", "a", "b", Marker, Marker},
		{"docker auth username, flattened", []string{"Config"}, "auth[0][username]", "a", "b", Marker, Marker},
		{"field below a secret object", []string{"Config", "auth"}, "username", "a", "b", Marker, Marker},
		{"field deep below a secret object", []string{"Vault", "Token", "Inner"}, "value", "a", "b", Marker, Marker},

		// Added and removed secrets stay added and removed.
		{"added secret", nil, "Env[NEW]", "", "b", "", Marker},
		{"removed secret", nil, "Env[OLD]", "a", "", Marker, ""},

		// Credentials in a URL, whatever the field.
		{"url credentials", []string{"Artifact"}, "GetterSource", "https://u:p1@git.example.com/r.git", "git::https://u:p2@git.example.com/r.git",
			"https://" + Marker + "@git.example.com/r.git", "git::https://" + Marker + "@git.example.com/r.git"},
		{"url without credentials", []string{"Artifact"}, "GetterSource", "https://git.example.com/r.git", "https://u@git.example.com/r.git",
			"https://git.example.com/r.git", "https://u@git.example.com/r.git"},

		// Not secrets: untouched.
		{"count", nil, "Count", "1", "2", "1", "2"},
		{"image", []string{"Config"}, "image", "busybox:1", "busybox:2", "busybox:1", "busybox:2"},
		{"plain meta", nil, "Meta[owner]", "a", "b", "a", "b"},
		{"template destination", []string{"Template"}, "DestPath", "local/a", "local/b", "local/a", "local/b"},
		{"not an env var", nil, "Environment", "a", "b", "a", "b"},
		// Known limit: a secret inside a value under a harmless name is not
		// detected. Secrets belong in env or templates.
		{"secret in args", []string{"Config"}, "args[0]", "--db-password=a", "--db-password=b", "--db-password=a", "--db-password=b"},
		// Known false positive of the conservative reading: "auth" is a
		// substring of "author".
		{"author", nil, "Meta[author]", "a", "b", Marker, Marker},
	}
	for _, tt := range tests {
		for _, level := range []string{"job", "group", "task"} {
			t.Run(level+"/"+tt.name, func(t *testing.T) {
				in := build(level, tt.path, &api.FieldDiff{Type: "Edited", Name: tt.field, Old: tt.old, New: tt.new})
				got := find(t, redacted(t, in), level, tt.path)
				if got.Old != tt.wantOld || got.New != tt.wantNew {
					t.Errorf("%s: got %q -> %q, want %q -> %q", tt.field, got.Old, got.New, tt.wantOld, tt.wantNew)
				}
				if got.Type != "Edited" || got.Name != tt.field {
					t.Errorf("type or name changed: %s %s", got.Type, got.Name)
				}
			})
		}
	}
}

// TestDiffRealPlan runs on the diff Nomad 2.0.3 returned for the job in
// tests/integration/redact_test.go (probeHCL), moving every value from
// "old-..." to "new-...". Every env var is a secret, even PLAIN.
func TestDiffRealPlan(t *testing.T) {
	b, err := os.ReadFile("testdata/plan_diff.json")
	if err != nil {
		t.Fatal(err)
	}
	var in *api.JobDiff
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	var before *api.JobDiff
	if err := json.Unmarshal(b, &before); err != nil {
		t.Fatal(err)
	}

	out, err := Diff(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"old", "new"} {
		for _, secret := range []string{"meta-token", "check-header", "docker-password", "user", "env-secret", "env-plain", "template-body", "artifact-header"} {
			if strings.Contains(string(out), v+"-"+secret) {
				t.Errorf("secret %s-%s reached the output", v, secret)
			}
		}
		for _, kept := range []string{v + "-owner", "busybox:" + v, v + "-getter-opt", "example.com/" + v + ".tar.gz"} {
			if !strings.Contains(string(out), kept) {
				t.Errorf("%s is not a secret but is missing from the output", kept)
			}
		}
	}
	if !strings.Contains(string(out), Marker) {
		t.Errorf("the output does not contain %s verbatim", Marker)
	}
	if !reflect.DeepEqual(in, before) {
		t.Error("Diff modified its input")
	}
	// Every change is still visible as a change.
	if strings.Count(string(out), `"Type":"Edited"`) != strings.Count(string(b), `"Type": "Edited"`) {
		t.Error("the number of edited entries changed")
	}
}

func TestDiffNil(t *testing.T) {
	b, err := Diff(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Errorf("Diff(nil) = %s, want null", b)
	}
}

func TestDiffNilEntries(t *testing.T) {
	d := &api.JobDiff{
		Fields:     []*api.FieldDiff{nil},
		Objects:    []*api.ObjectDiff{nil, {Name: "x", Fields: []*api.FieldDiff{nil}}},
		TaskGroups: []*api.TaskGroupDiff{nil, {Tasks: []*api.TaskDiff{nil}}},
	}
	if _, err := Diff(d); err != nil {
		t.Fatal(err)
	}
}
