package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// withSpecs is a deployment of job with a job file, a var-file and a frozen
// hook, everything DropEndedSpecs drops.
func withSpecs(t *testing.T, s *Store, job string) *Deployment {
	t.Helper()
	d := newDep(job)
	d.JobSource, d.JobVars = `job "`+job+`" {}`, "image = \"v1\"\n"
	d.Hooks = []DeploymentHook{hookRow("pre", 0, "migrate")}
	return mustCreate(t, s, d)
}

// endAs moves d from detected through each state in turn.
func endAs(t *testing.T, s *Store, d *Deployment, states ...State) *Deployment {
	t.Helper()
	from := StateDetected
	for _, to := range states {
		if err := s.Transition(context.Background(), d.ID, to, Transition{From: from, Actor: "nops", Error: "x"}); err != nil {
			t.Fatal(err)
		}
		from = to
	}
	return d
}

func dropSpecs(t *testing.T, s *Store, before time.Time) int {
	t.Helper()
	n, err := s.DropEndedSpecs(context.Background(), before)
	if err != nil {
		t.Fatalf("DropEndedSpecs: %v", err)
	}
	return n
}

// assertSpecs checks whether d still has its spec, job file, var-file and
// hook spec, and that what is never dropped is still there.
func assertSpecs(t *testing.T, s *Store, name string, d *Deployment, kept bool) {
	t.Helper()
	ctx := context.Background()
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := s.DeploymentHooks(ctx, d.ID)
	if err != nil || len(hooks) != 1 {
		t.Fatalf("%s: hooks = %+v, %v; want the frozen hook, with or without its spec", name, hooks, err)
	}
	if got.PlanDiff != d.PlanDiff || got.SpecHash != d.SpecHash ||
		hooks[0].Revision != "migrate-0a1b2c3d" || hooks[0].SpecHash != "hash-migrate" {
		t.Errorf("%s lost what is never dropped: %+v, hook %+v", name, got, hooks[0])
	}
	has := got.JobSpec != "" && got.JobSource != "" && got.JobVars != "" && hooks[0].JobSpec != ""
	gone := got.JobSpec == "" && got.JobSource == "" && got.JobVars == "" && hooks[0].JobSpec == ""
	switch {
	case kept && (!has || !got.SpecsDroppedAt.IsZero()):
		t.Errorf("%s should keep its specs: spec %q, source %q, vars %q, hook %q, dropped at %v",
			name, got.JobSpec, got.JobSource, got.JobVars, hooks[0].JobSpec, got.SpecsDroppedAt)
	case !kept && (!gone || got.SpecsDroppedAt.IsZero()):
		t.Errorf("%s should have dropped its specs: spec %q, source %q, vars %q, hook %q, dropped at %v",
			name, got.JobSpec, got.JobSource, got.JobVars, hooks[0].JobSpec, got.SpecsDroppedAt)
	}
}

func TestDropEndedSpecs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	oldWeb := endAs(t, s, withSpecs(t, s, "web"), StateCompleted)
	failed := endAs(t, s, withSpecs(t, s, "web"), StateFailed)
	lastWeb := endAs(t, s, withSpecs(t, s, "web"), StateCompleted)
	rejected := endAs(t, s, withSpecs(t, s, "api"), StatePendingApproval, StateRejected)
	superseded := endAs(t, s, withSpecs(t, s, "api"), StateSuperseded)
	active := endAs(t, s, withSpecs(t, s, "db"), StatePendingApproval)
	cutoff := s.now()
	recent := endAs(t, s, withSpecs(t, s, "worker"), StateFailed)
	events, err := s.Events(ctx, oldWeb.ID)
	if err != nil {
		t.Fatal(err)
	}

	if n := dropSpecs(t, s, cutoff); n != 4 {
		t.Errorf("dropped %d, want 4: the ended deployments before the cutoff, less the latest completed", n)
	}
	for name, d := range map[string]*Deployment{"an older completed": oldWeb, "failed": failed, "rejected": rejected, "superseded": superseded} {
		assertSpecs(t, s, name, d, false)
	}
	for name, d := range map[string]*Deployment{"the latest completed": lastWeb, "an active one": active, "one ended after the cutoff": recent} {
		assertSpecs(t, s, name, d, true)
	}
	if after, err := s.Events(ctx, oldWeb.ID); err != nil || len(after) != len(events) {
		t.Errorf("events = %d, %v; want the %d it had", len(after), err, len(events))
	}

	dropped, _ := s.GetDeployment(ctx, oldWeb.ID)
	if n := dropSpecs(t, s, cutoff); n != 0 {
		t.Errorf("a second sweep dropped %d, want 0", n)
	}
	if again, _ := s.GetDeployment(ctx, oldWeb.ID); !again.SpecsDroppedAt.Equal(dropped.SpecsDroppedAt) {
		t.Errorf("a second sweep moved when the specs were dropped: %v, was %v", again.SpecsDroppedAt, dropped.SpecsDroppedAt)
	}

	// Once a newer one completes, the old latest completed goes like any other.
	endAs(t, s, withSpecs(t, s, "web"), StateCompleted)
	if n := dropSpecs(t, s, cutoff); n != 1 {
		t.Errorf("dropped %d, want the deployment that is no longer the latest completed", n)
	}
	assertSpecs(t, s, "the former latest completed", lastWeb, false)
}

func TestDropEndedSpecsInBatches(t *testing.T) {
	s := newTestStore(t)
	defer func(n int) { dropSpecsBatch = n }(dropSpecsBatch)
	dropSpecsBatch = 2

	var ds []*Deployment
	for range 5 {
		ds = append(ds, endAs(t, s, withSpecs(t, s, "web"), StateFailed))
	}
	if n := dropSpecs(t, s, s.now()); n != 5 {
		t.Errorf("dropped %d, want all 5 across the batches", n)
	}
	for _, d := range ds {
		assertSpecs(t, s, d.ID, d, false)
	}
}

func TestDropEndedSpecsDoesNotScanTheHistory(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+dropSpecsSQL, formatTime(t0), formatTime(t0), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	for _, step := range plan {
		if strings.HasPrefix(step, "SCAN ") {
			t.Errorf("the sweep scans a table: %q in %q", step, plan)
		}
	}
	if !strings.Contains(strings.Join(plan, "\n"), "deployments_specs_to_drop") {
		t.Errorf("the sweep does not read the index of what is left to drop: %q", plan)
	}
}
