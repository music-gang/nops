//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hashicorp/nomad/api"
	"golang.org/x/crypto/bcrypt"

	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// The end-to-end tests drive the real nops binary the way a person would: a
// git repository they push to, the dashboard over HTTP, and a real Nomad. They
// stand in for what used to be manual acceptance checklists, with
// lightweight raw_exec jobs instead of heavy images and real volumes.

const (
	e2eUser     = "alice"
	e2ePassword = "s3cret"

	// e2eWait bounds every "wait until nops gets there" step. Intervals are
	// 200ms, so a healthy step takes about a second; this only matters when
	// something is broken.
	e2eWait = 30 * time.Second
)

// TestMain drops the NOMAD_* variables of the environment and removes the nops
// binary built for the run (see buildNopsBinary). The variables go first so a
// shell set up for a real cluster (NOMAD_ADDR, NOMAD_TOKEN, ...) is never
// used by a test, nor its token sent anywhere, whether the test talks to
// Nomad itself or through the nops process it starts.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "NOMAD_") {
			os.Unsetenv(name)
		}
	}
	code := m.Run()
	if nopsBinDir != "" {
		os.RemoveAll(nopsBinDir)
	}
	os.Exit(code)
}

var (
	nopsBinOnce sync.Once
	nopsBinDir  string
	nopsBinPath string
	nopsBinErr  error
)

// buildNopsBinary builds ./cmd/nops once per test run and returns its path.
func buildNopsBinary(t *testing.T) string {
	t.Helper()
	nopsBinOnce.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			nopsBinErr = err
			return
		}
		nopsBinDir, err = os.MkdirTemp("", "nops-bin-")
		if err != nil {
			nopsBinErr = err
			return
		}
		nopsBinPath = filepath.Join(nopsBinDir, "nops")
		cmd := exec.Command("go", "build", "-o", nopsBinPath, "./cmd/nops")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			nopsBinErr = fmt.Errorf("go build ./cmd/nops: %v\n%s", err, out)
		}
	})
	if nopsBinErr != nil {
		t.Fatal(nopsBinErr)
	}
	return nopsBinPath
}

// -- nops process ---------------------------------------------------------

// nopsProc is a running nops binary.
type nopsProc struct {
	baseURL string
	dbPath  string
	cmd     *exec.Cmd
	exited  chan error
}

// startNops runs the nops binary against the Nomad under test and the git
// repository at repoURL, with basic auth (e2eUser) and short intervals so a
// test does not wait for the production defaults. env entries ("KEY=value")
// are added last and win. It waits for /healthz and kills the process at
// cleanup if the test did not stop it.
func startNops(t *testing.T, repoURL string, env ...string) *nopsProc {
	t.Helper()
	addr := testAddr(t)
	bin := buildNopsBinary(t)

	usersFile := writeUsersFile(t, e2eUser, e2ePassword)
	dbPath := filepath.Join(t.TempDir(), "nops.db")
	listenAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"NOPS_NOMAD_ADDR="+addr,
		"NOPS_NOMAD_NAMESPACE=default",
		"NOPS_GIT_URL="+repoURL,
		"NOPS_GIT_BRANCH=main",
		"NOPS_DB_PATH="+dbPath,
		"NOPS_LISTEN_ADDR="+listenAddr,
		"NOPS_AUTH_MODE=basic",
		"NOPS_USERS_FILE="+usersFile,
		"NOPS_PUBLIC_URL=http://"+listenAddr,
		"NOPS_GIT_POLL_INTERVAL=200ms",
		"NOPS_DRIFT_INTERVAL=200ms",
		"NOPS_ENGINE_INTERVAL=200ms",
		"NOPS_HOOK_POLL_INTERVAL=200ms",
		"NOPS_APPLY_TIMEOUT=30s",
	)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nops: %v", err)
	}

	p := &nopsProc{baseURL: "http://" + listenAddr, dbPath: dbPath, cmd: cmd, exited: make(chan error, 1)}
	go func() { p.exited <- cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			cmd.Process.Kill()
		}
	})

	var lastErr error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(p.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case err := <-p.exited:
			t.Fatalf("nops exited early: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("GET /healthz never succeeded: %v", lastErr)
	return nil
}

// shutdownWaitTimeout is generous next to cmd/nops's own 10s
// http.Server.Shutdown budget: the background loops unwind almost
// immediately (they select on ctx.Done()), so the whole process is expected
// to exit well under this.
const shutdownWaitTimeout = 15 * time.Second

// stop sends SIGTERM and fails the test unless nops exits cleanly in time.
func (p *nopsProc) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal nops: %v", err)
	}
	select {
	case err := <-p.exited:
		if err != nil {
			t.Errorf("nops exited with an error after SIGTERM: %v", err)
		}
	case <-time.After(shutdownWaitTimeout):
		t.Fatal("nops did not exit within the shutdown grace period")
	}
}

