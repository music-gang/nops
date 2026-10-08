package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// TestJobPathWithBasePath is the simplest check that a base path is baked
// into every computed link: the templates only prepend {{.Base}} to their
// own hard-coded ones (docs/running-nops.md#under-a-sub-path).
func TestJobPathWithBasePath(t *testing.T) {
	s := &server{basePath: "/nops"}
	if got := s.jobPath("default", "web"); got != "/nops/jobs/default/web" {
		t.Errorf("jobPath = %q, want the base path prefixed", got)
	}
}

// newBasePathTestServer wires the same fixtures as newTestServer, but with
// the dashboard served under /nops: an Auth whose session cookies and
// redirects carry that base path too, matching what cmd/nops passes both
// web.New and the login backend from the same config.Config.BasePath.
func newBasePathTestServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens := openTestStore(t)
	auth, err := NewAuth(AuthOptions{
		Issuer: "https://idp.test", ClientID: "nops", ClientSecret: "secret",
		RedirectURL: "http://nops.test/nops/auth/callback",
		BasePath:    "/nops",
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{
		Auth: auth, Store: &fakeStore{}, Engine: &fakeEngine{}, Git: &fakeGit{}, Access: tokens,
		BasePath: "/nops",
		Now:      func() time.Time { return testNow },
		Log:      log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, tokens
}

// TestBasePathStripsPrefixFromIncomingRequests covers the whole mechanism a
// sub path deployment needs (docs/running-nops.md#under-a-sub-path): the prefix is
// stripped from incoming requests, /healthz stays reachable with or without
// it, and every generated link, form action and static asset address in the
// page carries it.
func TestBasePathStripsPrefixFromIncomingRequests(t *testing.T) {
	h, tokens := newBasePathTestServer(t)
	sess := mintSession(t, tokens, false, "alice")

	do := func(method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := do("GET", "/nops/", sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /nops/: status %d, body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/nops/jobs"`, `href="/nops/history"`, `action="/nops/auth/logout"`, `href="/nops/static/app.css`,
		`href="/nops/static/favicon-32x32.png`, `src="/nops/static/logo-96x96.png`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}

	if rec := do("GET", "/"); rec.Code != http.StatusNotFound {
		t.Errorf("GET / without the prefix: status %d, want 404", rec.Code)
	}

	// An orchestrator's health check hits the task's own port directly,
	// bypassing whatever prefix a reverse proxy mounts the dashboard under:
	// /healthz must answer either way.
	if rec := do("GET", "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz (bare): status %d", rec.Code)
	}
	if rec := do("GET", "/nops/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /nops/healthz (prefixed): status %d", rec.Code)
	}

	// Unauthenticated: sent to the prefixed login, remembering the stripped
	// path to return to (next is relative to the mux, without the prefix;
	// the login backend prepends it back once it redirects there).
	rec = do("GET", "/nops/jobs")
	if rec.Code != http.StatusFound {
		t.Fatalf("unauthenticated GET /nops/jobs: status %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/nops/auth/login?next=%2Fjobs" {
		t.Errorf("Location = %q", loc)
	}
}
