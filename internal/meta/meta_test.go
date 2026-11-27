package meta

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name            string
		in              map[string]string
		jobType         string
		managed         bool
		policy          Policy
		pre, post       []string
		isHook          bool
		timeout         time.Duration
		applyTimeout    time.Duration
		healthy         Healthy
		notifyCompleted bool
		wantIssues      []Issue
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
			name:            "a hook is not a managed job: the keys of one are ignored with a warning",
			in:              map[string]string{KeyRole: "hook", KeyManaged: "true", KeyPolicy: "auto", KeyNotifyCompleted: "true", KeyPreHook: "backup"},
			managed:         true,
			policy:          PolicyAuto,
			isHook:          true,
			timeout:         DefaultHookTimeout,
			pre:             []string{"backup"},
			notifyCompleted: true,
			wantIssues: []Issue{
				{SeverityWarn, KeyManaged, "set but nops_role is \"hook\": ignored"},
				{SeverityWarn, KeyNotifyCompleted, "set but nops_role is \"hook\": ignored"},
				{SeverityWarn, KeyPolicy, "set but nops_role is \"hook\": ignored"},
				{SeverityWarn, KeyPreHook, "set but nops_role is \"hook\": ignored"},
			},
		},
		{
			name:    "nops_managed on a hook is ignored with a warning whatever its value",
			in:      map[string]string{KeyRole: "hook", KeyManaged: "false"},
			policy:  PolicyNone,
			isHook:  true,
			timeout: DefaultHookTimeout,
			wantIssues: []Issue{
				{SeverityWarn, KeyManaged, "set but nops_role is \"hook\": ignored"},
			},
		},
		{
			name:    "a policy on a hook names the role, not nops_managed",
			in:      map[string]string{KeyRole: "hook", KeyPolicy: "auto"},
			policy:  PolicyNone,
			isHook:  true,
			timeout: DefaultHookTimeout,
			wantIssues: []Issue{
				{SeverityWarn, KeyPolicy, "set but nops_role is \"hook\": ignored"},
			},
		},
		{
			name:   "hooks on a job that is not managed are ignored with a warning",
			in:     map[string]string{KeyPreHook: "backup, migrate", KeyPostHook: "smoke"},
			policy: PolicyNone,
			pre:    []string{"backup", "migrate"},
			post:   []string{"smoke"},
			wantIssues: []Issue{
				{SeverityWarn, KeyPostHook, "set but nops_managed is not \"true\": ignored"},
				{SeverityWarn, KeyPreHook, "set but nops_managed is not \"true\": ignored"},
			},
		},
		{
			name:   "an invalid list of hooks on a job that is not managed is only an error",
			in:     map[string]string{KeyManaged: "false", KeyPostHook: "smoke,"},
			policy: PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyPostHook, "empty item in the list of hook job IDs \"smoke,\""},
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
		{
			name:            "opt in to a notification on completed",
			in:              map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyNotifyCompleted: "true"},
			managed:         true,
			policy:          PolicyAuto,
			notifyCompleted: true,
		},
		{
			name:    "explicit opt out is the same as not set",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyNotifyCompleted: "false"},
			managed: true,
			policy:  PolicyAuto,
		},
		{
			name:    "invalid notify-completed value falls back to none",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyNotifyCompleted: "yes"},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyNotifyCompleted, "invalid value \"yes\" (want \"true\" or \"false\")"},
			},
		},
		{
			name:            "notify-completed on an unmanaged job is a warning and still parses",
			in:              map[string]string{KeyNotifyCompleted: "true"},
			policy:          PolicyNone,
			notifyCompleted: true,
			wantIssues: []Issue{
				{SeverityWarn, KeyNotifyCompleted, "set but nops_managed is not \"true\": ignored"},
			},
		},
		{
			name:         "an apply timeout of its own",
			in:           map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyApplyTimeout: "2h"},
			managed:      true,
			policy:       PolicyAuto,
			applyTimeout: 2 * time.Hour,
		},
		{
			name:         "an apply timeout under the default is kept",
			in:           map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyApplyTimeout: "30s"},
			managed:      true,
			policy:       PolicyAuto,
			applyTimeout: 30 * time.Second,
		},
		{
			name:    "an invalid apply timeout falls back to none",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyApplyTimeout: "2 hours"},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyApplyTimeout, "invalid duration \"2 hours\": time: unknown unit \" hours\" in duration \"2 hours\""},
			},
		},
		{
			name:    "a non positive apply timeout falls back to none",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyApplyTimeout: "0s"},
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyApplyTimeout, "duration \"0s\" must be positive"},
			},
		},
		{
			name:         "an apply timeout on an unmanaged job is a warning and still parses",
			in:           map[string]string{KeyApplyTimeout: "2h"},
			policy:       PolicyNone,
			applyTimeout: 2 * time.Hour,
			wantIssues: []Issue{
				{SeverityWarn, KeyApplyTimeout, "set but nops_managed is not \"true\": ignored"},
			},
		},
		{
			name:         "an apply timeout on a hook is ignored with a warning",
			in:           map[string]string{KeyRole: "hook", KeyApplyTimeout: "2h"},
			policy:       PolicyNone,
			isHook:       true,
			timeout:      DefaultHookTimeout,
			applyTimeout: 2 * time.Hour,
			wantIssues: []Issue{
				{SeverityWarn, KeyApplyTimeout, "set but nops_role is \"hook\": ignored"},
			},
		},
		{
			name:    "a batch job healthy once running",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyHealthy: "running"},
			jobType: "batch",
			managed: true,
			policy:  PolicyAuto,
			healthy: HealthyRunning,
		},
		{
			name:    "a sysbatch job healthy once complete, as without the key",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyHealthy: "complete"},
			jobType: "sysbatch",
			managed: true,
			policy:  PolicyAuto,
			healthy: HealthyComplete,
		},
		{
			name:    "an invalid healthy rule falls back to none",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyHealthy: "Running"},
			jobType: "batch",
			managed: true,
			policy:  PolicyNone,
			wantIssues: []Issue{
				{SeverityError, KeyHealthy, "invalid value \"Running\" (want running or complete)"},
			},
		},
		{
			name:    "a healthy rule on a service job is ignored with a warning",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyHealthy: "complete"},
			jobType: "service",
			managed: true,
			policy:  PolicyAuto,
			healthy: HealthyComplete,
			wantIssues: []Issue{
				{SeverityWarn, KeyHealthy, "set on a service job, only a batch or sysbatch job reads it: ignored"},
			},
		},
		{
			name:    "a job without a type is a service job",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyHealthy: "running"},
			managed: true,
			policy:  PolicyAuto,
			healthy: HealthyRunning,
			wantIssues: []Issue{
				{SeverityWarn, KeyHealthy, "set on a service job, only a batch or sysbatch job reads it: ignored"},
			},
		},
		{
			name:    "a healthy rule on a system job is ignored with a warning",
			in:      map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyHealthy: "running"},
			jobType: "system",
			managed: true,
			policy:  PolicyAuto,
			healthy: HealthyRunning,
			wantIssues: []Issue{
				{SeverityWarn, KeyHealthy, "set on a system job, only a batch or sysbatch job reads it: ignored"},
			},
		},
		{
			name:    "a healthy rule on an unmanaged job is ignored with a warning",
			in:      map[string]string{KeyHealthy: "running"},
			jobType: "batch",
			policy:  PolicyNone,
			healthy: HealthyRunning,
			wantIssues: []Issue{
				{SeverityWarn, KeyHealthy, "set but nops_managed is not \"true\": ignored"},
			},
		},
		{
			name:    "a healthy rule on a hook is ignored with a warning",
			in:      map[string]string{KeyRole: "hook", KeyHealthy: "running"},
			jobType: "batch",
			policy:  PolicyNone,
			isHook:  true,
			timeout: DefaultHookTimeout,
			healthy: HealthyRunning,
			wantIssues: []Issue{
				{SeverityWarn, KeyHealthy, "set but nops_role is \"hook\": ignored"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in, tc.jobType)
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
			if got.ApplyTimeout != tc.applyTimeout {
				t.Errorf("ApplyTimeout = %s, want %s", got.ApplyTimeout, tc.applyTimeout)
			}
			if got.Healthy != tc.healthy {
				t.Errorf("Healthy = %q, want %q", got.Healthy, tc.healthy)
			}
			if got.IsHook != tc.isHook {
				t.Errorf("IsHook = %v, want %v", got.IsHook, tc.isHook)
			}
			if got.NotifyCompleted != tc.notifyCompleted {
				t.Errorf("NotifyCompleted = %v, want %v", got.NotifyCompleted, tc.notifyCompleted)
			}
			if !reflect.DeepEqual(got.Issues, tc.wantIssues) {
				t.Errorf("Issues = %+v, want %+v", got.Issues, tc.wantIssues)
			}
		})
	}
}

