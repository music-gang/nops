package web

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/store"
)

func TestRetry(t *testing.T) {
	en := &fakeEngine{}
	ts := newTestServer(t, &fakeStore{}, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("POST", "/jobs/default/web/retry", nil, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, Location %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if len(en.retryCalls) != 1 || en.retryCalls[0] != (retryCall{"default", "web", "alice"}) {
		t.Errorf("Retry calls = %+v, want one for default/web by alice", en.retryCalls)
	}
}

// The page a retry returns to is chosen by name from a fixed list: whatever
// else the form carries (a URL, a path, a "next") never reaches the Location
// header.
func TestRetryReturnsToANamedPage(t *testing.T) {
	blocked := engine.Observation{JobID: "web", Namespace: "default", BlockedBy: "d1"}
	for back, want := range map[string]string{
		"":         "/",
		"overview": "/",
		"jobs":     "/jobs",
		"activity": "/history",
		"job":      "/jobs/default/web",
		// not names: never taken as a destination
		"/jobs":               "/",
		"https://evil.test":   "/",
		"//evil.test":         "/",
		"/\\evil.test":        "/",
		"javascript:alert(1)": "/",
		"JOBS":                "/",
	} {
		ts := newTestServer(t, &fakeStore{}, &fakeEngine{observations: []engine.Observation{blocked}}, "")
		cookie := mintSession(t, ts.auth, "alice")
		rec := ts.do("POST", "/jobs/default/web/retry", formBody(url.Values{"back": {back}, "next": {back}, "url": {back}}), cookie)
		if got := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || got != want {
			t.Errorf("back=%q: status %d, Location %q, want 303 to %q", back, rec.Code, got, want)
		}
	}
}

// "job" is the job's page, built from the engine's own copy of its name: a job
// the engine does not know goes to the Overview rather than to a path made of
// the request's.
func TestRetryBackToAJobTheEngineDoesNotKnow(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	cookie := mintSession(t, ts.auth, "alice")
	rec := ts.do("POST", "/jobs/default/..%2F..%2Fevil/retry", formBody(url.Values{"back": {"job"}}), cookie)
	if got := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || got != "/" {
		t.Errorf("status %d, Location %q, want 303 to /", rec.Code, got)
	}
}

func TestFetchNowAlwaysGoesToTheOverview(t *testing.T) {
	for _, v := range []string{"", "/jobs", "https://evil.test", "//evil.test"} {
		ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
		cookie := mintSession(t, ts.auth, "alice")
		rec := ts.do("POST", "/fetch", formBody(url.Values{"back": {v}, "next": {v}}), cookie)
		if got := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || got != "/" {
			t.Errorf("form value %q: status %d, Location %q, want 303 to /", v, rec.Code, got)
		}
	}
}

func TestRetryErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		body string
	}{
		{"not blocked", engine.ErrNotBlocked, http.StatusConflict, "Nothing to retry"},
		{"already retried", store.ErrAlreadyRetried, http.StatusConflict, "Nothing to retry"},
		{"unknown job", store.ErrNotFound, http.StatusNotFound, "This job does not exist."},
		{"anything else", errors.New("sqlite: disk on fire"), http.StatusInternalServerError, "it has been logged"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			en := &fakeEngine{retryErr: c.err}
			ts := newTestServer(t, &fakeStore{}, en, "")
			cookie := mintSession(t, ts.auth, "alice")

			rec := ts.do("POST", "/jobs/default/web/retry", nil, cookie)
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.body) {
				t.Fatalf("status %d, want %d with %q; body: %s", rec.Code, c.code, c.body, rec.Body)
			}
			// Never the error text: it can quote SQLite or Nomad.
			if strings.Contains(rec.Body.String(), "disk on fire") {
				t.Error("the response leaks the underlying error")
			}
		})
	}
}

func TestRetryServerErrorIsLogged(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{retryErr: errors.New("disk on fire")}, "")
	cookie := mintSession(t, ts.auth, "alice")
	ts.do("POST", "/jobs/default/web/retry", nil, cookie)
	if logs := ts.logs.String(); !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "disk on fire") {
		t.Errorf("expected the failure at ERROR, got: %s", logs)
	}
}

func TestRetryRefusesCrossOrigin(t *testing.T) {
	en := &fakeEngine{}
	ts := newTestServer(t, &fakeStore{}, en, "")
	cookie := mintSession(t, ts.auth, "alice")

	req := httptest.NewRequest("POST", "/jobs/default/web/retry", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || len(en.retryCalls) != 0 {
		t.Errorf("status %d with %d Retry calls, want 403 and none", rec.Code, len(en.retryCalls))
	}
}

func TestFetchNow(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("POST", "/fetch", nil, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d, Location %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	if ts.trig != 1 {
		t.Errorf("Trigger called %d times, want 1", ts.trig)
	}
}

func TestFetchNowRefusesCrossOrigin(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	cookie := mintSession(t, ts.auth, "alice")

	req := httptest.NewRequest("POST", "/fetch", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || ts.trig != 0 {
		t.Errorf("status %d, Trigger called %d times, want 403 and none", rec.Code, ts.trig)
	}
}

// Without a Trigger there is nothing to call: the route is not served.
func TestFetchNowNotServedWithoutTrigger(t *testing.T) {
	a := newTestAuth(t)
	h, err := New(Options{Auth: a, Store: &fakeStore{}, Engine: &fakeEngine{}, Git: &fakeGit{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/fetch", nil)
	req.AddCookie(mintSession(t, a, "alice"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
}
