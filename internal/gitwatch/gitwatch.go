// Package gitwatch keeps a repository of Nomad job files in memory,
// read-only (invariant 5 in docs/philosophy.md), and tells the engine which
// files exist at which commit.
//
// It does not parse HCL and does not decide whether a file is a managed job,
// a hook, or neither: that is the engine's job, through Nomad, since Nomad is
// the only HCL interpreter. See docs/design/gitwatch.md for the full design.
package gitwatch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// jobSuffixes are the recognised job file extensions, checked in this order
// so ".nomad.hcl" is not also matched as ".nomad".
var jobSuffixes = []string{".nomad.hcl", ".nomad"}

const varsSuffix = ".vars.hcl"

// Options configures a Watcher.
type Options struct {
	URL, Branch string
	// Path is the subdirectory holding the job files, relative to the
	// repository root. Empty means the root.
	Path string
	// Username and Token are HTTPS basic auth credentials. An empty Token
	// means a public repository; no other auth method is supported.
	Username, Token string
	// PollInterval is how often Run checks the remote for a new commit.
	PollInterval time.Duration
}

// File is one job file found in the repository.
type File struct {
	// Path is repository-relative, e.g. "apps/api.nomad.hcl".
	Path    string
	Content string
	// VarsPath and Vars are empty when there is no matching <name>.vars.hcl.
	VarsPath string
	Vars     string
}

// Snapshot is every job file at one commit.
type Snapshot struct {
	Commit string
	// Subject is the first line of the commit message, Author the author's
	// name (no email) and CommittedAt the commit time. They exist so a
	// deployment can say which change it came from without asking git again:
	// the clone is shallow and only ever holds the head.
	Subject     string
	Author      string
	CommittedAt time.Time
	Files       []File // sorted by Path
}

// Status is how the last polls went, for the dashboard: the last time the
// remote was reached and the last failure, if it is still the latest thing
// that happened.
type Status struct {
	// CheckedAt is the last time a poll (or the initial clone) reached the
	// remote and read its head, whether or not there was a new commit. Zero
	// before Start.
	CheckedAt time.Time
	// Error is the last poll's failure, or "" if the last poll succeeded.
	// ErrorAt is when it happened.
	Error   string
	ErrorAt time.Time
}

// Watcher clones and polls a git repository. Build it with New.
type Watcher struct {
	opts Options
	log  *slog.Logger

	mu     sync.RWMutex
	snap   Snapshot
	status Status
	now    func() time.Time

	trigger chan struct{}
	changed chan struct{}
}

// New builds a Watcher. Call Start before Run.
func New(o Options, log *slog.Logger) *Watcher {
	return &Watcher{
		opts:    o,
		log:     log,
		now:     time.Now,
		trigger: make(chan struct{}, 1),
		changed: make(chan struct{}, 1),
	}
}

// Start does the initial clone. An error is fatal: there is nothing to work
// on without a repository.
func (w *Watcher) Start(ctx context.Context) error {
	snap, err := w.fetch(ctx)
	if err != nil {
		return fmt.Errorf("gitwatch: initial clone: %w", err)
	}
	w.mu.Lock()
	w.snap = snap
	w.status = Status{CheckedAt: w.now()}
	w.mu.Unlock()
	w.log.InfoContext(ctx, "gitwatch: initial clone", "commit", snap.Commit, "files", len(snap.Files))
	return nil
}

// Run polls the remote on every tick or Trigger, until ctx is done. Call it
// after Start.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll(ctx)
		case <-w.trigger:
			w.poll(ctx)
		}
	}
}

// Trigger asks for a poll as soon as possible, for example from the git
// webhook. It never blocks: a burst of triggers coalesces into one poll.
func (w *Watcher) Trigger() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// Snapshot returns the last good snapshot.
func (w *Watcher) Snapshot() Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.snap
}

// Status returns how the last poll went.
func (w *Watcher) Status() Status {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

// Changed is signalled after a poll picks up a new commit. It has a buffer
// of 1: several changes before the consumer reads it still signal once.
func (w *Watcher) Changed() <-chan struct{} {
	return w.changed
}

// poll checks the remote branch head and re-clones only if it moved. A
// failure is logged at ERROR and keeps the last good snapshot: the caller
// retries at the next tick.
func (w *Watcher) poll(ctx context.Context) {
	head, err := w.remoteHead(ctx)
	if err != nil {
		w.log.ErrorContext(ctx, "gitwatch: list remote refs", "error", err)
		w.failed(err)
		return
	}
	if head == w.Snapshot().Commit {
		w.checked()
		return
	}
	snap, err := w.fetch(ctx)
	if err != nil {
		w.log.ErrorContext(ctx, "gitwatch: fetch", "error", err)
		w.failed(err)
		return
	}
	w.mu.Lock()
	w.snap = snap
	w.status = Status{CheckedAt: w.now()}
	w.mu.Unlock()
	w.log.InfoContext(ctx, "gitwatch: new commit", "commit", snap.Commit, "files", len(snap.Files))
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// checked records a poll that reached the remote and clears the last error.
func (w *Watcher) checked() {
	w.mu.Lock()
	w.status = Status{CheckedAt: w.now()}
	w.mu.Unlock()
}

// failed records a poll that did not, keeping CheckedAt as it was. The error
// text comes from go-git, which never puts the token in it (see auth).
func (w *Watcher) failed(err error) {
	w.mu.Lock()
	w.status.Error, w.status.ErrorAt = err.Error(), w.now()
	w.mu.Unlock()
}

// remoteHead returns the commit the configured branch points to on the
// remote, without cloning: a git ls-remote.
func (w *Watcher) remoteHead(ctx context.Context) (string, error) {
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{w.opts.URL},
	})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: w.auth()})
	if err != nil {
		return "", err
	}
	want := plumbing.NewBranchReferenceName(w.opts.Branch)
	for _, ref := range refs {
		if ref.Name() == want {
			return ref.Hash().String(), nil
		}
	}
	return "", fmt.Errorf("branch %q not found on the remote", w.opts.Branch)
}

