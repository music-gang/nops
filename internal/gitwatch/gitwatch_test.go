package gitwatch

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// testRepo is a local bare repository (the "remote") plus a working clone
// used to push commits to it, all over a file:// URL. go-git's file
// transport shells out to the git binary, so tests skip when it is missing.
type testRepo struct {
	t       *testing.T
	workdir string
	repo    *git.Repository
	wt      *git.Worktree
	bareDir string
	url     string
	branch  string
}

func newTestRepo(t *testing.T, branch string) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH: go-git's file transport needs it")
	}

	bareDir := filepath.Join(t.TempDir(), "origin.git")
	if _, err := git.PlainInit(bareDir, true); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}

	workdir := t.TempDir()
	repo, err := git.PlainInitWithOptions(workdir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)},
	})
	if err != nil {
		t.Fatalf("init work repo: %v", err)
	}
	url := "file://" + bareDir
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	return &testRepo{t: t, workdir: workdir, repo: repo, wt: wt, bareDir: bareDir, url: url, branch: branch}
}

// commit writes files (repo-relative path -> content), commits and pushes.
func (r *testRepo) commit(files map[string]string) string {
	r.t.Helper()
	for p, content := range files {
		full := filepath.Join(r.workdir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatalf("mkdir for %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			r.t.Fatalf("write %s: %v", p, err)
		}
		if _, err := r.wt.Add(p); err != nil {
			r.t.Fatalf("add %s: %v", p, err)
		}
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0)}
	hash, err := r.wt.Commit("test commit", &git.CommitOptions{Author: sig})
	if err != nil {
		r.t.Fatalf("commit: %v", err)
	}
	if err := r.repo.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		r.t.Fatalf("push: %v", err)
	}
	return hash.String()
}

// forcePush resets the branch to an orphan commit (no shared history with
// what is already on the remote) with the given files, and force-pushes it.
func (r *testRepo) forcePush(files map[string]string) string {
	r.t.Helper()
	if err := r.wt.Reset(&git.ResetOptions{Mode: git.HardReset}); err != nil {
		r.t.Fatalf("reset: %v", err)
	}
	for p := range files {
		os.Remove(filepath.Join(r.workdir, filepath.FromSlash(p)))
	}
	for p, content := range files {
		full := filepath.Join(r.workdir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
		if _, err := r.wt.Add(p); err != nil {
			r.t.Fatal(err)
		}
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0)}
	hash, err := r.wt.Commit("orphan commit", &git.CommitOptions{
		Author:  sig,
		Parents: []plumbing.Hash{}, // no parent: unrelated history
	})
	if err != nil {
		r.t.Fatalf("orphan commit: %v", err)
	}
	spec := config.RefSpec("+" + plumbing.NewBranchReferenceName(r.branch).String() + ":" + plumbing.NewBranchReferenceName(r.branch).String())
	if err := r.repo.Push(&git.PushOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{spec}, Force: true}); err != nil {
		r.t.Fatalf("force push: %v", err)
	}
	return hash.String()
}

func fileNames(files []File) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Path
	}
	return names
}

func TestStartInitialClone(t *testing.T) {
	r := newTestRepo(t, "main")
	commit := r.commit(map[string]string{
		"api.nomad.hcl": `job "api" {}`,
	})

	log, _ := testLogger()
	w := New(Options{URL: r.url, Branch: "main", PollInterval: time.Hour}, log)
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := w.Snapshot()
	if snap.Commit != commit {
		t.Errorf("Commit = %s, want %s", snap.Commit, commit)
	}
	if len(snap.Files) != 1 || snap.Files[0].Path != "api.nomad.hcl" || snap.Files[0].Content != `job "api" {}` {
		t.Errorf("Files = %+v", snap.Files)
	}
	if snap.Files[0].VarsPath != "" {
		t.Errorf("VarsPath = %q, want empty", snap.Files[0].VarsPath)
	}
	if snap.Subject != "test commit" || snap.Author != "test" || !snap.CommittedAt.Equal(time.Unix(0, 0)) {
		t.Errorf("commit info = %q by %q at %v, want \"test commit\" by \"test\" at the epoch",
			snap.Subject, snap.Author, snap.CommittedAt)
	}
}

