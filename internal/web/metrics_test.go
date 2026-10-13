package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/music-gang/nops/internal/secret"
	"github.com/music-gang/nops/internal/store"
)

// stubMetrics stands in for internal/metrics: the dashboard only has to route
// to it, without a session.
var stubMetrics = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "nops_build_info 1\n")
})

func newMetricsTestServer(t *testing.T, basePath string, metrics http.Handler) http.Handler {
	t.Helper()
	h, err := New(Options{
		Auth: newTestAuth(t), Store: &fakeStore{}, Engine: &fakeEngine{}, Git: &fakeGit{}, Access: openTestStore(t),
		Metrics: metrics, BasePath: basePath,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func getStatus(h http.Handler, target string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code, rec.Body.String()
}

// TestMetricsNeedNoSession checks that /metrics is served without a session,
// with the headers every response gets, and that without a metrics handler
// there is no /metrics at all.
func TestMetricsNeedNoSession(t *testing.T) {
	h := newMetricsTestServer(t, "", stubMetrics)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "nops_build_info 1\n" {
		t.Fatalf("GET /metrics without a session: status %d, body %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") == "" {
		t.Errorf("GET /metrics misses the security headers: %v", rec.Header())
	}

	if code, _ := getStatus(newMetricsTestServer(t, "", nil), "/metrics"); code != http.StatusNotFound {
		t.Errorf("GET /metrics without a metrics handler: status %d, want 404", code)
	}
}

// TestMetricsUnderABasePath checks that, like /healthz, /metrics answers at the
// bare path as well as under the prefix: Prometheus scrapes the task's own
// port, not the reverse proxy that mounts the prefix.
func TestMetricsUnderABasePath(t *testing.T) {
	h := newMetricsTestServer(t, "/nops", stubMetrics)
	for _, target := range []string{"/metrics", "/nops/metrics"} {
		if code, body := getStatus(h, target); code != http.StatusOK || body != "nops_build_info 1\n" {
			t.Errorf("GET %s: status %d, body %q", target, code, body)
		}
	}
	if code, _ := getStatus(h, "/healthz"); code != http.StatusOK {
		t.Errorf("GET /healthz (bare): status %d", code)
	}

	h = newMetricsTestServer(t, "/nops", nil)
	for _, target := range []string{"/metrics", "/nops/metrics"} {
		if code, _ := getStatus(h, target); code != http.StatusNotFound {
			t.Errorf("GET %s without a metrics handler: status %d, want 404", target, code)
		}
	}
}

// TestMetricsWithTheACL checks that, with the ACL on, a scrape needs a token
// with the metrics capability or a management one, and is refused like /api/:
// 401 without a valid token, 403 with one that lacks the capability. A login
// has no bearer token, so it does not scrape.
func TestMetricsWithTheACL(t *testing.T) {
	boot := secret.New()
	s := aclServer{newTestServerLogin(t, &fakeStore{}, &fakeEngine{}, "", nil, "", nil, func(o *Options) {
		o.ACL, o.BootstrapToken, o.Metrics = true, boot, stubMetrics
	}), boot}
	s.policy("scrapers", `capabilities = ["metrics"]`)
	s.policy("operators", "namespace \"*\" {\n  policy = \"write\"\n}\ncapabilities = [\"fetch\", \"backup\", \"pause\"]")
	scraper := s.token(tokenBody{Name: "prometheus", Type: store.TokenClient, Policies: []string{"scrapers"}})
	operator := s.token(tokenBody{Name: "op", Type: store.TokenClient, Policies: []string{"operators"}})
	manager := s.token(tokenBody{Name: "root", Type: store.TokenManagement})

	for name, tt := range map[string]struct {
		token string
		want  int
	}{
		"no token":                   {"", http.StatusUnauthorized},
		"an unknown token":           {"nope", http.StatusUnauthorized},
		"the metrics capability":     {scraper, http.StatusOK},
		"every namespace capability": {operator, http.StatusForbidden},
		"a management token":         {manager, http.StatusOK},
		"the bootstrap token":        {boot, http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if tt.token != "" {
			req.Header.Set("Authorization", "Bearer "+tt.token)
		}
		rec := httptest.NewRecorder()
		s.h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s: status %d, want %d", name, rec.Code, tt.want)
		}
		if tt.want != http.StatusOK && rec.Body.String() == "nops_build_info 1\n" {
			t.Errorf("%s: the refusal carries metrics", name)
		}
	}

	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil).WithContext(s.t.Context()))
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("a 401 without WWW-Authenticate")
	}

	// A session cookie is not a bearer token.
	cookie := s.clientSession("alice", `capabilities = ["metrics"]`)
	if rec := s.do(http.MethodGet, "/metrics", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("a session cookie: status %d, want 401", rec.Code)
	}
}
