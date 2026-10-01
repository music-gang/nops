package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/store"
)

// A job with canary > 0 and auto_promote = false leaves its Nomad deployment
// running until a person promotes the canaries. That wait is not a failure to
// become healthy: the apply timeout does not run in it, and counts again from the
// promotion (docs/archive/engine-apply.md, decision 10).

// canaryJob is a managed job whose one group "g" has a canary.
func canaryJob(autoPromote bool, extra map[string]string) *api.Job {
	g := taskGroup("g", 1, false)
	canary := 1
	g.Update = &api.UpdateStrategy{Canary: &canary, AutoPromote: &autoPromote}
	return managed("web", "auto", extra, g)
}

// canaryDeployment is the running Nomad deployment of the applied index 9.
func canaryDeployment(state api.DeploymentState) *api.Deployment {
	return &api.Deployment{
		ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusRunning,
		StatusDescription: "Deployment is running but requires manual promotion",
		TaskGroups:        map[string]*api.DeploymentState{"g": &state},
	}
}

// healthyCanary is a group whose canary is placed and healthy, not promoted.
func healthyCanary() api.DeploymentState {
	return api.DeploymentState{DesiredCanaries: 1, DesiredTotal: 1, PlacedCanaries: []string{"a1"}, HealthyAllocs: 1}
}

func promotedCanary() api.DeploymentState {
	s := healthyCanary()
	s.Promoted = true
	return s
}

func (h *harness) waitingForPromotion(job *api.Job) *store.Deployment {
	h.t.Helper()
	h.engine.applyTimeout = time.Minute
	d := h.applyingWithIndex("web", job, 9)
	h.nomad.setDeployment("web", canaryDeployment(healthyCanary()))
	return d
}

func TestCanaryWaitingForPromotionDoesNotFailAtTheApplyTimeout(t *testing.T) {
	h := newHarness(t)
	d := h.waitingForPromotion(canaryJob(false, nil))

	h.step(d)
	if got := h.get(d.ID); got.State != store.StateApplying || got.PromotionWaitSince.IsZero() || !got.PromotedAt.IsZero() {
		t.Fatalf("after the first step: %+v, want applying and waiting for a promotion", got)
	}

	// Days later, still nobody promoted: nothing is failing.
	h.clock.Advance(72 * time.Hour)
	h.step(h.get(d.ID))
	if got := h.get(d.ID); got.State != store.StateApplying || got.Error != "" {
		t.Fatalf("after the apply timeout: state %s (%q), want still applying", got.State, got.Error)
	}
}

func TestCanaryWaitIsNotifiedOnceAndLogged(t *testing.T) {
	h := newHarness(t)
	d := h.waitingForPromotion(canaryJob(false, nil))

	for i := 0; i < 3; i++ {
		h.step(h.get(d.ID))
	}
	h.notifier.waitFor(t, 1)
	time.Sleep(50 * time.Millisecond) // a second one would be on its way
	calls := h.notifier.waitFor(t, 1)
	if len(calls) != 1 || calls[0].ID != d.ID || calls[0].State != store.StateApplying || calls[0].PromotionWaitSince.IsZero() {
		t.Fatalf("notifications = %+v, want exactly one, of the applying deployment already waiting", calls)
	}

	// A restart does not tell them again.
	h.restart(&fakeHooks{})
	h.step(h.get(d.ID))
	time.Sleep(50 * time.Millisecond)
	if n := len(h.notifier.waitFor(t, 1)); n != 1 {
		t.Errorf("%d notifications after a restart, want still 1", n)
	}

	evs, err := h.store.Events(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.From != store.StateApplying || last.To != store.StateApplying || last.Message != "waiting for canary promotion in Nomad" {
		t.Errorf("last event = %+v, want the applying -> applying wait", last)
	}
}

func TestCanaryPromotionRestartsTheApplyTimeout(t *testing.T) {
	h := newHarness(t)
	d := h.waitingForPromotion(canaryJob(false, nil))
	h.step(d)
	h.clock.Advance(time.Hour) // the wait, way past the timeout

	// Promoted: the rest of the rollout is still running, the timeout counts from now.
	h.nomad.setDeployment("web", canaryDeployment(promotedCanary()))
	h.step(h.get(d.ID))
	got := h.get(d.ID)
	if got.State != store.StateApplying || got.PromotedAt.IsZero() {
		t.Fatalf("after the promotion: %+v, want applying with promoted_at set", got)
	}

	h.clock.Advance(59 * time.Second)
	h.step(got)
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Fatalf("state = %s (%s) 59s after the promotion, want applying", got.State, got.Error)
	}

	h.clock.Advance(2 * time.Second)
	h.step(h.get(d.ID))
	if got := h.get(d.ID); got.State != store.StateFailed {
		t.Fatalf("state = %s a minute after the promotion, want failed: the rollout that follows has the apply timeout", got.State)
	}
}

