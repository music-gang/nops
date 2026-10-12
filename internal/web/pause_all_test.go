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

// allHold is what the engine says of every job alice paused an hour ago.
func allHold() *engine.Hold {
	h := pausedHold()
	h.Reason, h.Note, h.All = "every job paused by alice: nomad upgrade", "nomad upgrade", true
	return h
}

func TestPauseAll(t *testing.T) {
	en := &fakeEngine{}
	ts := newTestServer(t, &fakeStore{}, en, "")

	rec := ts.do("POST", "/pause", formBody(url.Values{"reason": {"nomad upgrade"}}), ts.session("alice"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, Location %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if len(en.pauseAllCalls) != 1 || en.pauseAllCalls[0] != (pauseAllCall{"alice", "nomad upgrade"}) {
		t.Errorf("PauseAll calls = %+v, want one by alice with the reason", en.pauseAllCalls)
	}
}

func TestResumeAll(t *testing.T) {
	en := &fakeEngine{}
	ts := newTestServer(t, &fakeStore{}, en, "")

	rec := ts.do("POST", "/resume", nil, ts.session("bob"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, Location %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if len(en.resumeAllCalls) != 1 || en.resumeAllCalls[0] != "bob" {
		t.Errorf("ResumeAll calls = %+v, want one by bob", en.resumeAllCalls)
	}
}

func TestPauseAllAndResumeAllErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		action string
		en     *fakeEngine
		code   int
		body   string
	}{
		{"pause: already paused", "pause", &fakeEngine{pauseAllErr: store.ErrAlreadyPaused}, http.StatusConflict, "already paused"},
		{"pause: reason too long", "pause", &fakeEngine{pauseAllErr: engine.ErrPauseNoteTooLong}, http.StatusBadRequest, "Reason too long"},
		{"pause: anything else", "pause", &fakeEngine{pauseAllErr: errors.New("sqlite: disk on fire")}, http.StatusInternalServerError, "it has been logged"},
		{"resume: not paused", "resume", &fakeEngine{resumeAllErr: store.ErrNotPaused}, http.StatusConflict, "not paused"},
		{"resume: anything else", "resume", &fakeEngine{resumeAllErr: errors.New("sqlite: disk on fire")}, http.StatusInternalServerError, "it has been logged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestServer(t, &fakeStore{}, c.en, "")
			rec := ts.do("POST", "/"+c.action, nil, ts.session("alice"))
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.body) {
				t.Fatalf("status %d, want %d with %q; body: %s", rec.Code, c.code, c.body, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "disk on fire") {
				t.Error("the response leaks the underlying error")
			}
		})
	}
}

func TestPauseAllAndResumeAllRefuseCrossOrigin(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		en := &fakeEngine{}
		ts := newTestServer(t, &fakeStore{}, en, "")

		req := httptest.NewRequest("POST", "/"+action, nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(ts.session("alice"))
		rec := httptest.NewRecorder()
		ts.h.ServeHTTP(rec, req)

		if calls := len(en.pauseAllCalls) + len(en.resumeAllCalls); rec.Code != http.StatusForbidden || calls != 0 {
			t.Errorf("%s cross-site: status %d with %d engine calls, want 403 and none", action, rec.Code, calls)
		}
	}
}

// Every page says Nops is paused, since when and by whom, and has the Resume
// button; the Overview offers the Pause form only while it is not.
func TestEveryPageSaysThatNopsIsPaused(t *testing.T) {
	obs := engine.Observation{JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "web.nomad.hcl", ObservedAt: testNow, Hold: allHold()}
	en := &fakeEngine{allPause: allHold(), observations: []engine.Observation{obs}}
	ts := newTestServer(t, &fakeStore{}, en, "")
	cookie := ts.session("bob")

	for _, path := range []string{"/", "/jobs", "/history", "/jobs/default/web"} {
		body := ts.do("GET", path, nil, cookie).Body.String()
		for _, want := range []string{"Nops is paused", "alice", "nomad upgrade", `action="/resume"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: no %q in the page", path, want)
			}
		}
	}
	over := ts.do("GET", "/", nil, cookie).Body.String()
	if strings.Contains(over, `action="/pause"`) {
		t.Error("the Overview offers to pause while Nops is paused")
	}
	// One banner, not a line for each job: the job's pause is not listed.
	if strings.Contains(over, "Needs attention") && strings.Contains(over, "default/web") {
		t.Errorf("the Overview lists the job held by the pause of every job: %s", over)
	}
	// The job page has no Resume of its own: the banner lifts it.
	job := ts.do("GET", "/jobs/default/web", nil, cookie).Body.String()
	if strings.Count(job, `/resume"`) != 1 || !strings.Contains(job, "Every job is paused by alice") {
		t.Errorf("job page = %s, want the one Resume of the banner and the pause of every job named", job)
	}
}

func TestTheOverviewOffersToPauseEveryJob(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	body := ts.do("GET", "/", nil, ts.session("alice")).Body.String()
	if !strings.Contains(body, `action="/pause"`) || strings.Contains(body, "Nops is paused") {
		t.Errorf("Overview = %s, want the Pause form and no banner", body)
	}
}

// A job paused on its own while every job is paused keeps its own Resume.
func TestAJobPausedOnItsOwnKeepsItsResume(t *testing.T) {
	obs := engine.Observation{JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, FilePath: "web.nomad.hcl", ObservedAt: testNow, Hold: pausedHold()}
	en := &fakeEngine{allPause: allHold(), observations: []engine.Observation{obs}}
	ts := newTestServer(t, &fakeStore{}, en, "")

	job := ts.do("GET", "/jobs/default/web", nil, ts.session("bob")).Body.String()
	if !strings.Contains(job, `action="/jobs/default/web/resume"`) {
		t.Errorf("job page = %s, want the job's own Resume", job)
	}
}

func TestOnlyTheGlobalPauseCapabilityPausesEveryJob(t *testing.T) {
	en := &fakeEngine{}
	s := newACLServer(t, &fakeStore{}, en)
	reader := s.clientWith(`namespace "*" { policy = "write" }`)
	if rec := s.api("POST", "/api/pause", reader, ``); rec.Code != http.StatusForbidden {
		t.Errorf("write on every namespace paused every job: status %d, want 403", rec.Code)
	}
	if len(en.pauseAllCalls) != 0 {
		t.Errorf("PauseAll calls = %+v, want none", en.pauseAllCalls)
	}
	pauser := s.clientWith(`capabilities = ["pause"]`)
	if rec := s.api("POST", "/api/pause", pauser, `{"reason":"x"}`); rec.Code != http.StatusNoContent {
		t.Errorf("pause capability: status %d: %s", rec.Code, rec.Body)
	}
}
