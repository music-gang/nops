package web

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

func TestNewValidates(t *testing.T) {
	auth := newTestAuth(t)
	st := &fakeStore{}
	en := &fakeEngine{}
	log := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))

	tests := []struct {
		name string
		opts Options
	}{
		{"no auth", Options{Store: st, Engine: en, Log: log}},
		{"no store", Options{Auth: auth, Engine: en, Log: log}},
		{"no engine", Options{Auth: auth, Store: st, Log: log}},
		{"no log", Options{Auth: auth, Store: st, Engine: en}},
		{"webhook secret without trigger", Options{Auth: auth, Store: st, Engine: en, Log: log, WebhookSecret: "s"}},
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
// for the same reason).
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
	dc := deploymentCard{
		ID: "d1", JobID: "web", Namespace: "default", State: store.StatePendingApproval,
		StateLabel: stateLabel(store.StatePendingApproval), StateClass: stateClass(store.StatePendingApproval),
		CommitSHA: "abcdef1234567890", SpecHash: "hash", CreatedAt: "2026-01-01 00:00:00 UTC",
	}
	dd := deploymentDetailData{
		baseData: baseData{Actor: "alice"}, Deployment: dc, Diff: diff, CanDecide: true,
		Events:   []eventView{{Time: "now", From: store.StateDetected, To: store.StatePendingApproval, Actor: "nops", Msg: "drift"}},
		HookRuns: []hookRunView{{Phase: "pre", JobID: "hook", State: store.HookRunning, StartedAt: "now"}},
	}

	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]any{
		"index":           indexData{baseData: baseData{Nav: "pending"}, Pending: []deploymentCard{dc}, Active: []deploymentCard{dc}},
		"history":         historyData{baseData: baseData{Nav: "history"}, Deployments: []deploymentCard{dc}},
		"drift":           driftData{baseData: baseData{Nav: "drift"}, Observations: []observationView{{JobID: "web", Namespace: "default", Policy: meta.PolicyAuto, Drift: true, Diff: diff, Issues: []meta.Issue{{Severity: meta.SeverityWarn, Key: "k", Message: "m"}}}}},
		"deployment":      dd,
		"status_fragment": dd,
		"error":           errorData{baseData: baseData{}, Status: 404, Title: "Not found", Message: "gone"},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
				t.Fatalf("render %s: %v", name, err)
			}
			if buf.Len() == 0 {
				t.Fatalf("render %s: empty output", name)
			}
		})
	}
}