// writeUsersFile writes a one-user "username:bcrypt-hash" file
// (internal/web/auth_basic.go's format) and returns its path. MinCost: it
// only needs to be a valid bcrypt hash, not a secure one, for these tests.
func writeUsersFile(t *testing.T, username, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "users")
	if err := os.WriteFile(path, fmt.Appendf(nil, "%s:%s\n", username, hash), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// freePort asks the OS for a free TCP port by binding to :0 and releasing
// it immediately. Small TOCTOU race, acceptable for a test.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// -- git repository -------------------------------------------------------

// scratchRepo is a local bare repository (reached over file://) with a
// working copy the test commits to, standing in for the forge a person pushes
// to.
type scratchRepo struct {
	url  string
	dir  string
	repo *git.Repository
}

// newScratchRepo creates the repository with one commit (a README, so
// gitwatch's initial clone has something to clone) and returns it.
func newScratchRepo(t *testing.T) *scratchRepo {
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
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("init work repo: %v", err)
	}
	url := "file://" + bareDir
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	r := &scratchRepo{url: url, dir: workdir, repo: repo}
	r.commit(t, "initial commit", map[string]string{"README.md": "scratch repo for the integration tests\n"})
	return r
}

// commit writes files (repository-relative path → content), commits them and
// pushes, like a person pushing a change.
func (r *scratchRepo) commit(t *testing.T, msg string, files map[string]string) {
	t.Helper()
	wt, err := r.repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for path, content := range files {
		full := filepath.Join(r.dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(path); err != nil {
			t.Fatalf("add %s: %v", path, err)
		}
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	if _, err := wt.Commit(msg, &git.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := r.repo.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("push: %v", err)
	}
}

// -- dashboard client -----------------------------------------------------

// dashboard is an HTTP client for the nops dashboard: a cookie jar for the
// session, and no redirect following so a test sees the status the handler
// answered with.
type dashboard struct {
	base string
	c    *http.Client
}

func newDashboard(t *testing.T, base string) *dashboard {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &dashboard{base: base, c: &http.Client{
		Jar:           jar,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (d *dashboard) do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := d.c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", req.Method, req.URL.Path, err)
	}
	return resp.StatusCode, string(body)
}

func (d *dashboard) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, d.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d.do(t, req)
}

func (d *dashboard) post(t *testing.T, path string, form url.Values) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, d.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return d.do(t, req)
}

// login starts a session as user and checks the dashboard now answers.
func (d *dashboard) login(t *testing.T, user, password string) {
	t.Helper()
	status, _ := d.post(t, "/auth/login", url.Values{"username": {user}, "password": {password}})
	if status != http.StatusFound {
		t.Fatalf("login as %s: status %d, want 302", user, status)
	}
	if status, _ := d.get(t, "/history"); status != http.StatusOK {
		t.Fatalf("GET /history after login: status %d, want 200", status)
	}
}

// approve and reject are the dashboard buttons. They return the HTTP status:
// 303 when the decision was taken, 409 for a stale spec_hash.
func (d *dashboard) approve(t *testing.T, id, specHash string) int {
	t.Helper()
	status, _ := d.post(t, "/deployments/"+id+"/approve", url.Values{"spec_hash": {specHash}})
	return status
}

func (d *dashboard) reject(t *testing.T, id string) int {
	t.Helper()
	status, _ := d.post(t, "/deployments/"+id+"/reject", nil)
	return status
}

// -- end-to-end environment -----------------------------------------------

// e2eEnv is a running nops wired to a scratch repository and a real Nomad,
// with a logged-in dashboard client and read access to nops's database.
type e2eEnv struct {
	t     *testing.T
	nomad *nomadx.Client
	raw   *api.Client
	repo  *scratchRepo
	proc  *nopsProc
	st    *store.Store
	dash  *dashboard
}

func newE2E(t *testing.T, env ...string) *e2eEnv {
	t.Helper()
	c, raw := newClient(t)
	repo := newScratchRepo(t)
	proc := startNops(t, repo.url, env...)

	// Read-only use of nops's own database, alongside the running process (WAL).
	st, err := store.Open(proc.dbPath)
	if err != nil {
		t.Fatalf("open nops database: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	dash := newDashboard(t, proc.baseURL)
	dash.login(t, e2eUser, e2ePassword)
	return &e2eEnv{t: t, nomad: c, raw: raw, repo: repo, proc: proc, st: st, dash: dash}
}

// latest returns the newest deployment of jobID, or nil if it has none yet.
func (e *e2eEnv) latest(jobID string) *store.Deployment {
	e.t.Helper()
	d, err := e.st.LatestDeployment(context.Background(), "default", jobID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		e.t.Fatalf("LatestDeployment(%s): %v", jobID, err)
	}
	return d
}

func (e *e2eEnv) deployment(id string) *store.Deployment {
	e.t.Helper()
	d, err := e.st.GetDeployment(context.Background(), id)
	if err != nil {
		e.t.Fatalf("GetDeployment(%s): %v", id, err)
	}
	return d
}

// waitNew waits for a deployment of jobID newer than prevID (empty: any) that
// is in state want, and returns it.
func (e *e2eEnv) waitNew(jobID, prevID string, want store.State) *store.Deployment {
	e.t.Helper()
	var last *store.Deployment
	deadline := time.Now().Add(e2eWait)
	for time.Now().Before(deadline) {
		last = e.latest(jobID)
		if last != nil && last.ID != prevID && last.State == want {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("no new %s deployment for %s within %s (after %q); latest: %s", want, jobID, e2eWait, prevID, describe(last))
	return nil
}

// waitState waits for the deployment id to reach want. Terminal states are
// final, so reaching a different one fails at once with its error.
func (e *e2eEnv) waitState(id string, want store.State) *store.Deployment {
	e.t.Helper()
	var last *store.Deployment
	deadline := time.Now().Add(e2eWait)
	for time.Now().Before(deadline) {
		last = e.deployment(id)
		if last.State == want {
			return last
		}
		if !last.State.IsActive() {
			e.t.Fatalf("deployment %s ended %s, want %s: %s", id, last.State, want, describe(last))
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("deployment %s did not reach %s within %s; now: %s", id, want, e2eWait, describe(last))
	return nil
}

func describe(d *store.Deployment) string {
	if d == nil {
		return "none"
	}
	return fmt.Sprintf("%s state=%s error=%q", d.ID, d.State, d.Error)
}

// states returns the states a deployment went through, in order, from its
// audit log.
func (e *e2eEnv) states(id string) []store.State {
	e.t.Helper()
	events, err := e.st.Events(context.Background(), id)
	if err != nil {
		e.t.Fatalf("Events(%s): %v", id, err)
	}
	var out []store.State
	for _, ev := range events {
		out = append(out, ev.To)
	}
	return out
}

// deploymentsOf counts every deployment nops ever created for jobID.
func (e *e2eEnv) deploymentsOf(jobID string) int {
	e.t.Helper()
	active, err := e.st.ListActive(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	history, err := e.st.ListHistory(context.Background(), 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, d := range append(active, history...) {
		if d.JobID == jobID {
			n++
		}
	}
	return n
}

// liveIndex is the JobModifyIndex of jobID in Nomad, 0 if it is not registered.
func (e *e2eEnv) liveIndex(jobID string) uint64 {
	e.t.Helper()
	job, err := e.nomad.Job(context.Background(), jobID)
	if errors.Is(err, nomadx.ErrJobNotFound) {
		return 0
	}
	if err != nil {
		e.t.Fatalf("Job(%s): %v", jobID, err)
	}
	return *job.JobModifyIndex
}

// liveVersion is the "version" meta of jobID in Nomad ("" if not registered),
// the field the e2e jobs change between commits.
func (e *e2eEnv) liveVersion(jobID string) string {
	e.t.Helper()
	job, err := e.nomad.Job(context.Background(), jobID)
	if errors.Is(err, nomadx.ErrJobNotFound) {
		return ""
	}
	if err != nil {
		e.t.Fatalf("Job(%s): %v", jobID, err)
	}
	return job.Meta["version"]
}

// editOutsideNops registers jobHCL straight into Nomad, the way someone with
// a terminal would, bypassing git and nops.
func (e *e2eEnv) editOutsideNops(jobID, jobHCL string) {
	e.t.Helper()
	ctx := context.Background()
	job, err := e.nomad.ParseHCL(ctx, jobHCL, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.nomad.RegisterCAS(ctx, job, e.liveIndex(jobID), false); err != nil {
		e.t.Fatalf("outside edit of %s: %v", jobID, err)
	}
}

// -- job HCL --------------------------------------------------------------

// e2eJob is a managed raw_exec batch job. version is a job meta key: changing
// it between commits is the "change" nops has to deploy.
type e2eJob struct {
	id, policy, version string
	preHook, postHook   string
	preTimeout          string
	// cmd is the target's shell command; empty means "exit 0".
	cmd string
}

func (j e2eJob) hcl() string {
	cmd := j.cmd
	if cmd == "" {
		cmd = "exit 0"
	}
	var meta strings.Builder
	fmt.Fprintf(&meta, "    nops_managed = %q\n    nops_policy  = %q\n    version      = %q\n", "true", j.policy, j.version)
	if j.preHook != "" {
		fmt.Fprintf(&meta, "    nops_pre_hook = %q\n", j.preHook)
	}
	if j.preTimeout != "" {
		fmt.Fprintf(&meta, "    nops_pre_hook_timeout = %q\n", j.preTimeout)
	}
	if j.postHook != "" {
		fmt.Fprintf(&meta, "    nops_post_hook = %q\n", j.postHook)
	}
	return fmt.Sprintf(`
job %q {
  type = "batch"
  meta {
%s  }
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", %q]
      }
    }
  }
}
`, j.id, meta.String(), cmd)
}

// file is the repository path of a job or hook.
func file(id string) string { return id + ".nomad.hcl" }
