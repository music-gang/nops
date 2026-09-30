package store

import (
	"context"
	"errors"
	"testing"
)

func TestPauseAndResume(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.PauseJob(ctx, "default", "web", "alice", "db incident"); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	p, err := s.PauseOf(ctx, "default", "web")
	if err != nil {
		t.Fatalf("PauseOf: %v", err)
	}
	if p.PausedBy != "alice" || p.Reason != "db incident" || p.PausedAt.IsZero() || !p.ResumedAt.IsZero() {
		t.Errorf("PauseOf = %+v, want a pause by alice for a db incident, not resumed", p)
	}

	if err := s.ResumeJob(ctx, "default", "web", "bob"); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if _, err := s.PauseOf(ctx, "default", "web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("PauseOf after the resume: err = %v, want ErrNotFound", err)
	}
	if got, err := s.ActivePauses(ctx); err != nil || len(got) != 0 {
		t.Errorf("ActivePauses after the resume = %+v, %v, want none", got, err)
	}
}

func TestPauseIsOnePerJob(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.PauseJob(ctx, "default", "web", "alice", ""); err != nil {
		t.Fatal(err)
	}

	if err := s.PauseJob(ctx, "default", "web", "bob", ""); !errors.Is(err, ErrAlreadyPaused) {
		t.Errorf("second PauseJob: err = %v, want ErrAlreadyPaused", err)
	}
	// The same ID in another namespace is another job.
	if err := s.PauseJob(ctx, "prod", "web", "bob", ""); err != nil {
		t.Errorf("PauseJob in another namespace: %v", err)
	}
	if err := s.PauseJob(ctx, "default", "api", "bob", ""); err != nil {
		t.Errorf("PauseJob of another job: %v", err)
	}
}

func TestAJobCanBePausedAgainAfterTheResume(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, by := range []string{"alice", "bob"} {
		if err := s.PauseJob(ctx, "default", "web", by, "round of "+by); err != nil {
			t.Fatalf("PauseJob by %s: %v", by, err)
		}
		if err := s.ResumeJob(ctx, "default", "web", by); err != nil {
			t.Fatalf("ResumeJob by %s: %v", by, err)
		}
	}
	// The resumed pauses stay as the record of who did what.
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM job_pauses WHERE resumed_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("resumed pauses kept = %d, want 2", n)
	}
}

func TestResumeWithoutAPause(t *testing.T) {
	s := newTestStore(t)
	if err := s.ResumeJob(context.Background(), "default", "web", "alice"); !errors.Is(err, ErrNotPaused) {
		t.Errorf("ResumeJob: err = %v, want ErrNotPaused", err)
	}
}

func TestPauseRequiresAnActor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.PauseJob(ctx, "default", "web", "", ""); err == nil {
		t.Error("PauseJob with no actor succeeded")
	}
	if err := s.ResumeJob(ctx, "default", "web", ""); err == nil {
		t.Error("ResumeJob with no actor succeeded")
	}
}

func TestActivePausesOldestFirst(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, job := range []string{"web", "api", "db"} {
		if err := s.PauseJob(ctx, "default", job, "alice", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ResumeJob(ctx, "default", "api", "alice"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ActivePauses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].JobID != "web" || got[1].JobID != "db" {
		t.Errorf("ActivePauses = %+v, want web then db", got)
	}
}
