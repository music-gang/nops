package metrics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

type fakeStore struct {
	active []*store.Deployment
	err    error
}

func (f fakeStore) ListActive(context.Context) ([]*store.Deployment, error) { return f.active, f.err }

type fakeEngine struct {
	status  engine.Status
	applyAt time.Time
	obs     []engine.Observation
}

func (f fakeEngine) Status() engine.Status              { return f.status }
func (f fakeEngine) ApplySucceededAt() time.Time        { return f.applyAt }
func (f fakeEngine) Observations() []engine.Observation { return f.obs }

type fakeGit struct{ status gitwatch.Status }

func (f fakeGit) Status() gitwatch.Status { return f.status }

type fakeNotifier map[string]uint64

func (f fakeNotifier) Failures() map[string]uint64 { return f }

// at is a whole second, so its timestamp prints the same in any format.
func at(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func options() Options {
	return Options{
		Store:    fakeStore{},
		Engine:   fakeEngine{},
		Git:      fakeGit{},
		Notifier: fakeNotifier{},
		Version:  "v1.2.3",
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestCollect checks every nops metric against a state that has one of each
// case: a deployment per kind of wait, a job per kind of trouble.
func TestCollect(t *testing.T) {
	o := options()
	o.Git = fakeGit{gitwatch.Status{CheckedAt: at(1000), Error: "a later poll failed", ErrorAt: at(1100)}}
	o.Engine = fakeEngine{
		status:  engine.Status{At: at(2100), SucceededAt: at(2000), Error: "disk on fire", Unparsed: 2, Skipped: 1, Orphans: 3},
		applyAt: at(3000),
		obs: []engine.Observation{
			{Namespace: "apps", JobID: "web", Policy: meta.PolicyAuto, Drift: true},
			{Namespace: "apps", JobID: "db", Policy: meta.PolicyApproval, Hold: &engine.Hold{Kind: engine.HoldPaused}, BlockedBy: "01FAILED"},
			// A closed sync window is a hold, but not a pause.
			{Namespace: "infra", JobID: "proxy", Policy: meta.PolicyAuto, Hold: &engine.Hold{Kind: engine.HoldWindow}},
			{Namespace: "infra", JobID: "typo", Policy: meta.PolicyNone, Issues: []meta.Issue{{Severity: meta.SeverityError}, {Severity: meta.SeverityWarn}}},
			// An unknown key is a warning: the policy stands.
			{Namespace: "infra", JobID: "extra", Policy: meta.PolicyAuto, Issues: []meta.Issue{{Severity: meta.SeverityWarn}}},
		},
	}
	o.Store = fakeStore{active: []*store.Deployment{
		{Namespace: "apps", JobID: "db", State: store.StatePendingApproval, UpdatedAt: at(4000), CreatedAt: at(3900)},
		{Namespace: "apps", JobID: "web", State: store.StateApplying, PromotionWaitSince: at(5000)},
		// Promoted: no longer waiting for anyone.
		{Namespace: "infra", JobID: "proxy", State: store.StateApplying, PromotionWaitSince: at(5000), PromotedAt: at(5100)},
		{Namespace: "infra", JobID: "cache", State: store.StatePreHook},
	}}
	o.Notifier = fakeNotifier{"discord": 0, "ntfy": 4}

	const want = `
# HELP nops_build_info Always 1; the version label is the running build.
# TYPE nops_build_info gauge
nops_build_info{version="v1.2.3"} 1
# HELP nops_deployments Active deployments per state.
# TYPE nops_deployments gauge
nops_deployments{state="applying"} 2
nops_deployments{state="detected"} 0
nops_deployments{state="pending_approval"} 1
nops_deployments{state="post_hook"} 0
nops_deployments{state="pre_hook"} 1
# HELP nops_detection_skipped_jobs Managed jobs the last detection cycle could not plan because of a Nomad failure.
# TYPE nops_detection_skipped_jobs gauge
nops_detection_skipped_jobs 1
# HELP nops_job_blocked 1 if a failed or rejected deployment keeps nops from deploying the job's drift until a retry or a new commit.
# TYPE nops_job_blocked gauge
nops_job_blocked{job="db",namespace="apps"} 1
nops_job_blocked{job="extra",namespace="infra"} 0
nops_job_blocked{job="proxy",namespace="infra"} 0
nops_job_blocked{job="typo",namespace="infra"} 0
nops_job_blocked{job="web",namespace="apps"} 0
# HELP nops_job_drift 1 if the live job differs from the one in git.
# TYPE nops_job_drift gauge
nops_job_drift{job="db",namespace="apps"} 0
nops_job_drift{job="extra",namespace="infra"} 0
nops_job_drift{job="proxy",namespace="infra"} 0
nops_job_drift{job="typo",namespace="infra"} 0
nops_job_drift{job="web",namespace="apps"} 1
# HELP nops_job_info Always 1, one per managed job of the last detection cycle; the policy label is its effective policy.
# TYPE nops_job_info gauge
nops_job_info{job="db",namespace="apps",policy="approval"} 1
nops_job_info{job="extra",namespace="infra",policy="auto"} 1
nops_job_info{job="proxy",namespace="infra",policy="auto"} 1
nops_job_info{job="typo",namespace="infra",policy="none"} 1
nops_job_info{job="web",namespace="apps",policy="auto"} 1
# HELP nops_job_invalid_meta 1 if a nops_* meta key of the job has an invalid value, so nops reads its policy as none.
# TYPE nops_job_invalid_meta gauge
nops_job_invalid_meta{job="db",namespace="apps"} 0
nops_job_invalid_meta{job="extra",namespace="infra"} 0
nops_job_invalid_meta{job="proxy",namespace="infra"} 0
nops_job_invalid_meta{job="typo",namespace="infra"} 1
nops_job_invalid_meta{job="web",namespace="apps"} 0
# HELP nops_job_paused 1 if a person paused the job.
# TYPE nops_job_paused gauge
nops_job_paused{job="db",namespace="apps"} 1
nops_job_paused{job="extra",namespace="infra"} 0
nops_job_paused{job="proxy",namespace="infra"} 0
nops_job_paused{job="typo",namespace="infra"} 0
nops_job_paused{job="web",namespace="apps"} 0
# HELP nops_loop_last_success_timestamp_seconds When each loop last ran to the end: git (a poll reached the remote), detection (a cycle ended without error), apply (a cycle read the active deployments). Absent before the first one.
# TYPE nops_loop_last_success_timestamp_seconds gauge
nops_loop_last_success_timestamp_seconds{loop="apply"} 3000
nops_loop_last_success_timestamp_seconds{loop="detection"} 2000
nops_loop_last_success_timestamp_seconds{loop="git"} 1000
# HELP nops_notifications_failed_total Notifications an adapter failed to deliver since nops started.
# TYPE nops_notifications_failed_total counter
nops_notifications_failed_total{adapter="discord"} 0
nops_notifications_failed_total{adapter="ntfy"} 4
# HELP nops_orphan_jobs Jobs nops deployed that are gone from the repository but still run in Nomad.
# TYPE nops_orphan_jobs gauge
nops_orphan_jobs 3
# HELP nops_unparsed_files Files the last detection cycle could not parse, or that name a namespace nops does not manage.
# TYPE nops_unparsed_files gauge
nops_unparsed_files 2
# HELP nops_waiting_since_timestamp_seconds When a deployment started waiting for a person: an approval, or the promotion of its canaries.
# TYPE nops_waiting_since_timestamp_seconds gauge
nops_waiting_since_timestamp_seconds{job="db",namespace="apps",waiting="approval"} 4000
nops_waiting_since_timestamp_seconds{job="web",namespace="apps",waiting="canary_promotion"} 5000
`
	if err := testutil.CollectAndCompare(newCollector(o), strings.NewReader(want)); err != nil {
		t.Error(err)
	}
}

// TestCollectBeforeTheFirstCycle checks that nothing claims a loop ran, or
// counts a cycle, before one did: a zero timestamp would read as 1970 and
// fire every staleness alert at once.
func TestCollectBeforeTheFirstCycle(t *testing.T) {
	const want = `
# HELP nops_build_info Always 1; the version label is the running build.
# TYPE nops_build_info gauge
nops_build_info{version="v1.2.3"} 1
# HELP nops_deployments Active deployments per state.
# TYPE nops_deployments gauge
nops_deployments{state="applying"} 0
nops_deployments{state="detected"} 0
nops_deployments{state="pending_approval"} 0
nops_deployments{state="post_hook"} 0
nops_deployments{state="pre_hook"} 0
`
	if err := testutil.CollectAndCompare(newCollector(options()), strings.NewReader(want)); err != nil {
		t.Error(err)
	}
}

func get(t *testing.T, h http.Handler, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestHandlerServesNopsAndRuntimeMetrics checks the open endpoint: the nops
// metrics and the Go runtime and process collectors are there.
func TestHandlerServesNopsAndRuntimeMetrics(t *testing.T) {
	h, err := Handler(options())
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, name := range []string{"nops_build_info", "go_goroutines", "go_memstats_heap_alloc_bytes", "process_start_time_seconds"} {
		if !strings.Contains(body, "\n"+name) {
			t.Errorf("%s missing from /metrics", name)
		}
	}
}

// TestHandlerToken checks that a token, when set, is required, and that the
// right one gets through.
func TestHandlerToken(t *testing.T) {
	o := options()
	o.Token = "s3cret"
	h, err := Handler(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		authorization string
		want          int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"Bearer s3cret-and-more", http.StatusUnauthorized},
		{"Basic czNjcmV0", http.StatusUnauthorized},
		{"s3cret", http.StatusUnauthorized},
		{"Bearer s3cret", http.StatusOK},
	} {
		rec := get(t, h, tt.authorization)
		if rec.Code != tt.want {
			t.Errorf("Authorization %q: status %d, want %d", tt.authorization, rec.Code, tt.want)
		}
		if tt.want == http.StatusUnauthorized {
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("Authorization %q: a 401 without WWW-Authenticate", tt.authorization)
			}
			if strings.Contains(rec.Body.String(), "nops_") {
				t.Errorf("Authorization %q: the 401 carries metrics", tt.authorization)
			}
		}
	}
}

// TestHandlerFailsLoudOnAStoreError checks that a scrape that cannot read the
// store fails (so up == 0) and logs the error, rather than answering with a
// picture that is missing the deployments.
func TestHandlerFailsLoudOnAStoreError(t *testing.T) {
	o := options()
	o.Store = fakeStore{err: errors.New("disk on fire")}
	var logs bytes.Buffer
	o.Log = slog.New(slog.NewTextHandler(&logs, nil))
	h, err := Handler(o)
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "disk on fire") {
		t.Errorf("log = %s, want an ERROR with the store's error", logs.String())
	}
}

func TestHandlerRequiresItsDependencies(t *testing.T) {
	o := options()
	o.Notifier = nil
	if _, err := Handler(o); err == nil {
		t.Error("Handler without a Notifier: want an error")
	}
}
