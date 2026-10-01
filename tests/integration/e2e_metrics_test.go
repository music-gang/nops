//go:build integration

package integration

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// scrape GETs /metrics with the given Authorization header (empty: none).
func scrape(t *testing.T, base, authorization string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// TestE2EMetrics scrapes the real binary, with a metrics token, while a job
// waits for an approval: the token is required, and the scrape shows the
// build, the three loops, the job and the deployment waiting for a person,
// next to the Go runtime's own metrics.
func TestE2EMetrics(t *testing.T) {
	const token = "metrics-e2e-token"
	e := newE2E(t, "NOPS_METRICS_TOKEN="+token)
	jobID := uniqueID(t, e.raw, "metrics")

	if status, body := scrape(t, e.proc.baseURL, ""); status != http.StatusUnauthorized {
		t.Fatalf("/metrics without the token: status %d, want 401: %s", status, body)
	}
	if status, _ := scrape(t, e.proc.baseURL, "Bearer wrong"); status != http.StatusUnauthorized {
		t.Fatalf("/metrics with a wrong token: status %d, want 401", status)
	}

	e.repo.commit(t, "job "+jobID, map[string]string{file(jobID): e2eJob{id: jobID, policy: "approval", version: "1"}.hcl()})
	e.waitNew(jobID, "", store.StatePendingApproval)

	want := []string{
		`nops_build_info{version="` + testVersion + `"} 1`,
		`nops_loop_last_success_timestamp_seconds{loop="git"} `,
		`nops_loop_last_success_timestamp_seconds{loop="detection"} `,
		`nops_loop_last_success_timestamp_seconds{loop="apply"} `,
		`nops_deployments{state="pending_approval"} 1`,
		`nops_waiting_since_timestamp_seconds{job="` + jobID + `",namespace="default",waiting="approval"} `,
		`nops_job_info{job="` + jobID + `",namespace="default",policy="approval"} 1`,
		`nops_job_drift{job="` + jobID + `",namespace="default"} 1`,
		"go_goroutines ",
		"process_start_time_seconds ",
	}
	var missing []string
	deadline := time.Now().Add(e2eWait)
	for {
		status, body := scrape(t, e.proc.baseURL, "Bearer "+token)
		if status != http.StatusOK {
			t.Fatalf("/metrics with the token: status %d: %s", status, body)
		}
		missing = missing[:0]
		for _, w := range want {
			if !strings.Contains(body, "\n"+w) {
				missing = append(missing, w)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/metrics misses, after %s:\n%s\n\nbody:\n%s", e2eWait, strings.Join(missing, "\n"), body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
