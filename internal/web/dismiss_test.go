package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/store"
)

func TestDismissFromTheJobPage(t *testing.T) {
	st, en := orphanFixture()
	ts := newTestServer(t, st, en, "")
	cookie := ts.session("alice")

	page := ts.get("/jobs/default/old")
	mustContain(t, page, `action="/jobs/default/old/dismiss"`, `name="deployment_id" value="d9"`, ">Dismiss<")

	rec := ts.do("POST", "/jobs/default/old/dismiss", formBody(url.Values{"deployment_id": {"d9"}}), cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/jobs/default/old" {
		t.Fatalf("status %d, Location %q, want 303 to the job's page", rec.Code, rec.Header().Get("Location"))
	}
	if len(en.dismissCalls) != 1 || en.dismissCalls[0] != (dismissCall{"default", "old", "d9", "alice"}) {
		t.Errorf("DismissOrphan calls = %+v, want one for default/old d9 by alice", en.dismissCalls)
	}
}

func TestDismissFromTheOverviewReturnsToIt(t *testing.T) {
	st, en := orphanFixture()
	ts := newTestServer(t, st, en, "")
	mustContain(t, ts.get("/"), `name="deployment_id" value="d9"`)

	rec := ts.do("POST", "/jobs/default/old/dismiss", formBody(url.Values{"deployment_id": {"d9"}, "back": {"overview"}}), ts.session("alice"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, Location %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
}

// The page of a dismissed job says who dismissed it, with the undo.
func TestDismissedJobPageOffersTheUndo(t *testing.T) {
	st, en := orphanFixture()
	en.orphans = nil
	st.dismissal = &store.Dismissal{Namespace: "default", JobID: "old", DeploymentID: "d9", DismissedBy: "alice", DismissedAt: testNow.Add(-time.Hour)}
	ts := newTestServer(t, st, en, "")

	page := ts.get("/jobs/default/old")
	mustContain(t, page, "Dismissed by alice", `action="/jobs/default/old/undismiss"`, ">Undo<")
	mustNotContain(t, page, "Removed from git, still running in Nomad")

	rec := ts.do("POST", "/jobs/default/old/undismiss", nil, ts.session("bob"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/jobs/default/old" {
		t.Fatalf("status %d, Location %q, want 303 to the job's page", rec.Code, rec.Header().Get("Location"))
	}
	if len(en.undismissCalls) != 1 || en.undismissCalls[0] != (resumeCall{"default", "old", "bob"}) {
		t.Errorf("UndismissOrphan calls = %+v, want one for default/old by bob", en.undismissCalls)
	}

	// The API says so too.
	rec = ts.api("GET", "/api/jobs/default/old", apiToken(t, ts, "carol", time.Time{}), "")
	var got apiJobDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Dismissed == nil || got.Dismissed.By != "alice" {
		t.Errorf("GET /api/jobs/default/old: dismissed = %+v, %v; body %s", got.Dismissed, err, rec.Body)
	}
}

func TestAPIOrphanCarriesTheDeploymentToDismiss(t *testing.T) {
	st, en := orphanFixture()
	ts := newTestServer(t, st, en, "")
	rec := ts.api("GET", "/api/jobs/default/old", apiToken(t, ts, "carol", time.Time{}), "")
	var got apiJobDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.OrphanDeploymentID != "d9" {
		t.Errorf("orphan_deployment_id = %q, %v, want d9; body %s", got.OrphanDeploymentID, err, rec.Body)
	}
}

func TestDismissAndUndismissErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		action string
		en     *fakeEngine
		code   int
		body   string
	}{
		{"dismiss: not an orphan", "dismiss", &fakeEngine{dismissErr: engine.ErrNotOrphan}, http.StatusConflict, "Not an orphan"},
		{"dismiss: already dismissed", "dismiss", &fakeEngine{dismissErr: store.ErrAlreadyDismissed}, http.StatusConflict, "already dismissed"},
		{"dismiss: unknown job", "dismiss", &fakeEngine{dismissErr: store.ErrNotFound}, http.StatusNotFound, "This job does not exist."},
		{"dismiss: anything else", "dismiss", &fakeEngine{dismissErr: errors.New("sqlite: disk on fire")}, http.StatusInternalServerError, "it has been logged"},
		{"undismiss: not dismissed", "undismiss", &fakeEngine{undismissErr: store.ErrNotDismissed}, http.StatusConflict, "not dismissed"},
		{"undismiss: unknown job", "undismiss", &fakeEngine{undismissErr: store.ErrNotFound}, http.StatusNotFound, "This job does not exist."},
		{"undismiss: anything else", "undismiss", &fakeEngine{undismissErr: errors.New("sqlite: disk on fire")}, http.StatusInternalServerError, "it has been logged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestServer(t, &fakeStore{}, c.en, "")
			rec := ts.do("POST", "/jobs/default/web/"+c.action, nil, ts.session("alice"))
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.body) {
				t.Fatalf("status %d, want %d with %q; body: %s", rec.Code, c.code, c.body, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "disk on fire") {
				t.Error("the response leaks the underlying error")
			}
		})
	}
}

func TestDismissAndUndismissRefuseCrossOriginAndNeedALogin(t *testing.T) {
	for _, action := range []string{"dismiss", "undismiss"} {
		en := &fakeEngine{}
		ts := newTestServer(t, &fakeStore{}, en, "")

		req := httptest.NewRequest("POST", "/jobs/default/web/"+action, nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(ts.session("alice"))
		rec := httptest.NewRecorder()
		ts.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s cross-site: status %d, want 403", action, rec.Code)
		}
		ts.do("POST", "/jobs/default/web/"+action, nil)
		if calls := len(en.dismissCalls) + len(en.undismissCalls); calls != 0 {
			t.Errorf("%s: %d engine calls cross-site or without a session, want none", action, calls)
		}
	}
}

func TestTheDashboardHidesDismissWithoutTheCapability(t *testing.T) {
	st, en := orphanFixture()
	read := dashboardAs(t, st, en, `namespace "*" { policy = "read" }`)
	mustContain(t, read.get("/jobs/default/old"), "Removed from git, still running in Nomad")
	mustNotContain(t, read.get("/jobs/default/old"), ">Dismiss<")
	mustNotContain(t, read.get("/"), ">Dismiss<")
	write := dashboardAs(t, st, en, `namespace "*" { policy = "write" }`)
	mustContain(t, write.get("/jobs/default/old"), ">Dismiss<")
	mustContain(t, write.get("/"), ">Dismiss<")

	dismissed, en2 := orphanFixture()
	en2.orphans = nil
	dismissed.dismissal = &store.Dismissal{Namespace: "default", JobID: "old", DeploymentID: "d9", DismissedBy: "alice", DismissedAt: testNow}
	read = dashboardAs(t, dismissed, en2, `namespace "*" { policy = "read" }`)
	mustContain(t, read.get("/jobs/default/old"), "Dismissed by alice")
	mustNotContain(t, read.get("/jobs/default/old"), ">Undo<")
	mustContain(t, dashboardAs(t, dismissed, en2, `namespace "*" { policy = "write" }`).get("/jobs/default/old"), ">Undo<")
}