func TestCanaryPromotedThenSuccessfulCompletes(t *testing.T) {
	h := newHarness(t)
	d := h.waitingForPromotion(canaryJob(false, map[string]string{"nops_post_hook": "web-smoke"}))
	h.step(d)
	h.clock.Advance(time.Hour)

	h.nomad.setDeployment("web", &api.Deployment{
		ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusSuccessful,
		TaskGroups: map[string]*api.DeploymentState{"g": func() *api.DeploymentState { s := promotedCanary(); return &s }()},
	})
	h.step(h.get(d.ID))
	if got := h.get(d.ID); got.State != store.StatePostHook {
		t.Fatalf("state = %s, want post_hook: the post-hooks run once the canaries are promoted and healthy", got.State)
	}
}

func TestCanaryWaitEndsWhenNomadFailsTheDeployment(t *testing.T) {
	h := newHarness(t)
	d := h.waitingForPromotion(canaryJob(false, nil))
	h.step(d)

	h.nomad.setDeployment("web", &api.Deployment{
		ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusFailed, StatusDescription: "Failed due to progress deadline",
	})
	h.step(h.get(d.ID))
	if got := h.get(d.ID); got.State != store.StateFailed {
		t.Fatalf("state = %s, want failed: someone failed the Nomad deployment", got.State)
	}
}

func TestCanaryWaitSurvivesANomadError(t *testing.T) {
	h := newHarness(t)
	d := h.waitingForPromotion(canaryJob(false, nil))
	h.step(d)
	h.clock.Advance(time.Hour)

	h.nomad.deployErr[nsKey(testNamespace, "web")] = errors.New("nomad is down")
	h.step(h.get(d.ID))
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Fatalf("state = %s (%s), want applying: an error while waiting for a person is retried, not a failure", got.State, got.Error)
	}
}

// What is not a wait for a person keeps the apply timeout.
func TestCanaryThatIsNotWaitingForAPersonStillTimesOut(t *testing.T) {
	starting := healthyCanary()
	starting.HealthyAllocs = 0
	for name, tt := range map[string]struct {
		job *api.Job
		dep *api.Deployment
	}{
		"canary still starting": {canaryJob(false, nil), canaryDeployment(starting)},
		"auto_promote":          {canaryJob(true, nil), canaryDeployment(healthyCanary())},
		"no canary":             {canaryJob(false, nil), canaryDeployment(api.DeploymentState{DesiredTotal: 1})},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.engine.applyTimeout = time.Minute
			d := h.applyingWithIndex("web", tt.job, 9)
			h.nomad.setDeployment("web", tt.dep)

			h.step(d)
			if got := h.get(d.ID); got.State != store.StateApplying || !got.PromotionWaitSince.IsZero() {
				t.Fatalf("after the first step: %+v, want applying and not waiting for a person", got)
			}
			if n := len(h.notifier.calls); n != 0 {
				t.Errorf("%d notifications, want none", n)
			}

			h.clock.Advance(2 * time.Minute)
			h.step(h.get(d.ID))
			if got := h.get(d.ID); got.State != store.StateFailed {
				t.Fatalf("state = %s, want failed at the apply timeout", got.State)
			}
		})
	}
}

