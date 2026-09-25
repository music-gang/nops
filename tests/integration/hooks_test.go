//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/nomadx"
	"github.com/music-gang/nops/internal/store"
)

// hookCmdHCL is a parameterized batch hook running `sh -c cmd`. It declares the
// dispatch meta a real hook would (one required, two optional).
// hookWithTimeoutHCL is a hook that sets its own nops_timeout.
func hookWithTimeoutHCL(id, cmd, timeout string) string {
	return strings.Replace(hookCmdHCL(id, cmd, true), `nops_role = "hook"`,
		`nops_role = "hook"`+"\n    "+`nops_timeout = "`+timeout+`"`, 1)
}

func hookCmdHCL(id, cmd string, isHook bool) string {
	role := ""
	if isHook {
		role = `nops_role = "hook"`
	}
	return fmt.Sprintf(`
job %q {
  type = "batch"
  meta {
    %s
  }
  parameterized {
    meta_required = ["nops_deployment_id"]
    meta_optional = ["nops_phase", "nops_image_api"]
  }
  group "g" {
    restart {
      attempts = 0
      mode     = "fail"
    }
    reschedule {
      attempts  = 0
      unlimited = false
    }
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", %q]
      }
    }
  }
}`, id, role, cmd)
}

type hookEnv struct {
	nomad  *nomadx.Client
	raw    *api.Client
	store  *store.Store
	runner *hooks.Runner
	depID  string
	hookID string
}

// newHookEnv registers a hook job running cmd and prepares a runner with a real
// store and a real deployment row.
func newHookEnv(t *testing.T, cmd string, isHook bool) *hookEnv {
	t.Helper()
	c, raw := newClient(t)
	ctx := context.Background()
	hookID := uniqueID(t, raw, "hookrun")

	job, err := c.ParseHCL(ctx, hookCmdHCL(hookID, cmd, isHook), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, job, 0, false); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "nops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &store.Deployment{JobID: "api", Namespace: "default", CommitSHA: "abc123", SpecHash: "h", JobSpec: "{}", Policy: store.PolicyAuto}
	if err := st.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}

	return &hookEnv{
		nomad:  c,
		raw:    raw,
		store:  st,
		runner: hooks.New(c, st, slog.New(slog.NewTextHandler(io.Discard, nil)), 200*time.Millisecond),
		depID:  d.ID,
		hookID: hookID,
	}
}

// dockerTarget is the job being deployed: one docker task, never registered.
func dockerTarget() *api.Job {
	id, group := "api", "web"
	return &api.Job{ID: &id, TaskGroups: []*api.TaskGroup{{
		Name:  &group,
		Tasks: []*api.Task{{Name: "api", Driver: "docker", Config: map[string]any{"image": "registry.example/api:2"}}},
	}}}
}

func (e *hookEnv) request(timeout time.Duration) hooks.Request {
	return hooks.Request{
		DeploymentID: e.depID,
		Namespace:    "default",
		Phase:        "pre",
		HookJobID:    e.hookID,
		Commit:       "abc123",
		Timeout:      timeout,
		Target:       dockerTarget(),
	}
}