// ParseJob reads the meta and the type of a job, a missing type as Nomad's
// default, service.
func TestParseJob(t *testing.T) {
	m := map[string]string{KeyManaged: "true", KeyPolicy: "auto", KeyHealthy: "running"}
	batch := api.JobTypeBatch
	if got := ParseJob(&api.Job{Type: &batch, Meta: m}); len(got.Issues) != 0 || got.Healthy != HealthyRunning {
		t.Errorf("batch job: %+v, want healthy once running and no issue", got)
	}
	if got := ParseJob(&api.Job{Meta: m}); len(got.Issues) != 1 || got.Issues[0].Key != KeyHealthy {
		t.Errorf("job without a type: issues %+v, want nops_healthy ignored on a service job", got.Issues)
	}
}

func TestHasErrors(t *testing.T) {
	if Parse(map[string]string{KeyManaged: "true", KeyPolicy: "auto"}, "").HasErrors() {
		t.Error("valid config reports errors")
	}
	if !Parse(map[string]string{KeyManaged: "true", KeyPolicy: "x"}, "").HasErrors() {
		t.Error("invalid policy does not report errors")
	}
	if Parse(map[string]string{"nops_typo": "x"}, "").HasErrors() {
		t.Error("warning-only config reports errors")
	}
}

// TestDocKeysTableMatchesTheKeys checks the table of docs/meta-keys.md against
// the keys this package reads, in both directions, and the defaults it can
// check: the policy of a managed job with no nops_policy, and the timeout of a
// hook with no nops_timeout.
func TestDocKeysTableMatchesTheKeys(t *testing.T) {
	b, err := os.ReadFile("../../docs/meta-keys.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `(nops_[a-z_]+)` \\|(.*)$")
	documented := map[string][]string{}
	for _, line := range strings.Split(string(b), "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, dup := documented[m[1]]; dup {
			t.Errorf("docs/meta-keys.md documents %s twice", m[1])
		}
		documented[m[1]] = strings.Split(m[2], "|") // values, default, meaning
	}
	for key := range documented {
		if _, ok := knownKeys[key]; !ok {
			t.Errorf("docs/meta-keys.md documents %s, which nops does not read", key)
		}
	}
	for key := range knownKeys {
		if _, ok := documented[key]; !ok {
			t.Errorf("docs/meta-keys.md has no row for %s", key)
		}
	}

	if cells := documented[KeyPolicy]; len(cells) > 1 {
		want := string(Parse(map[string]string{KeyManaged: "true"}, "").Policy)
		if got := strings.Trim(strings.TrimSpace(cells[1]), "`"); got != want {
			t.Errorf("docs/meta-keys.md gives %s the default %q, a managed job without it gets %q", KeyPolicy, got, want)
		}
	}
	if cells := documented[KeyTimeout]; len(cells) > 1 {
		got, err := time.ParseDuration(strings.Trim(strings.TrimSpace(cells[1]), "`"))
		if err != nil || got != DefaultHookTimeout {
			t.Errorf("docs/meta-keys.md gives %s the default %q, DefaultHookTimeout is %s", KeyTimeout, cells[1], DefaultHookTimeout)
		}
	}
}
