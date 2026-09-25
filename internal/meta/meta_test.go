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
		pre, post  []string
		isHook     bool
		timeout    time.Duration
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
			name:    "approval with a pre-hook and a post-hook",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "approval", KeyPreHook: "migrate", KeyPostHook: "smoke"},
			managed: true,
			policy:  PolicyApproval,
			pre:     []string{"migrate"},
			post:    []string{"smoke"},
		},
		{
			name:    "several hooks run in the order they are listed, spaces trimmed",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHook: "backup, migrate ,warm-cache", KeyPostHook: "smoke,notify"},
			managed: true,
			policy:  PolicyAuto,
			pre:     []string{"backup", "migrate", "warm-cache"},
			post:    []string{"smoke", "notify"},
		},
		{
			name:    "the same hook may be in both phases",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHook: "check", KeyPostHook: "check"},
			managed: true,
			policy:  PolicyAuto,
			pre:     []string{"check"},
			post:    []string{"check"},
		},
		{
			name:    "the same hook twice in a phase is an error",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHook: "backup,migrate,backup"},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyPreHook, "hook job \"backup\" is listed twice"},
			},
		},
		{
			name:    "an empty item is an error",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPostHook: "smoke,,notify"},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyPostHook, "empty item in the list of hook job IDs \"smoke,,notify\""},
			},
		},
		{
			name:    "a trailing comma is an error",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHook: "backup,"},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyPreHook, "empty item in the list of hook job IDs \"backup,\""},
			},
		},
		{
			name:    "only blanks is an empty hook ID",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHook: "  "},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyPreHook, "empty hook job ID"},
			},
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
			name:    "empty hook id is an error",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHook: ""},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyPreHook, "empty hook job ID"},
			},
		},
		{
			name:    "the old per-phase timeouts are removed, and say what replaces them",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyPreHook: "h", "nops_pre_hook_timeout": "10m", "nops_post_hook_timeout": "1m"},
			managed: true,
			policy:  PolicyAuto,
			pre:     []string{"h"},
			wantIssues: []Issue{
				{SeverityWarn, "nops_post_hook_timeout", "removed: set nops_timeout on the hook job"},
				{SeverityWarn, "nops_pre_hook_timeout", "removed: set nops_timeout on the hook job"},
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
			name:    "hook role has the default timeout",
			in:      map[string]string{KeyRole: "hook"},
			policy:  PolicyNone,
			isHook:  true,
			timeout: DefaultHookTimeout,
		},
		{
			name:    "hook with its own timeout",
			in:      map[string]string{KeyRole: "hook", KeyTimeout: "20m"},
			policy:  PolicyNone,
			isHook:  true,
			timeout: 20 * time.Minute,
		},
		{
			name:    "invalid hook timeout keeps the default",
			in:      map[string]string{KeyRole: "hook", KeyTimeout: "soon"},
			policy:  PolicyNone,
			isHook:  true,
			timeout: DefaultHookTimeout,
			wantIssues: []Issue{
				{SeverityError, KeyTimeout, "invalid duration \"soon\": time: invalid duration \"soon\""},
			},
		},
		{
			name:    "non positive hook timeout keeps the default",
			in:      map[string]string{KeyRole: "hook", KeyTimeout: "0s"},
			policy:  PolicyNone,
			isHook:  true,
			timeout: DefaultHookTimeout,
			wantIssues: []Issue{
				{SeverityError, KeyTimeout, "duration \"0s\" must be positive"},
			},
		},
		{
			name:    "a timeout on a job that is not a hook is ignored with a warning",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyTimeout: "1m"},
			managed: true,
			policy:  PolicyAuto,
			wantIssues: []Issue{
				{SeverityWarn, KeyTimeout, "set but nops_role is not \"hook\": ignored"},
			},
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
			if !reflect.DeepEqual(got.PreHooks, tc.pre) {
				t.Errorf("PreHooks = %+v, want %+v", got.PreHooks, tc.pre)
			}
			if !reflect.DeepEqual(got.PostHooks, tc.post) {
				t.Errorf("PostHooks = %+v, want %+v", got.PostHooks, tc.post)
			}
			if got.Timeout != tc.timeout {
				t.Errorf("Timeout = %s, want %s", got.Timeout, tc.timeout)
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
