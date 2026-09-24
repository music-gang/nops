//go:build integration

package integration

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"golang.org/x/crypto/bcrypt"
)

// TestNopsBinaryStartsAndShutsDown is the wiring task's smoke test: build the
// real nops binary, run it against a real Nomad and a scratch git
// repository (basic auth, so no OIDC provider is needed here), confirm the
// dashboard answers, then send SIGTERM and confirm it exits cleanly. It does
// not exercise detection or apply: those are covered end to end by
// engine_test.go against the engine package directly.
func TestNopsBinaryStartsAndShutsDown(t *testing.T) {
	addr := testAddr(t)
	bin := buildNopsBinary(t)

	repoURL := newScratchRepo(t)
	usersFile := writeUsersFile(t, "alice", "s3cret")
	dbPath := filepath.Join(t.TempDir(), "nops.db")
	port := freePort(t)
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port)

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
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nops: %v", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			cmd.Process.Kill()
		}
	})

	healthzURL := "http://" + listenAddr + "/healthz"
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(healthzURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case err := <-exited:
			t.Fatalf("nops exited early: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if lastErr != nil {
		t.Fatalf("GET /healthz never succeeded: %v", lastErr)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal nops: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("nops exited with an error after SIGTERM: %v", err)
		}
	case <-time.After(shutdownWaitTimeout):
		t.Fatal("nops did not exit within the shutdown grace period")
	}
}

// shutdownWaitTimeout is generous next to cmd/nops's own 10s
// http.Server.Shutdown budget: the three background loops unwind almost
// immediately (they select on ctx.Done()), so the whole process is expected
// to exit well under this.
const shutdownWaitTimeout = 15 * time.Second

// buildNopsBinary builds ./cmd/nops once into a temp directory and returns
// its path.
func buildNopsBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "nops")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/nops")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/nops: %v\n%s", err, out)
	}
	return bin
}

// writeUsersFile writes a one-user "username:bcrypt-hash" file
// (internal/web/auth_basic.go's format) and returns its path. MinCost: it
// only needs to be a valid bcrypt hash, not a secure one, for this test.
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

// newScratchRepo creates a local bare repository with one commit (an empty
// job directory is enough: this test only needs gitwatch's initial clone to
// succeed, not any managed job) and returns its file:// URL. Mirrors
// internal/gitwatch's own test helper; not reusable across packages since
// that one is unexported in a _test.go file.
func newScratchRepo(t *testing.T) string {
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
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	readme := filepath.Join(workdir, "README.md")
	if err := os.WriteFile(readme, []byte("scratch repo for the wiring smoke test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("add README.md: %v", err)
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0)}
	if _, err := wt.Commit("initial commit", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := repo.Push(&git.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	return url
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