func TestStatusTracksPolls(t *testing.T) {
	r := newTestRepo(t, "main")
	r.commit(map[string]string{"api.nomad.hcl": `job "api" {}`})

	log, _ := testLogger()
	w := New(Options{URL: r.url, Branch: "main", PollInterval: time.Hour}, log)
	clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return clock }
	ctx := context.Background()

	if got := w.Status(); !got.CheckedAt.IsZero() || got.Error != "" {
		t.Fatalf("Status before Start = %+v, want zero", got)
	}
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	started := clock
	if got := w.Status(); !got.CheckedAt.Equal(started) || got.Error != "" {
		t.Fatalf("Status after Start = %+v, want checked at %v, no error", got, started)
	}

	// A poll that finds nothing new still counts as a check.
	clock = clock.Add(time.Minute)
	w.poll(ctx)
	if got := w.Status(); !got.CheckedAt.Equal(clock) || got.Error != "" {
		t.Fatalf("Status after an idle poll = %+v, want checked at %v", got, clock)
	}
	checked := clock

	// The remote goes away: the failure is recorded and CheckedAt is kept.
	moved := r.bareDir + ".away"
	if err := os.Rename(r.bareDir, moved); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	w.poll(ctx)
	got := w.Status()
	if got.Error == "" || !got.ErrorAt.Equal(clock) || !got.CheckedAt.Equal(checked) {
		t.Fatalf("Status after a failed poll = %+v, want an error at %v and CheckedAt still %v", got, clock, checked)
	}

	// It comes back: the next poll clears the error.
	if err := os.Rename(moved, r.bareDir); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	w.poll(ctx)
	if got := w.Status(); got.Error != "" || !got.CheckedAt.Equal(clock) {
		t.Fatalf("Status after recovery = %+v, want no error, checked at %v", got, clock)
	}

	// A new commit is a successful poll too.
	r.commit(map[string]string{"api.nomad.hcl": `job "api" { count = 2 }`})
	clock = clock.Add(time.Minute)
	w.poll(ctx)
	if got := w.Status(); got.Error != "" || !got.CheckedAt.Equal(clock) {
		t.Fatalf("Status after a new commit = %+v, want checked at %v", got, clock)
	}
}