func (e *hookEnv) run(t *testing.T, timeout time.Duration) hooks.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := e.runner.Run(ctx, e.request(timeout))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func (e *hookEnv) children(t *testing.T) []string {
	t.Helper()
	stubs, _, err := e.raw.Jobs().PrefixList(e.hookID + "/dispatch-")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range stubs {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestHookRunSucceedsAndRerunDoesNotDispatch(t *testing.T) {
	e := newHookEnv(t, "true", true)

	res := e.run(t, time.Minute)
	if res.State != store.HookSucceeded || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.HasPrefix(res.DispatchedJobID, e.hookID+"/dispatch-") {
		t.Errorf("dispatched job = %q", res.DispatchedJobID)
	}

	// The assumption the outcome logic rests on: a batch allocation that ended
	// on its own is complete and still desired to run.
	allocs, err := e.nomad.Allocations(context.Background(), "default", res.DispatchedJobID)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("allocations = %+v, %v", allocs, err)
	}
	if allocs[0].ClientStatus != "complete" || allocs[0].DesiredStatus != "run" {
		t.Errorf("finished batch allocation = %+v, want complete/run", allocs[0])
	}

	// Running it again (a restart of nops) returns the stored result and does
	// not create another child.
	again := e.run(t, time.Minute)
	if again != res {
		t.Errorf("second run = %+v, want %+v", again, res)
	}
	if kids := e.children(t); len(kids) != 1 {
		t.Errorf("children = %v, want exactly one", kids)
	}
	if run, err := e.store.GetHookRun(context.Background(), e.depID, "pre", 0); err != nil || run.State != store.HookSucceeded {
		t.Errorf("stored run = %+v, %v", run, err)
	}
}

func TestHookRunFails(t *testing.T) {
	e := newHookEnv(t, "echo migration broke >&2; exit 3", true)

	res := e.run(t, time.Minute)
	if res.State != store.HookFailed {
		t.Fatalf("result = %+v", res)
	}
	t.Logf("failure message: %s", res.Error)
	if !strings.Contains(res.Error, "Exit Code: 3") {
		t.Errorf("error %q does not carry the exit code", res.Error)
	}
}

func TestHookRunTimesOutAndStopsTheChild(t *testing.T) {
	e := newHookEnv(t, "sleep 600", true)

	start := time.Now()
	res := e.run(t, 3*time.Second)
	if res.State != store.HookTimedOut {
		t.Fatalf("result = %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %s for a 3s timeout", elapsed)
	}

	child, err := e.nomad.Job(context.Background(), "default", res.DispatchedJobID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Stop == nil || !*child.Stop {
		t.Errorf("child not stopped: stop=%v status=%v", child.Stop, child.Status)
	}
	// Once the client has killed the task, a stopped allocation reads
	// complete/stop: the runner relies on that to tell "we stopped it" from a
	// failure when it retries after a failed save.
	deadline := time.Now().Add(30 * time.Second)
	for {
		allocs, err := e.nomad.Allocations(context.Background(), "default", res.DispatchedJobID)
		if err != nil || len(allocs) == 0 {
			t.Fatalf("allocations = %+v, %v", allocs, err)
		}
		a := allocs[0]
		if a.DesiredStatus != "stop" {
			t.Fatalf("alloc %s desired status = %q, want stop", a.ID, a.DesiredStatus)
		}
		if a.ClientStatus == "complete" {
			break
		}
		if a.ClientStatus != "running" && a.ClientStatus != "pending" || time.Now().After(deadline) {
			t.Fatalf("stopped alloc client status = %q, want complete", a.ClientStatus)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Stopping is idempotent, also for a job that does not exist.
	if err := e.nomad.StopJob(context.Background(), "default", res.DispatchedJobID); err != nil {
		t.Errorf("second stop: %v", err)
	}
	if err := e.nomad.StopJob(context.Background(), "default", "nops-it-no-such-job"); err != nil {
		t.Errorf("stop of a missing job: %v", err)
	}
}

// markDispatchAttempted leaves the store as a crash right after the dispatch
// was accepted would: the run is running and the child ID was never saved.
func (e *hookEnv) markDispatchAttempted(t *testing.T) *store.HookRun {
	t.Helper()
	ctx := context.Background()
	run, _, err := e.store.EnsureHookRun(ctx, e.depID, "pre", 0, e.hookID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.UpdateHookRun(ctx, run.ID, store.HookUpdate{State: store.HookRunning}); err != nil {
		t.Fatal(err)
	}
	return run
}

// A crash after Nomad accepted the dispatch but before the child ID was saved:
// the run finds the child by its idempotency token (which Nomad records on the
// child job) and adopts it instead of dispatching again.
func TestHookRunAdoptsChildAfterCrashBeforeSavingItsID(t *testing.T) {
	e := newHookEnv(t, "true", true)
	ctx := context.Background()
	run := e.markDispatchAttempted(t)
	first, err := e.nomad.Dispatch(ctx, "default", e.hookID, map[string]string{"nops_deployment_id": e.depID}, run.IdempotencyToken)
	if err != nil {
		t.Fatal(err)
	}

	found, err := e.nomad.FindDispatched(ctx, "default", e.hookID, run.IdempotencyToken)
	if err != nil || found != first.JobID {
		t.Fatalf("FindDispatched = %q, %v, want %s", found, err, first.JobID)
	}
	if other, err := e.nomad.FindDispatched(ctx, "default", e.hookID, "someone-else:pre"); err != nil || other != "" {
		t.Errorf("FindDispatched with another token = %q, %v, want empty", other, err)
	}

	res := e.run(t, time.Minute)
	if res.State != store.HookSucceeded || res.DispatchedJobID != first.JobID {
		t.Fatalf("result = %+v, want success on the first child %s", res, first.JobID)
	}
	if kids := e.children(t); len(kids) != 1 {
		t.Errorf("children = %v, want exactly one", kids)
	}
}

// The run is marked as dispatched but Nomad has no child with its token (never
// created, or garbage-collected): the hook may have run, so it is failed and
// nothing is dispatched.
func TestHookRunFailsWhenNoChildMatchesAMarkedRun(t *testing.T) {
	e := newHookEnv(t, "true", true)
	e.markDispatchAttempted(t)

	res := e.run(t, time.Minute)
	if res.State != store.HookFailed || !strings.Contains(res.Error, "outcome is unknown") {
		t.Fatalf("result = %+v", res)
	}
	if kids := e.children(t); len(kids) != 0 {
		t.Errorf("a marked run was dispatched again: %v", kids)
	}
}

func TestHookRunRejectsAJobThatIsNotAHook(t *testing.T) {
	e := newHookEnv(t, "true", false)

	res := e.run(t, time.Minute)
	if res.State != store.HookFailed || !strings.Contains(res.Error, `nops_role = "hook"`) {
		t.Fatalf("result = %+v", res)
	}
	if kids := e.children(t); len(kids) != 0 {
		t.Errorf("a job that is not a hook was dispatched: %v", kids)
	}
}

// The docker image of the new version reaches the hook as nops_image_<task>,
// and only because the hook declared it.
func TestHookRunPassesDeclaredMeta(t *testing.T) {
	e := newHookEnv(t, `test "$NOMAD_META_nops_image_api" = "registry.example/api:2" && test "$NOMAD_META_nops_phase" = "pre" && test -n "$NOMAD_META_nops_deployment_id"`, true)

	res := e.run(t, time.Minute)
	if res.State != store.HookSucceeded {
		t.Fatalf("result = %+v (the hook checks its own meta)", res)
	}
}
