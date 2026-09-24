package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// newTestStore opens a store on a temp file with a controllable clock that
// advances one second per call.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "nops.db"))
}

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	var mu sync.Mutex
	now := t0
	s, err := Open(path, WithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Second)
		return now
	}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newDep(job string) *Deployment {
	return &Deployment{
		JobID: job, Namespace: "default", CommitSHA: "abc1234", SpecHash: "hash-" + job,
		JobSpec: `{"ID":"` + job + `"}`, PlanDiff: `{"Type":"Edited"}`, Policy: PolicyApproval, CASIndex: 42,
	}
}

func mustCreate(t *testing.T, s *Store, d *Deployment) *Deployment {
	t.Helper()
	if err := s.CreateDeployment(context.Background(), d); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	return d
}

func TestCreateAndGet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))

	if d.ID == "" || d.State != StateDetected || d.CreatedAt.IsZero() {
		t.Fatalf("store did not fill ID/State/CreatedAt: %+v", d)
	}
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.JobID != "web" || got.CASIndex != 42 || got.PlanDiff != `{"Type":"Edited"}` ||
		got.Policy != PolicyApproval || got.State != StateDetected || !got.CreatedAt.Equal(d.CreatedAt) {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if !got.DecidedAt.IsZero() || got.DecidedBy != "" || got.AppliedIndex != 0 || got.Error != "" {
		t.Errorf("optional fields should be zero: %+v", got)
	}

	evs, err := s.Events(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].From != "" || evs[0].To != StateDetected || evs[0].Actor != "nops" {
		t.Errorf("creation event = %+v", evs)
	}

	if _, err := s.GetDeployment(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetDeployment(missing) err = %v, want ErrNotFound", err)
	}
}

