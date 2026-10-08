package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubMetrics stands in for internal/metrics, which checks its own token: the
// dashboard only has to route to it, without a session.
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
