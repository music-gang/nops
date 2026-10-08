package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// heldObservation is a job "web" held by its closed window, with drift.
func heldObservation(t *testing.T) engine.Observation {
	t.Helper()
	return engine.Observation{
		JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "web.nomad.hcl", ObservedAt: testNow,
		SpecHash: "h1", Drift: true, PlanDiff: diffJSON(t, sampleDiff()), Hold: windowHold(),
	}
}

func TestDeployNow(t *testing.T) {
	for _, c := range []struct {
		name, next, want string
	}{
		{"to the deployment it created", "d2", "/deployments/d2"},
		{"to the job when the cycle left the request to the loop", "", "/jobs/default/web"},
	} {
		t.Run(c.name, func(t *testing.T) {
			en := &fakeEngine{deployNowNext: c.next}
			ts := newTestServer(t, &fakeStore{}, en, "")
			cookie := ts.session("alice")

			rec := ts.do("POST", "/jobs/default/web/deploy-now", formBody(url.Values{"spec_hash": {"h1"}}), cookie)
			if got := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || got != c.want {
				t.Fatalf("status %d, Location %q, want 303 to %q", rec.Code, got, c.want)
			}
			if len(en.deployNowCalls) != 1 || en.deployNowCalls[0] != (deployNowCall{"default", "web", "h1", "alice"}) {
				t.Errorf("DeployNow calls = %+v, want one for default/web, spec h1, by alice", en.deployNowCalls)
			}
		})
	}
}

func TestDeployNowErrors(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		code int
		body string
	}{
		{"not deployable", engine.ErrNotDeployable, http.StatusConflict, "Nothing to deploy now"},
		{"unknown job", store.ErrNotFound, http.StatusNotFound, "This job does not exist."},
		{"anything else", errors.New("sqlite: disk on fire"), http.StatusInternalServerError, "it has been logged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestServer(t, &fakeStore{}, &fakeEngine{deployNowErr: c.err}, "")
			rec := ts.do("POST", "/jobs/default/web/deploy-now", nil, ts.session("alice"))
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.body) {
				t.Fatalf("status %d, want %d with %q; body: %s", rec.Code, c.code, c.body, rec.Body)
			}
			// Never the error text: it can quote SQLite.
			if strings.Contains(rec.Body.String(), "disk on fire") {
				t.Error("the response leaks the underlying error")
			}
		})
	}
}

func TestDeployNowRefusesCrossOriginAndNoLogin(t *testing.T) {
	en := &fakeEngine{}
	ts := newTestServer(t, &fakeStore{}, en, "")

	req := httptest.NewRequest("POST", "/jobs/default/web/deploy-now", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(ts.session("alice"))
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(en.deployNowCalls) != 0 {
		t.Errorf("cross-site: status %d with %d engine calls, want 403 and none", rec.Code, len(en.deployNowCalls))
	}

	if rec := ts.do("POST", "/jobs/default/web/deploy-now", nil); len(en.deployNowCalls) != 0 || rec.Code == http.StatusSeeOther && rec.Header().Get("Location") == "/jobs/default/web" {
		t.Errorf("without a login: status %d with %d engine calls, want none", rec.Code, len(en.deployNowCalls))
	}
}

func TestJobPageOffersDeployNowToAHeldJob(t *testing.T) {
	en := &fakeEngine{observations: []engine.Observation{heldObservation(t)}}
	page := newTestServer(t, &fakeStore{}, en, "").get("/jobs/default/web")

	mustContain(t, page, "Held:", `action="/jobs/default/web/deploy-now"`, `name="spec_hash" value="h1"`, "Deploy now")
}

// The button is for what a closed window holds and nothing else: a pause wins,
// a block needs a retry, there must be something to deploy and nothing running.
func TestJobPageDoesNotOfferDeployNow(t *testing.T) {
	run := func(t *testing.T, o engine.Observation, deps ...*store.Deployment) string {
		t.Helper()
		en := &fakeEngine{observations: []engine.Observation{o}}
		return newTestServer(t, &fakeStore{byJob: deps}, en, "").get("/jobs/default/web")
	}
	t.Run("paused", func(t *testing.T) {
		o := heldObservation(t)
		o.Hold = pausedHold()
		mustNotContain(t, run(t, o), "deploy-now", "Deploy now")
	})
	t.Run("nothing to deploy", func(t *testing.T) {
		o := heldObservation(t)
		o.Drift, o.PlanDiff = false, ""
		mustNotContain(t, run(t, o), "deploy-now", "Deploy now")
	})
	t.Run("blocked", func(t *testing.T) {
		o := heldObservation(t)
		o.BlockedBy, o.BlockedReason = "d1", "failed on the same live job; push a new commit or retry it"
		page := run(t, o, &store.Deployment{ID: "d1", State: store.StateFailed})
		mustContain(t, page, "Blocked.")
		mustNotContain(t, page, "deploy-now", "Deploy now")
	})
	t.Run("a deployment is running", func(t *testing.T) {
		mustNotContain(t, run(t, heldObservation(t), &store.Deployment{ID: "d1", State: store.StateApplying}), "deploy-now", "Deploy now")
	})
	t.Run("approval", func(t *testing.T) {
		o := heldObservation(t)
		o.Hold = nil // the window does not apply under approval
		mustNotContain(t, run(t, o), "deploy-now", "Deploy now")
	})
}

// The cycle a request asked for may not have run: the page says the request is
// waiting, rather than the stale state it had.
func TestJobPageSaysADeployNowWaits(t *testing.T) {
	o := heldObservation(t)
	o.DeployNowBy = "alice"
	page := newTestServer(t, &fakeStore{}, &fakeEngine{observations: []engine.Observation{o}}, "").get("/jobs/default/web")

	mustContain(t, page, "alice asked to deploy it now", "next detection cycle")
	mustNotContain(t, page, "deploy-now", ">Deploy now<")
}

// What a Deploy now started is not held: the window closing again does not show
// the job as held while it runs.
func TestJobPageOfADeploymentStartedByDeployNow(t *testing.T) {
	d := &store.Deployment{ID: "d1", JobID: "web", Namespace: "default", State: store.StateApplying, WindowLiftedBy: "alice"}
	en := &fakeEngine{observations: []engine.Observation{heldObservation(t)}}
	page := newTestServer(t, &fakeStore{byJob: []*store.Deployment{d}}, en, "").get("/jobs/default/web")

	mustNotContain(t, page, "Held:", "deploy-now")
	mustContain(t, page, "Deploying")
}

func TestDeploymentPageSaysWhoDeployedItNow(t *testing.T) {
	d := sampleDeployment()
	d.WindowLiftedBy = "alice"
	page := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "").get("/deployments/d1")
	mustContain(t, page, "Deployed now", "outside its sync window, asked by alice")

	d.WindowLiftedBy = ""
	mustNotContain(t, newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "").get("/deployments/d1"), "Deployed now")
}