func TestCreateValidation(t *testing.T) {
	s := newTestStore(t)
	for name, mod := range map[string]func(*Deployment){
		"no job":     func(d *Deployment) { d.JobID = "" },
		"no ns":      func(d *Deployment) { d.Namespace = "" },
		"no hash":    func(d *Deployment) { d.SpecHash = "" },
		"no spec":    func(d *Deployment) { d.JobSpec = "" },
		"bad policy": func(d *Deployment) { d.Policy = "none" },
	} {
		d := newDep("web")
		mod(d)
		if err := s.CreateDeployment(context.Background(), d); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// The per-job lock is enforced by the database, not by in-memory state.
func TestPerJobLock(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	first := mustCreate(t, s, newDep("web"))

	if err := s.CreateDeployment(ctx, newDep("web")); !errors.Is(err, ErrActiveDeployment) {
		t.Fatalf("second active deployment: err = %v, want ErrActiveDeployment", err)
	}
	// Different job and different namespace are independent.
	mustCreate(t, s, newDep("db"))
	other := newDep("web")
	other.Namespace = "staging"
	mustCreate(t, s, other)

	// Every active state holds the lock; a terminal state releases it.
	path := []State{StatePendingApproval, StatePreHook, StateApplying, StatePostHook}
	from := StateDetected
	for _, to := range path {
		if err := s.Transition(ctx, first.ID, to, Transition{From: from, Actor: "nops"}); err != nil {
			t.Fatalf("%s->%s: %v", from, to, err)
		}
		if err := s.CreateDeployment(ctx, newDep("web")); !errors.Is(err, ErrActiveDeployment) {
			t.Fatalf("lock not held in %s: err = %v", to, err)
		}
		from = to
	}
	if err := s.Transition(ctx, first.ID, StateCompleted, Transition{From: from, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, newDep("web"))
}

func TestActiveDeployment(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.ActiveDeployment(ctx, "default", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	d := mustCreate(t, s, newDep("web"))
	got, err := s.ActiveDeployment(ctx, "default", "web")
	if err != nil || got.ID != d.ID {
		t.Fatalf("ActiveDeployment = %+v, %v", got, err)
	}
}

func TestTransitionRecordsEventAndFields(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))

	if err := s.Transition(ctx, d.ID, StatePendingApproval, Transition{From: StateDetected, Actor: "nops", Message: "needs approval"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, d.ID, StateApplying, Transition{From: StatePendingApproval, Actor: "alice", DecidedBy: "alice", Message: "approved"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, d.ID, StateCompleted, Transition{From: StateApplying, Actor: "nops", AppliedIndex: 99, EvalID: "eval-1"}); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetDeployment(ctx, d.ID)
	if got.State != StateCompleted || got.DecidedBy != "alice" || got.DecidedAt.IsZero() ||
		got.AppliedIndex != 99 || got.EvalID != "eval-1" {
		t.Errorf("deployment = %+v", got)
	}
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Errorf("updated_at %v not after created_at %v", got.UpdatedAt, got.CreatedAt)
	}

	evs, _ := s.Events(ctx, d.ID)
	want := []struct {
		from, to State
		actor    string
	}{
		{"", StateDetected, "nops"},
		{StateDetected, StatePendingApproval, "nops"},
		{StatePendingApproval, StateApplying, "alice"},
		{StateApplying, StateCompleted, "nops"},
	}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(evs), len(want), evs)
	}
	for i, w := range want {
		if evs[i].From != w.from || evs[i].To != w.to || evs[i].Actor != w.actor {
			t.Errorf("event %d = %+v, want %+v", i, evs[i], w)
		}
	}
	if evs[2].Message != "approved" {
		t.Errorf("event message = %q", evs[2].Message)
	}
}

func TestTransitionFailureKeepsError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))
	if err := s.Transition(ctx, d.ID, StateApplying, Transition{From: StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, d.ID, StateFailed, Transition{From: StateApplying, Actor: "nops", Error: "register: CAS conflict"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDeployment(ctx, d.ID)
	if got.State != StateFailed || got.Error != "register: CAS conflict" {
		t.Errorf("deployment = %+v", got)
	}
	evs, _ := s.Events(ctx, d.ID)
	if last := evs[len(evs)-1]; last.Message != "register: CAS conflict" {
		t.Errorf("event message should default to the error, got %q", last.Message)
	}
}

func TestTransitionRules(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	t.Run("invalid transitions", func(t *testing.T) {
		d := mustCreate(t, s, newDep("a"))
		// detected -> post_hook, detected -> rejected are not allowed.
		for _, to := range []State{StatePostHook, StateRejected} {
			err := s.Transition(ctx, d.ID, to, Transition{From: StateDetected, Actor: "nops"})
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("detected->%s: err = %v, want ErrInvalidTransition", to, err)
			}
		}
		// Row untouched.
		got, _ := s.GetDeployment(ctx, d.ID)
		if got.State != StateDetected {
			t.Errorf("state changed to %s", got.State)
		}
	})

	t.Run("terminal states have no exit", func(t *testing.T) {
		for _, term := range []State{StateCompleted, StateFailed, StateRejected, StateSuperseded} {
			for _, to := range []State{StateDetected, StateApplying, StateCompleted} {
				if canTransition(term, to) {
					t.Errorf("%s -> %s should be forbidden", term, to)
				}
			}
		}
	})

	t.Run("stale From is a conflict", func(t *testing.T) {
		d := mustCreate(t, s, newDep("b"))
		if err := s.Transition(ctx, d.ID, StatePendingApproval, Transition{From: StateDetected, Actor: "nops"}); err != nil {
			t.Fatal(err)
		}
		err := s.Transition(ctx, d.ID, StateApplying, Transition{From: StateDetected, Actor: "nops"})
		if !errors.Is(err, ErrStateConflict) {
			t.Errorf("err = %v, want ErrStateConflict", err)
		}
		evs, _ := s.Events(ctx, d.ID)
		if len(evs) != 2 {
			t.Errorf("a rejected transition must not log an event, got %d events", len(evs))
		}
	})

	t.Run("unknown deployment", func(t *testing.T) {
		err := s.Transition(ctx, "missing", StateApplying, Transition{From: StateDetected, Actor: "nops"})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("actor and from required", func(t *testing.T) {
		if err := s.Transition(ctx, "x", StateApplying, Transition{From: StateDetected}); err == nil {
			t.Error("missing actor accepted")
		}
		if err := s.Transition(ctx, "x", StateApplying, Transition{Actor: "nops"}); err == nil {
			t.Error("missing from accepted")
		}
	})
}

// Two approvers racing on the same pending deployment: exactly one wins.
func TestConcurrentApprovalHasOneWinner(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))
	if err := s.Transition(ctx, d.ID, StatePendingApproval, Transition{From: StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			errs <- s.Transition(ctx, d.ID, StateApplying, Transition{From: StatePendingApproval, Actor: "u", DecidedBy: "u"})
		}()
	}
	wins := 0
	for i := 0; i < n; i++ {
		switch err := <-errs; {
		case err == nil:
			wins++
		case !errors.Is(err, ErrStateConflict):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d winners, want 1", wins)
	}
}

func TestLists(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a := mustCreate(t, s, newDep("a"))
	b := mustCreate(t, s, newDep("b"))
	c := mustCreate(t, s, newDep("c"))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Transition(ctx, a.ID, StatePendingApproval, Transition{From: StateDetected, Actor: "nops"}))
	must(s.Transition(ctx, b.ID, StateSuperseded, Transition{From: StateDetected, Actor: "nops"}))
	must(s.Transition(ctx, c.ID, StateCompleted, Transition{From: StateDetected, Actor: "nops"}))

	active, err := s.ListActive(ctx)
	if err != nil || len(active) != 1 || active[0].ID != a.ID {
		t.Errorf("ListActive = %+v, %v", active, err)
	}
	pending, err := s.ListByState(ctx, StatePendingApproval)
	if err != nil || len(pending) != 1 || pending[0].ID != a.ID {
		t.Errorf("ListByState = %+v, %v", pending, err)
	}
	hist, err := s.ListHistory(ctx, 10)
	if err != nil || len(hist) != 2 || hist[0].ID != c.ID || hist[1].ID != b.ID {
		t.Errorf("ListHistory should be newest first, got %+v, %v", hist, err)
	}
	if hist, _ := s.ListHistory(ctx, 1); len(hist) != 1 {
		t.Errorf("ListHistory limit not applied: %d rows", len(hist))
	}
}

func TestHookRunLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))

	run, created, err := s.EnsureHookRun(ctx, d.ID, "pre", "migrate", 10*time.Minute)
	if err != nil || !created {
		t.Fatalf("EnsureHookRun = %+v, created=%v, err=%v", run, created, err)
	}
	if run.State != HookDispatching || run.IdempotencyToken != d.ID+":pre" || run.Timeout != 10*time.Minute ||
		run.HookJobID != "migrate" || !run.FinishedAt.IsZero() {
		t.Errorf("new run = %+v", run)
	}

	// A restarted controller calls Ensure again and gets the same row back.
	again, created, err := s.EnsureHookRun(ctx, d.ID, "pre", "migrate", time.Minute)
	if err != nil || created || again.ID != run.ID || again.Timeout != 10*time.Minute {
		t.Errorf("second EnsureHookRun = %+v, created=%v, err=%v", again, created, err)
	}

	if err := s.UpdateHookRun(ctx, run.ID, HookUpdate{State: HookRunning, DispatchedJobID: "migrate/dispatch-1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetHookRun(ctx, d.ID, "pre")
	if got.State != HookRunning || got.DispatchedJobID != "migrate/dispatch-1" || !got.FinishedAt.IsZero() {
		t.Errorf("after running = %+v", got)
	}

	if err := s.UpdateHookRun(ctx, run.ID, HookUpdate{State: HookTimedOut, Error: "timeout after 10m0s"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetHookRun(ctx, d.ID, "pre")
	if got.State != HookTimedOut || got.Error != "timeout after 10m0s" || got.FinishedAt.IsZero() ||
		got.DispatchedJobID != "migrate/dispatch-1" {
		t.Errorf("after timeout = %+v", got)
	}

	// pre and post are independent runs of the same deployment.
	post, created, err := s.EnsureHookRun(ctx, d.ID, "post", "smoke", time.Minute)
	if err != nil || !created || post.ID == run.ID {
		t.Errorf("post run = %+v, created=%v, err=%v", post, created, err)
	}
}

func TestHookRunErrors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))

	if _, _, err := s.EnsureHookRun(ctx, d.ID, "during", "h", time.Minute); err == nil {
		t.Error("invalid phase accepted")
	}
	if _, _, err := s.EnsureHookRun(ctx, "no-such-deployment", "pre", "h", time.Minute); err == nil {
		t.Error("foreign key to missing deployment not enforced")
	}
	if _, err := s.GetHookRun(ctx, d.ID, "pre"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetHookRun(missing) err = %v", err)
	}
	if err := s.UpdateHookRun(ctx, "missing", HookUpdate{State: HookRunning}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateHookRun(missing) err = %v", err)
	}
	if err := s.UpdateHookRun(ctx, "missing", HookUpdate{}); err == nil {
		t.Error("UpdateHookRun without state accepted")
	}
}

