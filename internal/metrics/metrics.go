// Package metrics serves /metrics in the Prometheus text format: whether the
// loops of nops are still working, the deployments that wait for a person, the
// state of every managed job and the failed notifications. The metrics, their
// labels and example alerts are documented in docs/metrics.md: keep the two
// aligned.
//
// Nothing is kept here: every scrape reads the active deployments from the
// store and the rest from the in-memory state the engine, the git watcher and
// the notifier already hold, so a metric never disagrees with the dashboard
// and a job that leaves the repository leaves the metrics with it.
package metrics

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// Store is what a scrape reads from SQLite. *store.Store implements it.
type Store interface {
	ListActive(ctx context.Context) ([]*store.Deployment, error)
}

// Engine is what a scrape reads from the engine's last cycles.
// *engine.Engine implements it.
type Engine interface {
	Status() engine.Status
	ApplySucceededAt() time.Time
	Observations() []engine.Observation
}

// Git is how the git watcher's polls went. *gitwatch.Watcher implements it.
type Git interface {
	Status() gitwatch.Status
}

// Notifier counts the notifications each adapter failed to deliver.
// *notify.Notifier implements it.
type Notifier interface {
	Failures() map[string]uint64
}

// Options configures Handler.
type Options struct {
	Store    Store
	Engine   Engine
	Git      Git
	Notifier Notifier
	// Version is the build, the label of nops_build_info.
	Version string
	// Token, when set, is what a scrape must send as Authorization: Bearer;
	// empty leaves /metrics open, like /healthz.
	Token string
	Log   *slog.Logger
}

// Handler returns the /metrics handler. It uses a registry of its own, never
// the global one, so what it exposes is exactly what is registered here: the
// nops collector, and the Go runtime and process collectors.
func Handler(o Options) (http.Handler, error) {
	if o.Store == nil || o.Engine == nil || o.Git == nil || o.Notifier == nil || o.Log == nil {
		return nil, errors.New("metrics: Store, Engine, Git, Notifier and Log are required")
	}
	reg := prometheus.NewRegistry()
	for _, c := range []prometheus.Collector{
		newCollector(o),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("metrics: %w", err)
		}
	}
	// HTTPErrorOnError: a scrape that cannot read the store fails (a 500, so
	// up == 0) rather than answering with a partial picture.
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:      errorLog{o.Log},
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
	if o.Token == "" {
		return h, nil
	}
	return requireToken(o.Token, h), nil
}

// requireToken lets a request through only with "Authorization: Bearer
// <token>", compared in constant time.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nops metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// errorLog logs what promhttp reports (a collector error, a failed write) at
// ERROR: a scrape that fails is a failure like any other.
type errorLog struct{ log *slog.Logger }

func (l errorLog) Println(v ...any) {
	l.log.Error("metrics scrape", "error", strings.TrimSpace(fmt.Sprintln(v...)))
}

// Loops whose last success nops_loop_last_success_timestamp_seconds reports.
const (
	loopGit       = "git"
	loopDetection = "detection"
	loopApply     = "apply"
)

// Values of the waiting label of nops_waiting_since_timestamp_seconds.
const (
	waitingApproval        = "approval"
	waitingCanaryPromotion = "canary_promotion"
)

// activeStates are the states nops_deployments always reports, 0 included, so
// an alert on one of them has a series to read before the first deployment.
var activeStates = []store.State{
	store.StateDetected, store.StatePendingApproval, store.StatePreHook, store.StateApplying, store.StatePostHook,
}

type collector struct {
	o Options

	buildInfo, loopSuccess, deployments, waitingSince *prometheus.Desc
	unparsedFiles, skippedJobs, orphanJobs            *prometheus.Desc
	jobInfo, jobDrift, jobPaused, jobBlocked, jobMeta *prometheus.Desc
	notificationsFailed                               *prometheus.Desc
}