func TestSubject(t *testing.T) {
	for msg, want := range map[string]string{
		"fix(api): bump the image":              "fix(api): bump the image",
		"first line\n\nbody paragraph\n":        "first line",
		"  padded  \n":                          "padded",
		"":                                      "",
		"\n\nleading blank lines are trimmed\n": "leading blank lines are trimmed",
	} {
		if got := subject(msg); got != want {
			t.Errorf("subject(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestCommitURL(t *testing.T) {
	const sha = "0123456789abcdef"
	cases := []struct{ name, repo, want string }{
		{"github", "https://github.com/music-gang/nops.git", "https://github.com/music-gang/nops/commit/" + sha},
		{"no .git suffix", "https://git.example.com/ops/jobs", "https://git.example.com/ops/jobs/commit/" + sha},
		{"trailing slash", "https://git.example.com/ops/jobs/", "https://git.example.com/ops/jobs/commit/" + sha},
		{"credentials dropped", "https://user:secret@git.example.com/ops/jobs.git", "https://git.example.com/ops/jobs/commit/" + sha},
		{"query and fragment dropped", "https://git.example.com/ops/jobs.git?x=1#f", "https://git.example.com/ops/jobs/commit/" + sha},
		{"port kept", "https://git.example.com:8443/ops/jobs.git", "https://git.example.com:8443/ops/jobs/commit/" + sha},
		{"file transport", "file:///srv/git/jobs.git", ""},
		{"ssh scp form", "git@github.com:music-gang/nops.git", ""},
		{"no host", "https:///ops/jobs.git", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := CommitURL(c.repo, sha); got != c.want {
			t.Errorf("%s: CommitURL(%q) = %q, want %q", c.name, c.repo, got, c.want)
		}
	}
	if got := CommitURL("https://git.example.com/ops/jobs.git", ""); got != "" {
		t.Errorf("CommitURL with no sha = %q, want empty", got)
	}
}

func TestPollNewCommit(t *testing.T) {
	r := newTestRepo(t, "main")
	r.commit(map[string]string{"api.nomad.hcl": `job "api" {}`})

	log, _ := testLogger()
	w := New(Options{URL: r.url, Branch: "main", PollInterval: time.Hour}, log)
	ctx := context.Background()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	first := w.Snapshot().Commit

	// No change: poll does nothing, no signal.
	w.poll(ctx)
	select {
	case <-w.Changed():
		t.Fatal("Changed fired with no new commit")
	default:
	}
	if w.Snapshot().Commit != first {
		t.Fatal("snapshot changed with no new commit")
	}

	second := r.commit(map[string]string{"api.nomad.hcl": `job "api" { count = 2 }`})
	w.poll(ctx)
	select {
	case <-w.Changed():
	default:
		t.Fatal("Changed did not fire after a new commit")
	}
	// Drained already: a second read must not have another pending signal.
	select {
	case <-w.Changed():
		t.Fatal("Changed fired twice for one commit")
	default:
	}
	if got := w.Snapshot(); got.Commit != second || got.Files[0].Content != `job "api" { count = 2 }` {
		t.Errorf("snapshot after poll = %+v", got)
	}
}

func TestForcePush(t *testing.T) {
	r := newTestRepo(t, "main")
	r.commit(map[string]string{"api.nomad.hcl": `job "api" { v = 1 }`})

	log, _ := testLogger()
	w := New(Options{URL: r.url, Branch: "main", PollInterval: time.Hour}, log)
	ctx := context.Background()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	before := w.Snapshot().Commit

	after := r.forcePush(map[string]string{"api.nomad.hcl": `job "api" { v = 2 }`})
	if after == before {
		t.Fatal("test setup: force-pushed commit should differ")
	}
	w.poll(ctx)
	select {
	case <-w.Changed():
	default:
		t.Fatal("Changed did not fire after a force-push")
	}
	got := w.Snapshot()
	if got.Commit != after || got.Files[0].Content != `job "api" { v = 2 }` {
		t.Errorf("snapshot after force-push = %+v", got)
	}
}

func TestTriggerCoalesces(t *testing.T) {
	log, _ := testLogger()
	w := New(Options{PollInterval: time.Hour}, log)
	w.Trigger()
	w.Trigger()
	w.Trigger()
	if len(w.trigger) != 1 {
		t.Errorf("trigger channel has %d pending, want 1", len(w.trigger))
	}
}

func TestVarsPairing(t *testing.T) {
	r := newTestRepo(t, "main")
	r.commit(map[string]string{
		"api.nomad.hcl":   `job "api" {}`,
		"api.vars.hcl":    `image_tag = "1.2.3"`,
		"worker.nomad":    `job "worker" {}`,
		"worker.vars.hcl": `count = 3`,
		"orphan.vars.hcl": `unused = true`,
		"README.md":       "not a job",
		"terraform.tf":    "not a job either",
	})

	log, buf := testLogger()
	w := New(Options{URL: r.url, Branch: "main", PollInterval: time.Hour}, log)
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := w.Snapshot()
	if len(snap.Files) != 2 {
		t.Fatalf("Files = %v, want 2 job files", fileNames(snap.Files))
	}
	byPath := map[string]File{}
	for _, f := range snap.Files {
		byPath[f.Path] = f
	}
	api, ok := byPath["api.nomad.hcl"]
	if !ok || api.VarsPath != "api.vars.hcl" || api.Vars != `image_tag = "1.2.3"` {
		t.Errorf("api.nomad.hcl = %+v", api)
	}
	worker, ok := byPath["worker.nomad"]
	if !ok || worker.VarsPath != "worker.vars.hcl" || worker.Vars != `count = 3` {
		t.Errorf("worker.nomad = %+v", worker)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "orphan.vars.hcl") {
		t.Errorf("expected a WARN naming orphan.vars.hcl, got: %s", buf)
	}
}

func TestGitPathScoping(t *testing.T) {
	r := newTestRepo(t, "main")
	r.commit(map[string]string{
		"jobs/api.nomad.hcl":      `job "api" {}`,
		"jobs/notes.md":           "ignored",
		"other/ignored.nomad.hcl": `job "ignored" {}`,
	})

	log, _ := testLogger()
	w := New(Options{URL: r.url, Branch: "main", Path: "jobs", PollInterval: time.Hour}, log)
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := w.Snapshot()
	if got := fileNames(snap.Files); !reflect.DeepEqual(got, []string{"jobs/api.nomad.hcl"}) {
		t.Errorf("Files = %v", got)
	}
}

func TestFailedFetchKeepsSnapshot(t *testing.T) {
	r := newTestRepo(t, "main")
	commit := r.commit(map[string]string{"api.nomad.hcl": `job "api" {}`})

	log, buf := testLogger()
	w := New(Options{URL: r.url, Branch: "main", PollInterval: time.Hour}, log)
	ctx := context.Background()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// The remote becomes unreachable.
	if err := os.RemoveAll(r.bareDir); err != nil {
		t.Fatal(err)
	}
	w.poll(ctx)

	snap := w.Snapshot()
	if snap.Commit != commit || len(snap.Files) != 1 {
		t.Errorf("snapshot changed after a failed fetch: %+v", snap)
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("expected an ERROR log line, got: %s", buf)
	}
}

func TestStartGitPathMissing(t *testing.T) {
	r := newTestRepo(t, "main")
	r.commit(map[string]string{"api.nomad.hcl": `job "api" {}`})

	log, _ := testLogger()
	w := New(Options{URL: r.url, Branch: "main", Path: "no-such-dir", PollInterval: time.Hour}, log)
	if err := w.Start(context.Background()); err == nil {
		t.Fatal("expected an error for a missing git-path")
	}
}

func TestPollGitPathRemovedKeepsSnapshot(t *testing.T) {
	r := newTestRepo(t, "main")
	firstCommit := r.commit(map[string]string{"jobs/api.nomad.hcl": `job "api" {}`})

	log, buf := testLogger()
	w := New(Options{URL: r.url, Branch: "main", Path: "jobs", PollInterval: time.Hour}, log)
	ctx := context.Background()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := r.wt.Remove("jobs/api.nomad.hcl"); err != nil {
		t.Fatal(err)
	}
	r.commit(map[string]string{"unrelated.txt": "the jobs directory is now gone"})

	w.poll(ctx)
	snap := w.Snapshot()
	if snap.Commit != firstCommit || len(snap.Files) != 1 {
		t.Errorf("snapshot changed when git-path disappeared: %+v", snap)
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("expected an ERROR log line, got: %s", buf)
	}
}

func TestStartCloneFailure(t *testing.T) {
	r := newTestRepo(t, "main")
	r.commit(map[string]string{"api.nomad.hcl": `job "api" {}`})

	log, _ := testLogger()
	w := New(Options{URL: r.url, Branch: "does-not-exist", PollInterval: time.Hour}, log)
	if err := w.Start(context.Background()); err == nil {
		t.Fatal("expected an error for a nonexistent branch")
	}
}

func TestRemoteHeadBranchNotFound(t *testing.T) {
	r := newTestRepo(t, "main")
	r.commit(map[string]string{"api.nomad.hcl": `job "api" {}`})

	log, _ := testLogger()
	w := New(Options{URL: r.url, Branch: "no-such-branch"}, log)
	if _, err := w.remoteHead(context.Background()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want a \"not found\" error", err)
	}
}

func TestAuth(t *testing.T) {
	log, _ := testLogger()
	if got := New(Options{}, log).auth(); got != nil {
		t.Errorf("auth() = %v, want nil without a token", got)
	}
	w := New(Options{Username: "git", Token: "s3cr3t"}, log)
	auth, ok := w.auth().(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("auth() = %T, want *http.BasicAuth", w.auth())
	}
	if auth.Username != "git" || auth.Password != "s3cr3t" {
		t.Errorf("auth() = %+v", auth)
	}
}

func TestRun(t *testing.T) {
	r := newTestRepo(t, "main")
	r.commit(map[string]string{"api.nomad.hcl": `job "api" {}`})

	log, _ := testLogger()
	w := New(Options{URL: r.url, Branch: "main", PollInterval: 20 * time.Millisecond}, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// The ticker picks up a commit made after Run starts.
	second := r.commit(map[string]string{"api.nomad.hcl": `job "api" { v = 2 }`})
	select {
	case <-w.Changed():
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not pick up a new commit via its ticker")
	}
	if w.Snapshot().Commit != second {
		t.Errorf("Commit = %s, want %s", w.Snapshot().Commit, second)
	}

	// A Trigger also causes a poll, without waiting for the ticker.
	third := r.commit(map[string]string{"api.nomad.hcl": `job "api" { v = 3 }`})
	w.Trigger()
	select {
	case <-w.Changed():
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not pick up a triggered poll")
	}
	if w.Snapshot().Commit != third {
		t.Errorf("Commit = %s, want %s", w.Snapshot().Commit, third)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}
