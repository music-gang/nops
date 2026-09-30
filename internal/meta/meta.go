// Package meta parses and validates the nops_* job meta keys.
//
// It is the source of truth for the HCL syntax documented in
// docs/meta-keys.md: any change to keys, values or defaults here must be
// reflected there.
package meta

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/cronexpr"
)

const prefix = "nops_"

// Meta keys.
const (
	KeyManaged         = "nops_managed"
	KeyPolicy          = "nops_policy"
	KeyPreHook         = "nops_pre_hook"
	KeyPostHook        = "nops_post_hook"
	KeyRole            = "nops_role"
	KeyTimeout         = "nops_timeout"
	KeyNotifyCompleted = "nops_notify_completed"
	// KeySyncWindow and KeySyncWindowDuration say when Nops may deploy a job on
	// its own: a cron expression for when the window opens and how long it stays
	// open (docs/meta-keys.md).
	KeySyncWindow         = "nops_sync_window"
	KeySyncWindowDuration = "nops_sync_window_duration"
)

// DefaultHookTimeout applies to a hook job that has no nops_timeout.
const DefaultHookTimeout = 5 * time.Minute

var knownKeys = map[string]struct{}{
	KeyManaged: {}, KeyPolicy: {}, KeyPreHook: {}, KeyPostHook: {}, KeyRole: {}, KeyTimeout: {}, KeyNotifyCompleted: {},
	KeySyncWindow: {}, KeySyncWindowDuration: {},
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
	// NotifyCompleted opts the job into a notification when a deployment of it
	// becomes completed (nops_notify_completed = "true"; docs/logs-and-notifications.md#notifications).
	NotifyCompleted bool
	// SyncWindow is when Nops may deploy the job on its own
	// (nops_sync_window + nops_sync_window_duration); nil when not declared or
	// when either key is invalid. It is set whatever the policy is: only
	// policy auto is gated by it (docs/policies.md).
	SyncWindow *SyncWindow
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

	if v, ok := m[KeyNotifyCompleted]; ok {
		switch v {
		case "true":
			c.NotifyCompleted = true
		case "false":
		default:
			add(SeverityError, KeyNotifyCompleted, "invalid value %q (want \"true\" or \"false\")", v)
		}
		if !c.Managed {
			add(SeverityWarn, KeyNotifyCompleted, "set but %s is not \"true\": ignored", KeyManaged)
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

	c.SyncWindow = parseSyncWindow(m, c.Managed, policy, add)

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

// SyncWindow is a recurring span of time in which Nops may start a deployment
// of a job on its own: it opens at every time its cron expression matches and
// stays open for Duration. It says nothing about the time zone: the caller
// passes times in the one the expression is read in.
type SyncWindow struct {
	// Spec is the cron expression as written, Duration how long the window stays
	// open from each time it matches.
	Spec     string
	Duration time.Duration
	expr     *cronexpr.Expression
}

// Open reports whether t is inside the window: the latest time the expression
// matched at or before t is less than Duration ago. The end is exclusive.
func (w *SyncWindow) Open(t time.Time) bool {
	open := w.expr.Next(t.Add(-w.Duration))
	return !open.IsZero() && !open.After(t)
}

// NextOpen is the next time after t that the window opens. It is the zero
// time if it never does again, which a Spec accepted by Parse does not do
// unless it names a year that has passed.
func (w *SyncWindow) NextOpen(t time.Time) time.Time { return w.expr.Next(t) }

// maxCloseSteps bounds NextClose: a window that opens again before it has
// closed, every minute for an hour say, is open for ever.
const maxCloseSteps = 10000

// NextClose is when the window that is open at t closes: the end of the last
// of the windows that run into each other from the one covering t. It is the
// zero time if t is outside the window, or if it never closes (a window that
// reopens before each one has ended).
func (w *SyncWindow) NextClose(t time.Time) time.Time {
	if !w.Open(t) {
		return time.Time{}
	}
	start := w.expr.Next(t.Add(-w.Duration)) // the earliest window still covering t
	end := start.Add(w.Duration)
	for i := 0; i < maxCloseSteps; i++ {
		next := w.expr.Next(start)
		if next.IsZero() || next.After(end) {
			return end
		}
		start, end = next, next.Add(w.Duration)
	}
	return time.Time{}
}

// neverMatchesFrom is where parseSyncWindow looks for the first time an
// expression matches: one that does not match after it (a 30th of February)
// never does.
var neverMatchesFrom = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// parseSyncWindow reads nops_sync_window and nops_sync_window_duration, which
// are declared together or not at all. It returns nil, with an Error issue on
// the key at fault, for any invalid value (so the job falls back to policy
// none like any other invalid key), and, like the other keys of a managed job,
// a Warn when the job is not managed. A window under a policy other than auto
// is a Warn: it gates only what Nops starts on its own, and under approval the
// person's approval is the gate.
func parseSyncWindow(m map[string]string, managed bool, policy Policy, add func(Severity, string, string, ...any)) *SyncWindow {
	spec, hasSpec := m[KeySyncWindow]
	durStr, hasDur := m[KeySyncWindowDuration]
	switch {
	case !hasSpec && !hasDur:
		return nil
	case !hasDur:
		add(SeverityError, KeySyncWindow, "needs %s: how long the window stays open", KeySyncWindowDuration)
		return nil
	case !hasSpec:
		add(SeverityError, KeySyncWindowDuration, "set without %s: when the window opens", KeySyncWindow)
		return nil
	}

	expr, err := cronexpr.Parse(spec)
	if err != nil {
		add(SeverityError, KeySyncWindow, "invalid cron expression %q: %v", spec, err)
		return nil
	}
	if expr.Next(neverMatchesFrom).IsZero() {
		add(SeverityError, KeySyncWindow, "cron expression %q never matches", spec)
		return nil
	}
	d, err := time.ParseDuration(durStr)
	switch {
	case err != nil:
		add(SeverityError, KeySyncWindowDuration, "invalid duration %q: %v", durStr, err)
		return nil
	case d <= 0:
		add(SeverityError, KeySyncWindowDuration, "duration %q must be positive", durStr)
		return nil
	}

	switch {
	case !managed:
		add(SeverityWarn, KeySyncWindow, "set but %s is not \"true\": ignored", KeyManaged)
	case policy != PolicyAuto:
		add(SeverityWarn, KeySyncWindow, "ignored under policy %s: a window gates only what Nops starts on its own (policy auto)", policy)
	}
	return &SyncWindow{Spec: spec, Duration: d, expr: expr}
}