// A restart in the middle of the wait, or after the promotion, picks up where
// it was: the first apply cycle neither fails the wait nor forgets the promotion.
func TestCanaryWaitAcrossARestart(t *testing.T) {
	h := newHarness(t)
	d := h.waitingForPromotion(canaryJob(false, nil))
	h.step(d)
	h.clock.Advance(time.Hour)

	e := h.restart(&fakeHooks{})
	e.applyTimeout = time.Minute
	e.applyStep(context.Background(), h.get(d.ID))
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Fatalf("state = %s after a restart in the wait, want applying", got.State)
	}

	h.nomad.setDeployment("web", canaryDeployment(promotedCanary()))
	e.applyStep(context.Background(), h.get(d.ID))
	h.restart(&fakeHooks{}).applyTimeout = time.Minute
	h.clock.Advance(30 * time.Second)
	h.engine.applyStep(context.Background(), h.get(d.ID))
	if got := h.get(d.ID); got.State != store.StateApplying || got.PromotedAt.IsZero() {
		t.Fatalf("state = %s, promoted_at %v after a restart after the promotion, want applying, counting from the promotion", got.State, got.PromotedAt)
	}
}

func TestCanaryHealth(t *testing.T) {
	two := func() *api.Job {
		a, b := taskGroup("a", 1, false), taskGroup("b", 1, false)
		canary := 1
		yes, no := true, false
		a.Update = &api.UpdateStrategy{Canary: &canary, AutoPromote: &no}
		b.Update = &api.UpdateStrategy{Canary: &canary, AutoPromote: &yes}
		return managed("web", "auto", nil, a, b)
	}
	healthy, promoted := healthyCanary(), promotedCanary()
	starting := healthyCanary()
	starting.PlacedCanaries = nil

	for name, tt := range map[string]struct {
		job    *api.Job
		groups map[string]api.DeploymentState
		want   health
	}{
		"healthy, not promoted":        {canaryJob(false, nil), map[string]api.DeploymentState{"g": healthy}, healthAwaitingPromotion},
		"promoted":                     {canaryJob(false, nil), map[string]api.DeploymentState{"g": promoted}, healthPromoted},
		"not placed yet":               {canaryJob(false, nil), map[string]api.DeploymentState{"g": starting}, healthWaiting},
		"group unknown to the deploy":  {canaryJob(false, nil), map[string]api.DeploymentState{}, healthWaiting},
		"the job's own update block":   {jobWithJobLevelCanary(), map[string]api.DeploymentState{"g": healthy}, healthAwaitingPromotion},
		"only the manual group counts": {two(), map[string]api.DeploymentState{"a": healthy, "b": {DesiredCanaries: 1}}, healthAwaitingPromotion},
		"one group promoted, one not":  {twoManual(), map[string]api.DeploymentState{"a": promoted, "b": healthy}, healthAwaitingPromotion},
		"one group still starting":     {twoManual(), map[string]api.DeploymentState{"a": healthy, "b": starting}, healthWaiting},
	} {
		t.Run(name, func(t *testing.T) {
			dep := &api.Deployment{Status: api.DeploymentStatusRunning, TaskGroups: map[string]*api.DeploymentState{}}
			for g, s := range tt.groups {
				s := s
				dep.TaskGroups[g] = &s
			}
			if got := canaryHealth(dep, tt.job); got != tt.want {
				t.Errorf("canaryHealth = %d, want %d", got, tt.want)
			}
		})
	}
}

// jobWithJobLevelCanary has its update block on the job and none on its group,
// as a spec that was not canonicalized would.
func jobWithJobLevelCanary() *api.Job {
	canary, no := 1, false
	j := managed("web", "auto", nil, taskGroup("g", 1, false))
	j.Update = &api.UpdateStrategy{Canary: &canary, AutoPromote: &no}
	return j
}

func twoManual() *api.Job {
	a, b := taskGroup("a", 1, false), taskGroup("b", 1, false)
	canary, no := 1, false
	a.Update = &api.UpdateStrategy{Canary: &canary, AutoPromote: &no}
	b.Update = &api.UpdateStrategy{Canary: &canary, AutoPromote: &no}
	return managed("web", "auto", nil, a, b)
}

// -- Promote ---------------------------------------------------------------

// waiting is a deployment the apply step has seen waiting for a promotion.
func (h *harness) waiting(job *api.Job) *store.Deployment {
	h.t.Helper()
	d := h.waitingForPromotion(job)
	h.step(d)
	return h.get(d.ID)
}

