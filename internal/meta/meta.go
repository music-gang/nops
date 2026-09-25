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
	KeyManaged  = "nops_managed"
	KeyPolicy   = "nops_policy"
	KeyPreHook  = "nops_pre_hook"
	KeyPostHook = "nops_post_hook"
	KeyRole     = "nops_role"
	KeyTimeout  = "nops_timeout"
)

// DefaultHookTimeout applies to a hook job that has no nops_timeout.
const DefaultHookTimeout = 5 * time.Minute

var knownKeys = map[string]struct{}{
	KeyManaged: {}, KeyPolicy: {}, KeyPreHook: {}, KeyPostHook: {}, KeyRole: {}, KeyTimeout: {},
}

// removedKeys are keys that used to exist, with what replaces them: still
// warned about, in a message that says so, rather than as a typo.
var removedKeys = map[string]string{
	"nops_pre_hook_timeout":  "removed: set " + KeyTimeout + " on the hook job",
	"nops_post_hook_timeout": "removed: set " + KeyTimeout + " on the hook job",
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

// Config is the parsed nops configuration of one job.
type Config struct {
	// Managed is true only for nops_managed = "true".
	Managed bool
	// Policy is the effective policy. It is PolicyNone whenever the job is not
	// managed or any Error issue was found (conservative reading).
	Policy Policy
	// PreHooks and PostHooks are the IDs of the hook jobs the job declares, in
	// the order they run; empty when not declared.
	PreHooks  []string
	PostHooks []string
	// IsHook marks a job with nops_role = "hook".
	IsHook bool
	// Timeout is how long a hook job may run (nops_timeout, or
	// DefaultHookTimeout); zero for a job that is not a hook.
	Timeout time.Duration
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
		if _, ok := knownKeys[k]; ok {
			continue
		}
		if msg, ok := removedKeys[k]; ok {
			add(SeverityWarn, k, "%s", msg)
			continue
		}
		add(SeverityWarn, k, "unknown key")
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

	if c.IsHook {
		c.Timeout = DefaultHookTimeout
	}
	if v, ok := m[KeyTimeout]; ok {
		d, err := time.ParseDuration(v)
		switch {
		case !c.IsHook:
			add(SeverityWarn, KeyTimeout, "set but %s is not \"hook\": ignored", KeyRole)
		case err != nil:
			add(SeverityError, KeyTimeout, "invalid duration %q: %v", v, err)
		case d <= 0:
			add(SeverityError, KeyTimeout, "duration %q must be positive", v)
		default:
			c.Timeout = d
		}
	}

	c.PreHooks = parseHooks(m, KeyPreHook, add)
	c.PostHooks = parseHooks(m, KeyPostHook, add)

	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Key < issues[j].Key })
	c.Issues = issues

	// Conservative reading: unmanaged or misconfigured jobs are never applied.
	if c.Managed && !c.HasErrors() {
		c.Policy = policy
	}
	return c
}

// parseHooks reads a comma-separated list of hook job IDs, in the order they
// run. An empty item (a stray comma, or an empty value) or the same hook twice
// is an error for the whole key: the list is then not declared.
func parseHooks(m map[string]string, key string, add func(Severity, string, string, ...any)) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	var ids []string
	seen := make(map[string]bool)
	for _, item := range strings.Split(v, ",") {
		id := strings.TrimSpace(item)
		switch {
		case id == "" && strings.TrimSpace(v) == "":
			add(SeverityError, key, "empty hook job ID")
			return nil
		case id == "":
			add(SeverityError, key, "empty item in the list of hook job IDs %q", v)
			return nil
		case seen[id]:
			add(SeverityError, key, "hook job %q is listed twice", id)
			return nil
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}
