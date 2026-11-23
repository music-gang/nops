package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/store"
)

func TestDismissedOrphanIsNotReported(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.deployed("web", store.PolicyAuto)
	h.runningInNomad("web", "running")
	h.deployed("db", store.PolicyAuto)
	h.runningInNomad("db", "running")
	h.detect()

	if err := h.engine.DismissOrphan(ctx, testNamespace, "web", d.ID, "alice"); err != nil {
		t.Fatalf("DismissOrphan: %v", err)
	}
	// Gone at once, for the page the person is sent back to.
	if got := orphanIDs(h.engine.Orphans()); got != "db" {
		t.Errorf("orphans right after the dismissal = %q, want db", got)
	}
	if st := h.engine.Status(); st.Orphans != 1 {
		t.Errorf("Status.Orphans = %d, want 1", st.Orphans)
	}
	if got, err := h.store.DismissalOf(ctx, testNamespace, "web"); err != nil || got.DismissedBy != "alice" || got.DeploymentID != d.ID {
		t.Errorf("DismissalOf = %+v, %v", got, err)
	}

	// The next cycles do not bring it back, and do not even look it up.
	h.nomad.jobErr["web"] = errors.New("looked up")
	logs := new(bytes.Buffer)
	h.engine.log = slog.New(slog.NewTextHandler(logs, nil))
	h.detect()
	if got := orphanIDs(h.engine.Orphans()); got != "db" {
		t.Errorf("orphans after a cycle = %q, want db", got)
	}
	if strings.Contains(logs.String(), "looked up") {
		t.Errorf("a dismissed orphan was looked up in Nomad: %s", logs)
	}
	if len(h.nomad.registerCalls) != 0 {
		t.Errorf("register calls = %+v: a dismissal never touches Nomad", h.nomad.registerCalls)
	}
}

func TestUndismissedOrphanIsReportedAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.deployed("web", store.PolicyAuto)
	h.runningInNomad("web", "running")
	h.detect()
	if err := h.engine.DismissOrphan(ctx, testNamespace, "web", d.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	if err := h.engine.UndismissOrphan(ctx, testNamespace, "web", "bob"); err != nil {
		t.Fatalf("UndismissOrphan: %v", err)
	}
	h.detect()
	if got := orphanIDs(h.engine.Orphans()); got != "web" {
		t.Errorf("orphans after the undo = %q, want web", got)
	}
	if err := h.engine.UndismissOrphan(ctx, testNamespace, "web", "bob"); !errors.Is(err, store.ErrNotDismissed) {
		t.Errorf("second UndismissOrphan = %v, want store.ErrNotDismissed", err)
	}
}

// A new deployment of the job that completes ends the dismissal: removed from
// git again, it is reported again.
func TestDismissalEndsWithANewCompletedDeployment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := h.deployed("web", store.PolicyAuto)
	h.runningInNomad("web", "running")
	h.detect()
	if err := h.engine.DismissOrphan(ctx, testNamespace, "web", d.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	h.deployed("web", store.PolicyAuto)
	h.detect()
	if got := orphanIDs(h.engine.Orphans()); got != "web" {
		t.Errorf("orphans after a new completed deployment = %q, want web", got)
	}
}

func TestDismissOrphanRefusesWhatIsNotTheOrphanShown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	old := h.deployed("web", store.PolicyAuto)
	last := h.deployed("web", store.PolicyAuto)
	h.runningInNomad("web", "running")
	h.detect()

	for _, c := range []struct {
		name, ns, job, dep string
		want               error
	}{
		{"an older deployment", testNamespace, "web", old.ID, ErrNotOrphan},
		{"a job that is not an orphan", testNamespace, "db", last.ID, ErrNotOrphan},
		{"an unmanaged namespace", "elsewhere", "web", last.ID, store.ErrNotFound},
	} {
		if err := h.engine.DismissOrphan(ctx, c.ns, c.job, c.dep, "alice"); !errors.Is(err, c.want) {
			t.Errorf("%s: DismissOrphan = %v, want %v", c.name, err, c.want)
		}
	}
	if err := h.engine.DismissOrphan(ctx, testNamespace, "web", last.ID, ""); err == nil {
		t.Error("DismissOrphan without an actor: want an error")
	}
	if got := orphanIDs(h.engine.Orphans()); got != "web" {
		t.Errorf("orphans = %q, want web still reported", got)
	}
}

func TestDismissOrphanStoreFailureKeepsTheOrphan(t *testing.T) {
	h := newHarness(t)
	d := h.deployed("web", store.PolicyAuto)
	h.runningInNomad("web", "running")
	h.detect()
	h.engine.store = failingDismiss{Store: h.engine.store}

	if err := h.engine.DismissOrphan(context.Background(), testNamespace, "web", d.ID, "alice"); err == nil {
		t.Fatal("DismissOrphan = nil, want the store failure")
	}
	if got := orphanIDs(h.engine.Orphans()); got != "web" {
		t.Errorf("orphans = %q: a dismissal not on record must not hide it", got)
	}
}

type failingDismiss struct{ Store }

func (failingDismiss) DismissOrphan(context.Context, string, string, string, string) error {
	return fmt.Errorf("disk on fire")
}
