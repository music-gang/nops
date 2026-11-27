package store

import (
	"context"
	"errors"
	"testing"
)

// completedDep creates a deployment of job and completes it.
func completedDep(t *testing.T, s *Store, job string) *Deployment {
	t.Helper()
	d := mustCreate(t, s, newDep(job))
	if err := s.Transition(context.Background(), d.ID, StateCompleted, Transition{From: StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDismissAndUndismissOrphan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := completedDep(t, s, "web")

	if err := s.DismissOrphan(ctx, "default", "web", d.ID, "alice"); err != nil {
		t.Fatalf("DismissOrphan: %v", err)
	}
	got, err := s.DismissalOf(ctx, "default", "web")
	if err != nil || got.DeploymentID != d.ID || got.DismissedBy != "alice" || got.DismissedAt.IsZero() {
		t.Fatalf("DismissalOf = %+v, %v", got, err)
	}
	if err := s.DismissOrphan(ctx, "default", "web", d.ID, "bob"); !errors.Is(err, ErrAlreadyDismissed) {
		t.Errorf("second DismissOrphan = %v, want ErrAlreadyDismissed", err)
	}

	if err := s.UndismissOrphan(ctx, "default", "web"); err != nil {
		t.Fatalf("UndismissOrphan: %v", err)
	}
	if got, err := s.OrphanCandidates(ctx, "default"); err != nil || len(got) != 1 {
		t.Errorf("OrphanCandidates after the undo = %v, %v, want web again", got, err)
	}
	if _, err := s.DismissalOf(ctx, "default", "web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DismissalOf after the undo = %v, want ErrNotFound", err)
	}
	if err := s.UndismissOrphan(ctx, "default", "web"); !errors.Is(err, ErrNotDismissed) {
		t.Errorf("UndismissOrphan of none = %v, want ErrNotDismissed", err)
	}
}

// A dismissed job is no orphan candidate; the others of its namespace, and
// the same ID in another namespace, still are.
func TestOrphanCandidatesLeaveOutTheDismissed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	web := completedDep(t, s, "web")
	db := completedDep(t, s, "db")
	other := newDep("web")
	other.Namespace = "prod"
	mustCreate(t, s, other)
	if err := s.Transition(ctx, other.ID, StateCompleted, Transition{From: StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissOrphan(ctx, "default", "web", web.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	got, err := s.OrphanCandidates(ctx, "default")
	if err != nil || len(got) != 1 || got[0].ID != db.ID {
		t.Errorf("OrphanCandidates(default) = %v, %v, want db only", got, err)
	}
	if got, err := s.OrphanCandidates(ctx, "prod"); err != nil || len(got) != 1 || got[0].ID != other.ID {
		t.Errorf("OrphanCandidates(prod) = %v, %v, want its web", got, err)
	}

	// One job at a time, the same answers.
	if got, err := s.OrphanCandidate(ctx, "default", "db"); err != nil || got.ID != db.ID {
		t.Errorf("OrphanCandidate(default, db) = %v, %v, want db", got, err)
	}
	if got, err := s.OrphanCandidate(ctx, "prod", "web"); err != nil || got.ID != other.ID {
		t.Errorf("OrphanCandidate(prod, web) = %v, %v, want its web", got, err)
	}
	if _, err := s.OrphanCandidate(ctx, "default", "web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OrphanCandidate of the dismissed = %v, want ErrNotFound", err)
	}
	if _, err := s.OrphanCandidate(ctx, "default", "never"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OrphanCandidate of a job never deployed = %v, want ErrNotFound", err)
	}
}

func TestDismissOrphanTakesOnlyTheLatestCompletedDeployment(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	old := completedDep(t, s, "web")
	completedDep(t, s, "web")
	active := mustCreate(t, s, newDep("db"))

	for _, c := range []struct{ job, id string }{
		{"web", old.ID},        // an older completed deployment
		{"db", active.ID},      // never completed
		{"gone", "no-such-id"}, // no deployment at all
	} {
		if err := s.DismissOrphan(ctx, "default", c.job, c.id, "alice"); !errors.Is(err, ErrNotLatestCompleted) {
			t.Errorf("DismissOrphan(%s, %s) = %v, want ErrNotLatestCompleted", c.job, c.id, err)
		}
	}
	if err := s.DismissOrphan(ctx, "default", "web", old.ID, ""); err == nil {
		t.Error("DismissOrphan without an actor: want an error")
	}
}

func TestANewCompletedDeploymentEndsTheDismissal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := completedDep(t, s, "web")
	db := completedDep(t, s, "db")
	if err := s.DismissOrphan(ctx, "default", "web", d.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissOrphan(ctx, "default", "db", db.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	// A deployment that does not complete leaves it.
	failed := mustCreate(t, s, newDep("web"))
	if err := s.Transition(ctx, failed.ID, StateFailed, Transition{From: StateDetected, Actor: "nops", Error: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DismissalOf(ctx, "default", "web"); err != nil {
		t.Fatalf("DismissalOf after a failed deployment = %v, want the dismissal", err)
	}

	completedDep(t, s, "web")
	if _, err := s.DismissalOf(ctx, "default", "web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DismissalOf after a completed deployment = %v, want ErrNotFound", err)
	}
	if _, err := s.DismissalOf(ctx, "default", "db"); err != nil {
		t.Errorf("the dismissal of another job went too: %v", err)
	}
}