func TestPromotePromotesTheCanariesAndRecordsWho(t *testing.T) {
	h := newHarness(t)
	d := h.waiting(canaryJob(false, nil))

	// The request is on record when Nomad is asked, not after.
	var eventsWhenAsked []store.Event
	h.nomad.onPromote = func() {
		eventsWhenAsked, _ = h.store.Events(context.Background(), d.ID)
	}
	if err := h.engine.Promote(context.Background(), d.ID, "iacopo"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(h.nomad.promoted) != 1 || h.nomad.promoted[0] != "dep-1" {
		t.Fatalf("promoted = %v, want the Nomad deployment dep-1 once", h.nomad.promoted)
	}
	if len(eventsWhenAsked) == 0 {
		t.Fatal("no events when Nomad was asked")
	}
	last := eventsWhenAsked[len(eventsWhenAsked)-1]
	if last.Actor != "iacopo" || last.Message != "promotion requested" {
		t.Errorf("last event when Nomad was asked = %+v, want the request by iacopo (invariant 7)", last)
	}

	// Nomad answered: the canaries are promoted, and the deployment says so
	// now, not at the next apply cycle, so the page the person is sent back to
	// no longer asks for a promotion.
	got := h.get(d.ID)
	if got.State != store.StateApplying || got.PromotedAt.IsZero() {
		t.Fatalf("after Promote: %+v, want applying with promoted_at set", got)
	}
}

// The apply loop then sees in Nomad the promotion Promote already recorded: it
// records it no second time, and the apply timeout counts from the promotion.
func TestPromoteThenTheApplyLoop(t *testing.T) {
	h := newHarness(t)
	d := h.waiting(canaryJob(false, nil))
	h.clock.Advance(time.Hour) // longer than the apply timeout, which does not run in the wait
	if err := h.engine.Promote(context.Background(), d.ID, "iacopo"); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	h.nomad.setDeployment("web", canaryDeployment(promotedCanary()))
	h.step(h.get(d.ID))
	if got := h.get(d.ID); got.State != store.StateApplying {
		t.Fatalf("after the apply step: state %s (%q), want still applying", got.State, got.Error)
	}
	evs, _ := h.store.Events(context.Background(), d.ID)
	promoted := 0
	for _, ev := range evs {
		if ev.Message == "canaries promoted in Nomad: the apply timeout counts again" {
			promoted++
		}
	}
	if promoted != 1 {
		t.Errorf("promotion recorded %d times, want once: %+v", promoted, evs)
	}
}

// The Nomad deployment can finish, and the apply loop move the deployment on,
// between Nomad promoting and Promote recording it: that is no error.
func TestPromoteAfterTheApplyLoopMovedOn(t *testing.T) {
	h := newHarness(t)
	d := h.waiting(canaryJob(false, nil))
	var logs bytes.Buffer
	h.engine.log = slog.New(slog.NewTextHandler(&logs, nil))
	h.nomad.onPromote = func() {
		h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusSuccessful})
		h.step(h.get(d.ID))
	}

	if err := h.engine.Promote(context.Background(), d.ID, "iacopo"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := h.get(d.ID); got.State != store.StateCompleted {
		t.Fatalf("state = %s, want completed", got.State)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("logged an error for a deployment that finished meanwhile:\n%s", logs.String())
	}
}

func TestPromoteKeepsTheRequestWhenNomadRefuses(t *testing.T) {
	h := newHarness(t)
	d := h.waiting(canaryJob(false, nil))
	h.nomad.promoteErr = errors.New("permission denied")

	err := h.engine.Promote(context.Background(), d.ID, "iacopo")
	if err == nil || !errors.Is(err, h.nomad.promoteErr) {
		t.Fatalf("Promote err = %v, want Nomad's error passed on", err)
	}
	evs, _ := h.store.Events(context.Background(), d.ID)
	if last := evs[len(evs)-1]; last.Message != "promotion requested" || last.Actor != "iacopo" {
		t.Errorf("last event = %+v, want the request to stay on record", last)
	}
	if got := h.get(d.ID); got.State != store.StateApplying || !got.PromotedAt.IsZero() {
		t.Errorf("deployment = %+v, want still applying and not promoted", got)
	}
}

