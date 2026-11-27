package gitwatch

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// twoRepos is a Group of two local repositories, "default" and "apps", each
// with one job file, started.
func twoRepos(t *testing.T, poll time.Duration) (g *Group, def, apps *testRepo) {
	t.Helper()
	def, apps = newTestRepo(t, "main"), newTestRepo(t, "main")
	def.commit(map[string]string{"api.nomad.hcl": `job "api" {}`})
	apps.commit(map[string]string{"nomad/web.nomad.hcl": `job "web" {}`})
	log, _ := testLogger()
	g = NewGroup(
		New(Options{Name: "default", URL: def.url, Branch: "main", PollInterval: poll, Timeout: time.Minute}, log),
		New(Options{Name: "apps", URL: apps.url, Branch: "main", Path: "nomad", PollInterval: poll, Timeout: time.Minute}, log),
	)
	if err := g.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g, def, apps
}

func TestGroupSnapshotsNameTheirRepository(t *testing.T) {
	g, _, _ := twoRepos(t, time.Hour)
	snaps := g.Snapshots()
	if len(snaps) != 2 || snaps[0].Repository != "default" || snaps[1].Repository != "apps" {
		t.Fatalf("snapshots = %+v, want default then apps", snaps)
	}
	if got := fileNames(snaps[1].Files); len(got) != 1 || got[0] != "nomad/web.nomad.hcl" {
		t.Errorf("apps files = %v, want [nomad/web.nomad.hcl]", got)
	}
	repos := g.Repositories()
	if len(repos) != 2 || repos[1].Name != "apps" || repos[1].Snapshot.Commit != snaps[1].Commit || repos[1].Status.CheckedAt.IsZero() {
		t.Errorf("repositories = %+v", repos)
	}
}

func TestGroupStartFailsOnAnyRepository(t *testing.T) {
	ok := newTestRepo(t, "main")
	ok.commit(map[string]string{"api.nomad.hcl": `job "api" {}`})
	log, _ := testLogger()
	g := NewGroup(
		New(Options{Name: "default", URL: ok.url, Branch: "main", PollInterval: time.Hour, Timeout: time.Minute}, log),
		New(Options{Name: "apps", URL: ok.url, Branch: "no-such-branch", PollInterval: time.Hour, Timeout: time.Minute}, log),
	)
	err := g.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"apps"`) {
		t.Fatalf("Start = %v, want an error naming the apps repository", err)
	}
}

func TestGroupFailedPollKeepsOnlyThatRepository(t *testing.T) {
	g, def, apps := twoRepos(t, time.Hour)
	before := g.Snapshots()
	ctx := context.Background()

	// apps becomes unreachable, default gets a new commit.
	if err := os.RemoveAll(apps.bareDir); err != nil {
		t.Fatal(err)
	}
	second := def.commit(map[string]string{"api.nomad.hcl": `job "api" { v = 2 }`})
	for _, w := range g.watchers {
		w.poll(ctx)
	}

	repos := g.Repositories()
	if repos[0].Snapshot.Commit != second || repos[0].Status.Error != "" {
		t.Errorf("default = %+v, want the new commit and no error", repos[0])
	}
	if repos[1].Snapshot.Commit != before[1].Commit || repos[1].Status.Error == "" {
		t.Errorf("apps = %+v, want its last good snapshot and the error", repos[1])
	}
}

func TestGroupRunSignalsAnyRepositoryAndTriggersAll(t *testing.T) {
	g, def, apps := twoRepos(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()

	wait := func(what string) {
		t.Helper()
		select {
		case <-g.Changed():
		case <-time.After(2 * time.Second):
			t.Fatalf("no change signalled after %s", what)
		}
	}

	second := apps.commit(map[string]string{"nomad/web.nomad.hcl": `job "web" { v = 2 }`})
	g.Trigger()
	wait("a commit in apps")
	if got := g.Snapshots()[1].Commit; got != second {
		t.Errorf("apps commit = %s, want %s", got, second)
	}

	third := def.commit(map[string]string{"api.nomad.hcl": `job "api" { v = 2 }`})
	g.Trigger()
	wait("a commit in default")
	if got := g.Snapshots()[0].Commit; got != third {
		t.Errorf("default commit = %s, want %s", got, third)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}

func TestGroupCommitURL(t *testing.T) {
	log, _ := testLogger()
	g := NewGroup(
		New(Options{Name: "default", URL: "https://git.example.com/ops/jobs.git"}, log),
		New(Options{Name: "apps", URL: "https://git.example.com/ops/apps.git"}, log),
	)
	if got, want := g.CommitURL("apps", "abc"), "https://git.example.com/ops/apps/commit/abc"; got != want {
		t.Errorf("CommitURL(apps) = %q, want %q", got, want)
	}
	if got := g.CommitURL("other", "abc"); got != "" {
		t.Errorf("CommitURL(other) = %q, want empty", got)
	}
}

// Select reads the files a client sends as a poll reads the same tree: under
// the repository's path, each job file with its vars file.
func TestGroupSelectReadsFilesAsAPoll(t *testing.T) {
	files := map[string]string{
		"nomad/api.nomad.hcl":     `job "api" {}`,
		"nomad/api.vars.hcl":      `tag = "1"`,
		"nomad/sub/worker.nomad":  `job "worker" {}`,
		"nomad/lone.vars.hcl":     `unused = true`,
		"nomad/README.md":         "not a job",
		"other/ignored.nomad.hcl": `job "ignored" {}`,
		"nomadic/ignored.nomad":   `job "ignored" {}`,
		"top.nomad.hcl":           `job "top" {}`,
	}
	r := newTestRepo(t, "main")
	r.commit(files)
	log, _ := testLogger()
	g := NewGroup(New(Options{Name: "apps", URL: r.url, Branch: "main", Path: "nomad", PollInterval: time.Hour, Timeout: time.Minute}, log))
	if err := g.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, ok := g.Select("apps", files)
	if !ok {
		t.Fatal("Select does not know the repository apps")
	}
	if want := g.Snapshots()[0].Files; !reflect.DeepEqual(got, want) {
		t.Errorf("Select = %+v, want what the poll read: %+v", got, want)
	}
	if _, ok := g.Select("nope", files); ok {
		t.Error("Select knows a repository the group does not have")
	}
}
