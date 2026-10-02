package store

import (
	"context"
	"testing"
)

func TestWindowLiftIsOnePerJob(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.RequestWindowLift(ctx, "default", "web", "h1", "alice"); err != nil {
		t.Fatalf("RequestWindowLift: %v", err)
	}
	if err := s.RequestWindowLift(ctx, "default", "web", "h2", "bob"); err != nil {
		t.Fatalf("second RequestWindowLift: %v", err)
	}
	// The same ID in another namespace is another job.
	if err := s.RequestWindowLift(ctx, "prod", "web", "h3", "carol"); err != nil {
		t.Fatalf("RequestWindowLift in another namespace: %v", err)
	}

	got, err := s.WindowLifts(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("WindowLifts = %+v, %v, want two", got, err)
	}
	for _, l := range got {
		if l.Namespace == "default" && (l.SpecHash != "h2" || l.RequestedBy != "bob" || l.RequestedAt.IsZero()) {
			t.Errorf("the new request did not replace the one waiting: %+v", l)
		}
	}
}

func TestWindowLiftRequiresAnActorAndASpec(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.RequestWindowLift(ctx, "default", "web", "h1", ""); err == nil {
		t.Error("RequestWindowLift without an actor: want an error")
	}
	if err := s.RequestWindowLift(ctx, "default", "web", "", "alice"); err == nil {
		t.Error("RequestWindowLift without a spec hash: want an error")
	}
	if got, err := s.WindowLifts(ctx); err != nil || len(got) != 0 {
		t.Errorf("WindowLifts = %+v, %v, want none", got, err)
	}
}

func TestDropWindowLift(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if dropped, err := s.DropWindowLift(ctx, "default", "web"); err != nil || dropped {
		t.Errorf("DropWindowLift of none = %v, %v, want false and no error", dropped, err)
	}
	if err := s.RequestWindowLift(ctx, "default", "web", "h1", "alice"); err != nil {
		t.Fatal(err)
	}
	if dropped, err := s.DropWindowLift(ctx, "default", "web"); err != nil || !dropped {
		t.Fatalf("DropWindowLift = %v, %v, want true and no error", dropped, err)
	}
	if got, err := s.WindowLifts(ctx); err != nil || len(got) != 0 {
		t.Errorf("WindowLifts after the drop = %+v, %v, want none", got, err)
	}
}

// The request is used up with the deployment it led to, and who asked is on
// the deployment and in its events, in the one transaction.
func TestCreateDeploymentUsesTheWindowLift(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.RequestWindowLift(ctx, "default", "web", "hash-web", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestWindowLift(ctx, "default", "api", "hash-api", "bob"); err != nil {
		t.Fatal(err)
	}

	d := newDep("web")
	d.WindowLiftedBy = "alice"
	mustCreate(t, s, d)

	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil || got.WindowLiftedBy != "alice" {
		t.Errorf("deployment = %+v, %v, want WindowLiftedBy alice", got, err)
	}
	lifts, err := s.WindowLifts(ctx)
	if err != nil || len(lifts) != 1 || lifts[0].JobID != "api" {
		t.Errorf("WindowLifts = %+v, %v, want only the other job's", lifts, err)
	}
	events, err := s.Events(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Actor == "alice" && e.From == StateDetected && e.To == StateDetected {
			found = true
		}
	}
	if !found {
		t.Errorf("no event by alice for the deploy now: %+v", events)
	}
}

func TestADeploymentWithoutALiftHasNoWindowLiftedBy(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil || got.WindowLiftedBy != "" {
		t.Errorf("deployment = %+v, %v, want no WindowLiftedBy", got, err)
	}
}
