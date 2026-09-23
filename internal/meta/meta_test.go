package meta

import (
	"reflect"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		in         map[string]string
		managed    bool
		policy     Policy
		pre, post  *Hook
		isHook     bool
		wantIssues []Issue
	}{
		{
			name:   "empty meta is unmanaged with policy none",
			in:     nil,
			policy: PolicyNone,
		},
		{
			name:    "managed without policy defaults to none",
			in:      map[string]string{KeyManaged: "true"},
			managed: true,
			policy:  PolicyNone,
		},
		{
			name:    "auto",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto"},
			managed: true,
			policy:  PolicyAuto,
		},
		{
			name:    "approval with hooks and timeouts",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "approval", KeyPreHook: "migrate", KeyPreHookTimeout: "10m", KeyPostHook: "smoke"},
			managed: true,
			policy:  PolicyApproval,
			pre:     &Hook{JobID: "migrate", Timeout: 10 * time.Minute},
			post:    &Hook{JobID: "smoke", Timeout: DefaultHookTimeout},
		},
		{
			name:    "explicit false",
			in:      map[string]string{KeyManaged: "false", KeyPolicy: "auto"},
			managed: false,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityWarn, KeyPolicy, "set but nops_managed is not \"true\": ignored"},
			},
		},
		{
			name:    "managed is case sensitive",
			in:      map[string]string{KeyManaged: "True", KeyPolicy: "auto"},
			managed: false,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyManaged, "invalid value \"True\" (want \"true\" or \"false\")"},
				{SeverityWarn, KeyPolicy, "set but nops_managed is not \"true\": ignored"},
			},
		},
		{
			name:    "invalid policy falls back to none",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "yolo"},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyPolicy, "invalid value \"yolo\" (want auto, approval or none)"},
			},
		},
		{
			name:    "invalid timeout forces policy none, hook kept with default timeout",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHook: "h", KeyPreHookTimeout: "soon"},
			managed: true,
			policy:  PolicyNone,
			pre:     &Hook{JobID: "h", Timeout: DefaultHookTimeout},
			wantIssues: []Issue{
				{SeverityError, KeyPreHookTimeout, "invalid duration \"soon\": time: invalid duration \"soon\""},
			},
		},
		{
			name:    "non positive timeout",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPostHook: "h", KeyPostHookTimeout: "0s"},
			managed: true,
			policy:  PolicyNone,
			post:    &Hook{JobID: "h", Timeout: DefaultHookTimeout},
			wantIssues: []Issue{
				{SeverityError, KeyPostHookTimeout, "duration \"0s\" must be positive"},
			},
		},
		{
			name:    "empty hook id is an error",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHook: ""},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyPreHook, "empty hook job ID"},
			},
		},
		{
			name:    "timeout without hook is only a warning",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHookTimeout: "1m"},
			managed: true,
			policy:  PolicyAuto,
			wantIssues: []Issue{
				{SeverityWarn, KeyPreHookTimeout, "set but nops_pre_hook is not declared: ignored"},
			},
		},
		{
			name:    "unknown key under prefix is a warning and does not change behaviour",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", "nops_polcy": "approval", "other": "x"},
			managed: true,
			policy:  PolicyAuto,
			wantIssues: []Issue{
				{SeverityWarn, "nops_polcy", "unknown key"},
			},
		},
		{
			name:   "hook role",
			in:     map[string]string{KeyRole: "hook"},
			policy: PolicyNone,
			isHook: true,
		},
		{
			name:   "invalid role",
			in:     map[string]string{KeyRole: "worker"},
			policy: PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyRole, "invalid value \"worker\" (want \"hook\")"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in)
			if got.Managed != tc.managed {
				t.Errorf("Managed = %v, want %v", got.Managed, tc.managed)
			}
			if got.Policy != tc.policy {
				t.Errorf("Policy = %q, want %q", got.Policy, tc.policy)
			}
			if !reflect.DeepEqual(got.PreHook, tc.pre) {
				t.Errorf("PreHook = %+v, want %+v", got.PreHook, tc.pre)
			}
			if !reflect.DeepEqual(got.PostHook, tc.post) {
				t.Errorf("PostHook = %+v, want %+v", got.PostHook, tc.post)
			}
			if got.IsHook != tc.isHook {
				t.Errorf("IsHook = %v, want %v", got.IsHook, tc.isHook)
			}
			if !reflect.DeepEqual(got.Issues, tc.wantIssues) {
				t.Errorf("Issues = %+v, want %+v", got.Issues, tc.wantIssues)
			}
		})
	}
}

func TestHasErrors(t *testing.T) {
	if Parse(map[string]string{KeyManaged: "true", KeyPolicy: "auto"}).HasErrors() {
		t.Error("valid config reports errors")
	}
	if !Parse(map[string]string{KeyManaged: "true", KeyPolicy: "x"}).HasErrors() {
		t.Error("invalid policy does not report errors")
	}
	if Parse(map[string]string{"nops_typo": "x"}).HasErrors() {
		t.Error("warning-only config reports errors")
	}
}
