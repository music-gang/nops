// Package meta parses and validates the nops_* job meta keys.
//
// It is the source of truth for the HCL syntax documented in CLAUDE.md: any
// change to keys, values or defaults here must be reflected there.
package meta

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const prefix = "nops_"

// Meta keys.
const (
	KeyManaged         = "nops_managed"
	KeyPolicy          = "nops_policy"
	KeyPreHook         = "nops_pre_hook"
	KeyPreHookTimeout  = "nops_pre_hook_timeout"
	KeyPostHook        = "nops_post_hook"
	KeyPostHookTimeout = "nops_post_hook_timeout"
	KeyRole            = "nops_role"
)

// DefaultHookTimeout applies when a hook is declared without an explicit timeout.
const DefaultHookTimeout = 5 * time.Minute

var knownKeys = map[string]struct{}{
	KeyManaged: {}, KeyPolicy: {}, KeyPreHook: {}, KeyPreHookTimeout: {},
	KeyPostHook: {}, KeyPostHookTimeout: {}, KeyRole: {},
}

// Policy decides how a detected difference is applied.
type Policy string

const (
	PolicyAuto     Policy = "auto"
	PolicyApproval Policy = "approval"
	PolicyNone     Policy = "none"
)

// Severity of a validation issue. The caller logs Warn at WARN and Error at ERROR.
type Severity string

const (
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Issue is a problem found while parsing a job's meta.
type Issue struct {
	Severity Severity
	Key      string
	Message  string
}

func (i Issue) String() string { return fmt.Sprintf("%s: %s", i.Key, i.Message) }

// Hook is a declared deployment hook.
type Hook struct {
	JobID   string
	Timeout time.Duration
}

// Config is the parsed nops configuration of one job.
type Config struct {
	// Managed is true only for nops_managed = "true".
	Managed bool
	// Policy is the effective policy. It is PolicyNone whenever the job is not
	// managed or any Error issue was found (conservative reading).
	Policy Policy
	// PreHook and PostHook are nil when not declared.
	PreHook  *Hook
	PostHook *Hook
	// IsHook marks a job with nops_role = "hook".
	IsHook bool
	// Issues lists every problem found, sorted by key.
	Issues []Issue
}

// HasErrors reports whether any issue is of severity Error.
func (c Config) HasErrors() bool {
	for _, i := range c.Issues {
		if i.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Parse reads the nops_* keys of a job's meta map. It never fails: invalid
// input is reported through Config.Issues and resolved to the most
// conservative reading. Values are case-sensitive.
func Parse(m map[string]string) Config {
	c := Config{Policy: PolicyNone}
	var issues []Issue
	add := func(sev Severity, key, format string, args ...any) {
		issues = append(issues, Issue{Severity: sev, Key: key, Message: fmt.Sprintf(format, args...)})
	}

	for k := range m {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if _, ok := knownKeys[k]; !ok {
			add(SeverityWarn, k, "unknown key")
		}
	}

	switch v, ok := m[KeyManaged]; {
	case !ok:
	case v == "true":
		c.Managed = true
	case v == "false":
	default:
		add(SeverityError, KeyManaged, "invalid value %q (want \"true\" or \"false\")", v)
	}

	policy := PolicyNone
	if v, ok := m[KeyPolicy]; ok {
		switch p := Policy(v); p {
		case PolicyAuto, PolicyApproval, PolicyNone:
			policy = p
		default:
			add(SeverityError, KeyPolicy, "invalid value %q (want auto, approval or none)", v)
		}
		if !c.Managed {
			add(SeverityWarn, KeyPolicy, "set but %s is not \"true\": ignored", KeyManaged)
		}
	}

	if v, ok := m[KeyRole]; ok {
		if v == "hook" {
			c.IsHook = true
		} else {
			add(SeverityError, KeyRole, "invalid value %q (want \"hook\")", v)
		}
	}

	c.PreHook = parseHook(m, KeyPreHook, KeyPreHookTimeout, add)
	c.PostHook = parseHook(m, KeyPostHook, KeyPostHookTimeout, add)

	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Key < issues[j].Key })
	c.Issues = issues

	// Conservative reading: unmanaged or misconfigured jobs are never applied.
	if c.Managed && !c.HasErrors() {
		c.Policy = policy
	}
	return c
}

func parseHook(m map[string]string, hookKey, timeoutKey string, add func(Severity, string, string, ...any)) *Hook {
	id, hasID := m[hookKey]
	tv, hasTimeout := m[timeoutKey]

	if hasTimeout && (!hasID || id == "") {
		add(SeverityWarn, timeoutKey, "set but %s is not declared: ignored", hookKey)
	}
	if hasID && id == "" {
		add(SeverityError, hookKey, "empty hook job ID")
		return nil
	}
	if !hasID {
		return nil
	}

	h := &Hook{JobID: id, Timeout: DefaultHookTimeout}
	if hasTimeout {
		d, err := time.ParseDuration(tv)
		switch {
		case err != nil:
			add(SeverityError, timeoutKey, "invalid duration %q: %v", tv, err)
		case d <= 0:
			add(SeverityError, timeoutKey, "duration %q must be positive", tv)
		default:
			h.Timeout = d
		}
	}
	return h
}