// fetch does a fresh shallow, in-memory clone of the configured branch and
// reads the job files at its head commit. The clone is bare (no worktree),
// so nothing is ever written to disk and memory stays constant across calls.
func (w *Watcher) fetch(ctx context.Context) (Snapshot, error) {
	repo, err := git.CloneContext(ctx, memory.NewStorage(), nil, &git.CloneOptions{
		URL:           w.opts.URL,
		Auth:          w.auth(),
		ReferenceName: plumbing.NewBranchReferenceName(w.opts.Branch),
		SingleBranch:  true,
		Depth:         1,
		NoCheckout:    true,
		Tags:          git.NoTags,
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("clone: %w", err)
	}
	head, err := repo.Head()
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return Snapshot{}, fmt.Errorf("read commit %s: %w", head.Hash(), err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return Snapshot{}, fmt.Errorf("read tree of commit %s: %w", head.Hash(), err)
	}
	if w.opts.Path != "" {
		tree, err = tree.Tree(w.opts.Path)
		if err != nil {
			return Snapshot{}, fmt.Errorf("git-path %q: %w", w.opts.Path, err)
		}
	}
	files, err := w.collect(tree)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Commit:      head.Hash().String(),
		Subject:     subject(commit.Message),
		Author:      commit.Author.Name,
		CommittedAt: commit.Committer.When,
		Files:       files,
	}, nil
}

// subject returns the first line of a commit message.
func subject(msg string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return strings.TrimSpace(first)
}

// CommitURL returns the web address of a commit of the repository at
// repoURL, in the form GitHub, Gitea and Forgejo share
// (<repo>/commit/<sha>; GitLab redirects it). It returns "" when repoURL is
// not an http(s) URL, so the dashboard shows the SHA without a link rather
// than a broken one. Credentials embedded in repoURL are dropped, never
// linked.
func CommitURL(repoURL, sha string) string {
	u, err := url.Parse(repoURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || sha == "" {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git") + "/commit/" + sha
	return u.String()
}

// auth returns the HTTPS basic auth credentials, or nil for a public
// repository. The token never becomes part of a URL, so it never reaches a
// log line or an error.
func (w *Watcher) auth() transport.AuthMethod {
	if w.opts.Token == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: w.opts.Username, Password: w.opts.Token}
}

// varsEntry is a *.vars.hcl file found while walking the tree, indexed by
// its job's key (its directory and name, without the job extension).
type varsEntry struct {
	path, content string
}

// collect walks tree recursively and pairs every job file with its vars
// file, if any. tree is rooted at Options.Path, so paths are joined back to
// the repository root. A *.vars.hcl with no matching job file is logged at
// WARN and dropped.
func (w *Watcher) collect(tree *object.Tree) ([]File, error) {
	var jobs []*File
	keys := make(map[*File]string)
	vars := make(map[string]varsEntry)

	iter := tree.Files()
	defer iter.Close()
	for {
		f, err := iter.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("walk tree: %w", err)
		}

		dir, base := path.Split(f.Name)
		switch {
		case strings.HasSuffix(base, varsSuffix):
			content, err := f.Contents()
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", w.repoPath(f.Name), err)
			}
			key := dir + strings.TrimSuffix(base, varsSuffix)
			vars[key] = varsEntry{path: w.repoPath(f.Name), content: content}

		default:
			name, ok := jobName(base)
			if !ok {
				continue
			}
			content, err := f.Contents()
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", w.repoPath(f.Name), err)
			}
			jf := &File{Path: w.repoPath(f.Name), Content: content}
			jobs = append(jobs, jf)
			keys[jf] = dir + name
		}
	}

	used := make(map[string]bool)
	for _, jf := range jobs {
		if v, ok := vars[keys[jf]]; ok {
			jf.VarsPath, jf.Vars = v.path, v.content
			used[keys[jf]] = true
		}
	}
	for key, v := range vars {
		if !used[key] {
			w.log.Warn("gitwatch: vars file with no matching job", "path", v.path)
		}
	}

	files := make([]File, len(jobs))
	for i, jf := range jobs {
		files[i] = *jf
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// jobName reports whether base is a job file name and returns its name
// without the job extension.
func jobName(base string) (name string, ok bool) {
	for _, suffix := range jobSuffixes {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(base, suffix), true
		}
	}
	return "", false
}

// repoPath joins a path relative to the configured Path back to a
// repository-relative one.
func (w *Watcher) repoPath(name string) string {
	if w.opts.Path == "" {
		return name
	}
	return path.Join(w.opts.Path, name)
}
