package web

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

func TestNewValidates(t *testing.T) {
	auth := newTestAuth(t)
	st := &fakeStore{}
	en := &fakeEngine{}
	git := &fakeGit{}
	log := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))

	tests := []struct {
		name string
		opts Options
	}{
		{"no auth", Options{Store: st, Engine: en, Git: git, Log: log}},
		{"no store", Options{Auth: auth, Engine: en, Git: git, Log: log}},
		{"no engine", Options{Auth: auth, Store: st, Git: git, Log: log}},
		{"no git", Options{Auth: auth, Store: st, Engine: en, Log: log}},
		{"no log", Options{Auth: auth, Store: st, Engine: en, Git: git}},
		{"webhook secret without trigger", Options{Auth: auth, Store: st, Engine: en, Git: git, Log: log, WebhookSecret: "s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil {
				t.Error("want an error")
			}
		})
	}
}

// TestTemplatesRenderEveryPage is a render smoke test for every page and
// fragment template with representative data, including a diff four levels
// deep (job, task group, task, field) and a redacted value: a broken
// template or a typo in a field name fails here, at test time, rather than
// on the first real request (New already parses every template at startup
// for the same reason). The data is the fullest each page can have, so every
// branch of a template that needs a field is exercised; the pages' content is
// checked by dashboard_test.go.
func TestTemplatesRenderEveryPage(t *testing.T) {
	diff := &api.JobDiff{
		Fields: []*api.FieldDiff{
			{Type: "Edited", Name: "Meta[api_token]", Old: "<redacted>", New: "<redacted>"},
			{Type: "None", Name: "Priority", Old: "50", New: "50"},
		},
		TaskGroups: []*api.TaskGroupDiff{
			{Type: "Edited", Name: "g", Tasks: []*api.TaskDiff{
				{Type: "Edited", Name: "t", Objects: []*api.ObjectDiff{
					{Type: "Edited", Name: "Service", Fields: []*api.FieldDiff{
						{Type: "Added", Name: "Env[FOO]", Old: "", New: "bar"},
					}},
				}},
			}},
		},
	}
	tv := timeView{Rel: "3m ago", Full: "2026-01-01 00:00:00 UTC", ISO: "2026-01-01T00:00:00Z"}
	dc := deploymentCard{
		ID: "d1", JobID: "web", Namespace: "default", Title: "default/web", JobPath: "/jobs/default/web", Path: "/deployments/d1",
		Policy: store.PolicyApproval, State: store.StatePendingApproval,
		StateLabel: stateLabel(store.StatePendingApproval), StateClass: stateClass(store.StatePendingApproval),
		Error: "boom", CommitSHA: "abcdef1234567890", CommitShort: "abcdef1", CommitURL: "https://git.test/c/abcdef1",
		CommitSubj: "subject", CommitAuthor: "me", SpecHash: "hash", SpecShort: "hash", CASIndex: 7, EvalID: "eval", EvalShort: "eval",
		DecidedBy: "alice", DecidedAt: tv, RetriedBy: "alice", RetriedAt: tv, CreatedAt: tv, UpdatedAt: tv,
	}
	summary := summarize(diff)
	dd := deploymentDetailData{
		baseData: baseData{Actor: "alice"}, Deployment: dc, Diff: diff, Summary: summary, CanDecide: true, Notice: "review again",
		Steps: []planStep{{Kind: "pre", Job: "h", Timeout: "5m"}, {Kind: "register", Text: "Update"}, {Kind: "health"}, {Kind: "post", Job: "p", Timeout: "1m"}},
		Events: []eventView{
			{Time: tv, From: store.StateDetected, To: store.StatePendingApproval, Actor: "nops", Msg: "drift"},
			{Time: tv, From: store.StateFailed, To: store.StateFailed, Actor: "alice", Msg: "retry requested", Retry: true},
		},
		HookRuns: []hookRunView{{Phase: "pre", JobID: "hook", State: store.HookFailed, Error: "exit 1", StartedAt: tv, FinishedAt: tv}},
	}
	row := jobRow{
		Namespace: "default", JobID: "web", Title: "default/web", Path: "/jobs/default/web", Policy: meta.PolicyApproval,
		Sync: syncBlocked, SyncLabel: "Blocked", SyncClass: "state-failed", BlockedReason: "failed before", File: "web.nomad.hcl",
		Observed: tv, Last: &dc, Warnings: 2,
	}
	oob := dd
	oob.OOB = true

	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]any{
		"overview": overviewData{
			baseData: baseData{Actor: "alice", Nav: "overview"},
			Git: gitView{Known: true, SHA: "abc", Short: "abc", URL: "https://git.test/c/abc", Subject: "s", Author: "me", CommittedAt: tv,
				CheckedAt: tv, Error: "network is down", ErrorAt: tv, CanFetch: true},
			Cycle: cycleView{Ran: true, At: tv, Took: "1ms", Error: "disk", Managed: 2, Skipped: 1, Unparsed: 1, OrphanCheckSkipped: true},
			Attention: []attentionItem{
				{Kind: "pending", KindLabel: "Needs approval", KindClass: "state-pending", Title: "default/web", Path: "/deployments/d1", Detail: "d", When: tv},
				{Kind: "blocked", KindLabel: "Blocked", KindClass: "state-failed", Title: "default/web", Path: "/jobs/default/web", Detail: "d", When: tv, RetryPath: "/jobs/default/web/retry"},
				{Kind: "orphan", KindLabel: "Not in git", KindClass: "state-pending", Title: "default/old", Path: "/jobs/default/old", Detail: "d"},
			},
			InProgress: []deploymentCard{dc},
		},
		"jobs": jobsData{
			baseData: baseData{Nav: "jobs"}, Rows: []jobRow{row}, Total: 1,
			Filters: []filterLink{{Label: "All", Count: 1, Active: true}, {Key: "blocked", Label: "Blocked", Count: 1}},
		},
		"job": jobData{
			baseData: baseData{Nav: "jobs"}, Namespace: "default", JobID: "web", Title: "default/web", InRepo: true, Policy: meta.PolicyApproval,
			Sync: syncBlocked, SyncLabel: "Blocked", SyncClass: "state-failed", File: "web.nomad.hcl", Observed: tv,
			PreHooks: []hookView{{JobID: "a", Timeout: "5m0s"}}, PostHooks: []hookView{{JobID: "b", Timeout: "5m0s"}},
			Blocked: true, BlockedReason: "why", BlockedBy: "d1", RetryPath: "/jobs/default/web/retry",
			Drift: true, Diff: diff, Summary: summary,
			Issues:      []meta.Issue{{Severity: meta.SeverityError, Key: "k", Message: "m"}, {Severity: meta.SeverityWarn, Key: "k2", Message: "m2"}},
			Deployments: []deploymentCard{dc},
		},
		"job (not in the repository)": jobData{baseData: baseData{Nav: "jobs"}, Namespace: "default", JobID: "gone", Title: "default/gone"},
		"job (an orphan)": jobData{
			baseData: baseData{Nav: "jobs"}, Namespace: "default", JobID: "old", Title: "default/old", Policy: meta.PolicyAuto,
			Sync: syncOrphan, SyncLabel: "Not in git", SyncClass: "state-pending",
			Orphan:      &orphanView{NomadStatus: "running", StopCommand: "nomad job stop -namespace default old"},
			Deployments: []deploymentCard{dc},
		},
		"activity": activityData{
			baseData: baseData{Nav: "history"}, Total: 1, Limited: true,
			Days:    []activityDay{{Label: "Today", Rows: []deploymentCard{dc}}},
			Filters: []filterLink{{Label: "All", Count: 1, Active: true}},
		},
		"deployment":      dd,
		"status_fragment": oob,
		"error":           errorData{baseData: baseData{}, Status: 404, Title: "Not found", Message: "gone"},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			tmplName, _, _ := strings.Cut(name, " ")
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, tmplName, data); err != nil {
				t.Fatalf("render %s: %v", name, err)
			}
			if buf.Len() == 0 {
				t.Fatalf("render %s: empty output", name)
			}
		})
	}
}