// State must survive a restart: this is the whole point of persisting it.
func TestReopenKeepsState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nops.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d := newDep("web")
	if err := s1.CreateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s1.Transition(ctx, d.ID, StatePreHook, Transition{From: StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	run, _, err := s1.EnsureHookRun(ctx, d.ID, "pre", "migrate", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.UpdateHookRun(ctx, run.ID, HookUpdate{State: HookRunning, DispatchedJobID: "migrate/d1"}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(path) // migrations must be a no-op the second time
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	active, err := s2.ListActive(ctx)
	if err != nil || len(active) != 1 || active[0].State != StatePreHook {
		t.Fatalf("ListActive after reopen = %+v, %v", active, err)
	}
	h, err := s2.GetHookRun(ctx, d.ID, "pre")
	if err != nil || h.DispatchedJobID != "migrate/d1" || h.State != HookRunning {
		t.Errorf("hook run after reopen = %+v, %v", h, err)
	}
	if err := s2.CreateDeployment(ctx, newDep("web")); !errors.Is(err, ErrActiveDeployment) {
		t.Errorf("lock lost after reopen: %v", err)
	}
}

func TestSchemaVersion(t *testing.T) {
	s := newTestStore(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 1 {
		t.Errorf("user_version = %d, %v; want 1", v, err)
	}
}

func TestStateIsActive(t *testing.T) {
	for st, want := range map[State]bool{
		StateDetected: true, StatePendingApproval: true, StatePreHook: true, StateApplying: true, StatePostHook: true,
		StateCompleted: false, StateFailed: false, StateRejected: false, StateSuperseded: false,
	} {
		if st.IsActive() != want {
			t.Errorf("%s.IsActive() = %v, want %v", st, st.IsActive(), want)
		}
	}
}

func TestLatestDeployment(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.LatestDeployment(ctx, "default", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	first := mustCreate(t, s, newDep("web"))
	if err := s.Transition(ctx, first.ID, StateFailed, Transition{From: StateDetected, Actor: "nops", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestDeployment(ctx, "default", "web")
	if err != nil || got.ID != first.ID || got.State != StateFailed {
		t.Fatalf("LatestDeployment = %+v, %v", got, err)
	}

	// A newer deployment for the same job wins, even over an older one still
	// unresolved elsewhere (the per-job lock guarantees there is at most one
	// active at a time, but LatestDeployment looks at every state).
	second := mustCreate(t, s, newDep("web"))
	got, err = s.LatestDeployment(ctx, "default", "web")
	if err != nil || got.ID != second.ID {
		t.Fatalf("LatestDeployment = %+v, %v, want %s", got, err, second.ID)
	}

	// A different job or namespace is independent.
	if _, err := s.LatestDeployment(ctx, "default", "db"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("db: err = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestDeployment(ctx, "staging", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("staging/web: err = %v, want ErrNotFound", err)
	}
}

func TestSetApplied(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))
	if err := s.Transition(ctx, d.ID, StateApplying, Transition{From: StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetApplied(ctx, d.ID, 7, "eval-1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil || got.AppliedIndex != 7 || got.EvalID != "eval-1" || got.State != StateApplying {
		t.Fatalf("deployment = %+v, %v", got, err)
	}

	// A crash-recovery call with no fresh eval ID keeps the one already
	// recorded rather than blanking it out.
	if err := s.SetApplied(ctx, d.ID, 8, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetDeployment(ctx, d.ID)
	if got.AppliedIndex != 8 || got.EvalID != "eval-1" {
		t.Errorf("deployment = %+v", got)
	}

	if err := s.SetApplied(ctx, d.ID, 0, "eval-2"); err == nil {
		t.Error("appliedIndex = 0: want an error")
	}

	// The deployment moved on: SetApplied no longer matches applying.
	if err := s.Transition(ctx, d.ID, StateCompleted, Transition{From: StateApplying, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetApplied(ctx, d.ID, 9, "eval-3"); !errors.Is(err, ErrStateConflict) {
		t.Errorf("err = %v, want ErrStateConflict", err)
	}

	if err := s.SetApplied(ctx, "missing", 1, "eval-4"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAppliedSince(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))

	if _, err := s.AppliedSince(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before applying: err = %v, want ErrNotFound", err)
	}

	if err := s.Transition(ctx, d.ID, StateApplying, Transition{From: StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	since, err := s.AppliedSince(ctx, d.ID)
	if err != nil || since.IsZero() {
		t.Fatalf("AppliedSince = %v, %v", since, err)
	}
	evs, _ := s.Events(ctx, d.ID)
	var want time.Time
	for _, e := range evs {
		if e.To == StateApplying {
			want = e.Time
		}
	}
	if !since.Equal(want) {
		t.Errorf("AppliedSince = %v, want %v (the -> applying event)", since, want)
	}
}

func TestOpenFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permissions are not enforced the same way on windows")
	}
	path := filepath.Join(t.TempDir(), "nops.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustCreate(t, s, newDep("web")) // a write forces the WAL/SHM sidecars to exist

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s: mode = %o, want 0600 (job_spec is not redacted)", p, mode)
		}
	}
}