func TestPromoteRefusals(t *testing.T) {
	ctx := context.Background()
	notWaiting := func(h *harness) *store.Deployment {
		// applying, canaries healthy in Nomad, but the apply step has not seen it yet
		return h.waitingForPromotion(canaryJob(false, nil))
	}
	for name, tt := range map[string]struct {
		prepare func(h *harness) *store.Deployment
		want    error
	}{
		"not seen waiting by the apply loop": {notWaiting, ErrNotWaitingForPromotion},
		"already promoted": {func(h *harness) *store.Deployment {
			d := h.waiting(canaryJob(false, nil))
			h.nomad.setDeployment("web", canaryDeployment(promotedCanary()))
			h.step(d)
			return h.get(d.ID)
		}, ErrNotWaitingForPromotion},
		"promoted in Nomad since the last cycle": {func(h *harness) *store.Deployment {
			d := h.waiting(canaryJob(false, nil))
			h.nomad.setDeployment("web", canaryDeployment(promotedCanary()))
			return d
		}, ErrNotWaitingForPromotion},
		"the Nomad deployment is another one": {func(h *harness) *store.Deployment {
			d := h.waiting(canaryJob(false, nil))
			dep := canaryDeployment(healthyCanary())
			dep.JobModifyIndex = 12
			h.nomad.setDeployment("web", dep)
			return d
		}, ErrNotWaitingForPromotion},
		"no Nomad deployment any more": {func(h *harness) *store.Deployment {
			d := h.waiting(canaryJob(false, nil))
			h.nomad.setDeployment("web", nil)
			return d
		}, ErrNotWaitingForPromotion},
		"the canaries are not healthy": {func(h *harness) *store.Deployment {
			d := h.waiting(canaryJob(false, nil))
			starting := healthyCanary()
			starting.HealthyAllocs = 0
			h.nomad.setDeployment("web", canaryDeployment(starting))
			return d
		}, ErrNotWaitingForPromotion},
		"not applying": {func(h *harness) *store.Deployment {
			d := h.waiting(canaryJob(false, nil))
			h.nomad.setDeployment("web", &api.Deployment{ID: "dep-1", JobModifyIndex: 9, Status: api.DeploymentStatusSuccessful})
			h.step(d)
			return h.get(d.ID)
		}, ErrNotWaitingForPromotion},
		"a namespace nops does not manage": {func(h *harness) *store.Deployment {
			d := h.waiting(canaryJob(false, nil))
			h.engine.managedNS = map[string]bool{"other": true}
			return d
		}, store.ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			d := tt.prepare(h)
			evsBefore, _ := h.store.Events(ctx, d.ID)

			if err := h.engine.Promote(ctx, d.ID, "iacopo"); !errors.Is(err, tt.want) {
				t.Fatalf("Promote err = %v, want %v", err, tt.want)
			}
			if len(h.nomad.promoted) != 0 {
				t.Errorf("Nomad was asked to promote %v", h.nomad.promoted)
			}
			if evsAfter, _ := h.store.Events(ctx, d.ID); len(evsAfter) != len(evsBefore) {
				t.Errorf("a refused Promote logged an event: %+v", evsAfter[len(evsBefore):])
			}
		})
	}

	h := newHarness(t)
	d := h.waiting(canaryJob(false, nil))
	if err := h.engine.Promote(ctx, d.ID, ""); err == nil {
		t.Error("Promote without an actor should fail")
	}
	if err := h.engine.Promote(ctx, "nope", "iacopo"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Promote of a missing deployment: err = %v, want ErrNotFound", err)
	}
	h.nomad.deployErr[nsKey(testNamespace, "web")] = errors.New("nomad is down")
	if err := h.engine.Promote(ctx, d.ID, "iacopo"); err == nil || errors.Is(err, ErrNotWaitingForPromotion) {
		t.Errorf("Promote while Nomad is down: err = %v, want Nomad's error, not a refusal", err)
	}
	if len(h.nomad.promoted) != 0 {
		t.Errorf("Nomad was asked to promote %v", h.nomad.promoted)
	}
}