func newCollector(o Options) *collector {
	job := []string{"namespace", "job"}
	return &collector{
		o: o,
		buildInfo: prometheus.NewDesc("nops_build_info",
			"Always 1; the version label is the running build.", []string{"version"}, nil),
		loopSuccess: prometheus.NewDesc("nops_loop_last_success_timestamp_seconds",
			"When each loop last ran to the end: git (a poll reached the remote), detection (a cycle ended without error), apply (a cycle read the active deployments). Absent before the first one.",
			[]string{"loop"}, nil),
		deployments: prometheus.NewDesc("nops_deployments",
			"Active deployments per state.", []string{"state"}, nil),
		waitingSince: prometheus.NewDesc("nops_waiting_since_timestamp_seconds",
			"When a deployment started waiting for a person: an approval, or the promotion of its canaries.",
			append(job, "waiting"), nil),
		unparsedFiles: prometheus.NewDesc("nops_unparsed_files",
			"Files the last detection cycle could not parse, or that name a namespace nops does not manage.", nil, nil),
		skippedJobs: prometheus.NewDesc("nops_detection_skipped_jobs",
			"Managed jobs the last detection cycle could not plan because of a Nomad failure.", nil, nil),
		orphanJobs: prometheus.NewDesc("nops_orphan_jobs",
			"Jobs nops deployed that are gone from the repository but still run in Nomad.", nil, nil),
		jobInfo: prometheus.NewDesc("nops_job_info",
			"Always 1, one per managed job of the last detection cycle; the policy label is its effective policy.",
			append(job, "policy"), nil),
		jobDrift: prometheus.NewDesc("nops_job_drift",
			"1 if the live job differs from the one in git.", job, nil),
		jobPaused: prometheus.NewDesc("nops_job_paused",
			"1 if a person paused the job.", job, nil),
		jobBlocked: prometheus.NewDesc("nops_job_blocked",
			"1 if a failed or rejected deployment keeps nops from deploying the job's drift until a retry or a new commit.", job, nil),
		jobMeta: prometheus.NewDesc("nops_job_invalid_meta",
			"1 if a nops_* meta key of the job has an invalid value, so nops reads its policy as none.", job, nil),
		notificationsFailed: prometheus.NewDesc("nops_notifications_failed_total",
			"Notifications an adapter failed to deliver since nops started.", []string{"adapter"}, nil),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.buildInfo, c.loopSuccess, c.deployments, c.waitingSince,
		c.unparsedFiles, c.skippedJobs, c.orphanJobs,
		c.jobInfo, c.jobDrift, c.jobPaused, c.jobBlocked, c.jobMeta,
		c.notificationsFailed,
	} {
		ch <- d
	}
}

// Collect reads everything anew. prometheus.Collector gives it no context;
// the store is a local SQLite file, so a read does not hang on the network.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	timestamp := func(loop string, t time.Time) {
		if !t.IsZero() {
			gauge(c.loopSuccess, unixSeconds(t), loop)
		}
	}

	gauge(c.buildInfo, 1, c.o.Version)

	st := c.o.Engine.Status()
	timestamp(loopGit, c.o.Git.Status().CheckedAt)
	timestamp(loopDetection, st.SucceededAt)
	timestamp(loopApply, c.o.Engine.ApplySucceededAt())

	if err := c.collectDeployments(gauge); err != nil {
		ch <- prometheus.NewInvalidMetric(c.deployments, err)
	}

	// The counts of a cycle are only known once one has run.
	if !st.At.IsZero() {
		gauge(c.unparsedFiles, float64(st.Unparsed))
		gauge(c.skippedJobs, float64(st.Skipped))
		gauge(c.orphanJobs, float64(st.Orphans))
	}

	for _, obs := range c.o.Engine.Observations() {
		ns, job := obs.Namespace, obs.JobID
		gauge(c.jobInfo, 1, ns, job, string(obs.Policy))
		gauge(c.jobDrift, boolValue(obs.Drift), ns, job)
		gauge(c.jobPaused, boolValue(obs.Hold != nil && obs.Hold.Kind == engine.HoldPaused), ns, job)
		gauge(c.jobBlocked, boolValue(obs.BlockedBy != ""), ns, job)
		gauge(c.jobMeta, boolValue(invalidMeta(obs.Issues)), ns, job)
	}

	for adapter, n := range c.o.Notifier.Failures() {
		ch <- prometheus.MustNewConstMetric(c.notificationsFailed, prometheus.CounterValue, float64(n), adapter)
	}
}

// collectDeployments reports the active deployments per state and the ones
// that wait for a person. An error from the store fails the whole scrape.
func (c *collector) collectDeployments(gauge func(*prometheus.Desc, float64, ...string)) error {
	active, err := c.o.Store.ListActive(context.Background())
	if err != nil {
		return fmt.Errorf("list active deployments: %w", err)
	}
	perState := make(map[store.State]int, len(activeStates))
	for _, d := range active {
		perState[d.State]++
		switch {
		case d.State == store.StatePendingApproval:
			// Nothing else writes a deployment while it waits for an
			// approval: UpdatedAt is when it started to.
			gauge(c.waitingSince, unixSeconds(d.UpdatedAt), d.Namespace, d.JobID, waitingApproval)
		case d.State == store.StateApplying && !d.PromotionWaitSince.IsZero() && d.PromotedAt.IsZero():
			gauge(c.waitingSince, unixSeconds(d.PromotionWaitSince), d.Namespace, d.JobID, waitingCanaryPromotion)
		}
	}
	for _, s := range activeStates {
		gauge(c.deployments, float64(perState[s]), string(s))
	}
	return nil
}

// invalidMeta says whether issues hold an error, the kind that makes the job's
// policy none; a warning (an unknown key) changes nothing.
func invalidMeta(issues []meta.Issue) bool {
	for _, i := range issues {
		if i.Severity == meta.SeverityError {
			return true
		}
	}
	return false
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
