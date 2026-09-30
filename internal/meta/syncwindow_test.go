package meta

import (
	"reflect"
	"testing"
	"time"
)

func managedAuto(extra map[string]string) map[string]string {
	m := map[string]string{KeyManaged: "true", KeyPolicy: "auto"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestParseSyncWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         map[string]string
		wantWindow bool
		wantPolicy Policy
		wantIssues []Issue
	}{
		{
			name:       "a window under auto",
			in:         managedAuto(map[string]string{KeySyncWindow: "0 9 * * 1-5", KeySyncWindowDuration: "9h"}),
			wantWindow: true, wantPolicy: PolicyAuto,
		},
		{
			name:       "no window",
			in:         managedAuto(nil),
			wantPolicy: PolicyAuto,
		},
		{
			name:       "the window without its duration is an error",
			in:         managedAuto(map[string]string{KeySyncWindow: "0 9 * * *"}),
			wantPolicy: PolicyNone,
			wantIssues: []Issue{{SeverityError, KeySyncWindow, "needs nops_sync_window_duration: how long the window stays open"}},
		},
		{
			name:       "the duration without its window is an error",
			in:         managedAuto(map[string]string{KeySyncWindowDuration: "2h"}),
			wantPolicy: PolicyNone,
			wantIssues: []Issue{{SeverityError, KeySyncWindowDuration, "set without nops_sync_window: when the window opens"}},
		},
		{
			name:       "an invalid cron expression",
			in:         managedAuto(map[string]string{KeySyncWindow: "whenever", KeySyncWindowDuration: "2h"}),
			wantPolicy: PolicyNone,
			wantIssues: []Issue{{SeverityError, KeySyncWindow, `invalid cron expression "whenever": `}},
		},
		{
			name:       "a cron expression that never matches",
			in:         managedAuto(map[string]string{KeySyncWindow: "0 0 30 2 *", KeySyncWindowDuration: "2h"}),
			wantPolicy: PolicyNone,
			wantIssues: []Issue{{SeverityError, KeySyncWindow, `cron expression "0 0 30 2 *" never matches`}},
		},
		{
			name:       "a duration that is not one",
			in:         managedAuto(map[string]string{KeySyncWindow: "0 9 * * *", KeySyncWindowDuration: "all day"}),
			wantPolicy: PolicyNone,
			wantIssues: []Issue{{SeverityError, KeySyncWindowDuration, `invalid duration "all day": `}},
		},
		{
			name:       "a duration that is not positive",
			in:         managedAuto(map[string]string{KeySyncWindow: "0 9 * * *", KeySyncWindowDuration: "0s"}),
			wantPolicy: PolicyNone,
			wantIssues: []Issue{{SeverityError, KeySyncWindowDuration, `duration "0s" must be positive`}},
		},
		{
			name:       "under approval the approval is the gate: the window is a warning",
			in:         map[string]string{KeyManaged: "true", KeyPolicy: "approval", KeySyncWindow: "0 9 * * *", KeySyncWindowDuration: "2h"},
			wantWindow: true, wantPolicy: PolicyApproval,
			wantIssues: []Issue{{SeverityWarn, KeySyncWindow, "ignored under policy approval: a window gates only what Nops starts on its own (policy auto)"}},
		},
		{
			name:       "under none there is nothing to gate",
			in:         map[string]string{KeyManaged: "true", KeyPolicy: "none", KeySyncWindow: "0 9 * * *", KeySyncWindowDuration: "2h"},
			wantWindow: true, wantPolicy: PolicyNone,
			wantIssues: []Issue{{SeverityWarn, KeySyncWindow, "ignored under policy none: a window gates only what Nops starts on its own (policy auto)"}},
		},
		{
			name:       "a job that is not managed ignores it",
			in:         map[string]string{KeyPolicy: "auto", KeySyncWindow: "0 9 * * *", KeySyncWindowDuration: "2h"},
			wantWindow: true, wantPolicy: PolicyNone,
			wantIssues: []Issue{
				{SeverityWarn, KeyPolicy, `set but nops_managed is not "true": ignored`},
				{SeverityWarn, KeySyncWindow, `set but nops_managed is not "true": ignored`},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Parse(tc.in)
			if (c.SyncWindow != nil) != tc.wantWindow {
				t.Errorf("SyncWindow = %+v, want set: %v", c.SyncWindow, tc.wantWindow)
			}
			if c.Policy != tc.wantPolicy {
				t.Errorf("Policy = %q, want %q", c.Policy, tc.wantPolicy)
			}
			if len(c.Issues) != len(tc.wantIssues) {
				t.Fatalf("issues = %v, want %v", c.Issues, tc.wantIssues)
			}
			for i, want := range tc.wantIssues {
				got := c.Issues[i]
				// An error from the parser library is quoted as a prefix only.
				if got.Severity != want.Severity || got.Key != want.Key || len(got.Message) < len(want.Message) || got.Message[:len(want.Message)] != want.Message {
					t.Errorf("issue %d = %v, want %v", i, got, want)
				}
			}
		})
	}
}

func TestSyncWindowKeepsWhatWasWritten(t *testing.T) {
	c := Parse(managedAuto(map[string]string{KeySyncWindow: "0 9 * * 1-5", KeySyncWindowDuration: "9h30m"}))
	if c.SyncWindow == nil || c.SyncWindow.Spec != "0 9 * * 1-5" || c.SyncWindow.Duration != 9*time.Hour+30*time.Minute {
		t.Errorf("SyncWindow = %+v, want the expression and the duration as written", c.SyncWindow)
	}
}

func window(t *testing.T, spec, duration string) *SyncWindow {
	t.Helper()
	c := Parse(managedAuto(map[string]string{KeySyncWindow: spec, KeySyncWindowDuration: duration}))
	if c.SyncWindow == nil {
		t.Fatalf("window %q for %s is invalid: %v", spec, duration, c.Issues)
	}
	return c.SyncWindow
}

func TestSyncWindowOpen(t *testing.T) {
	// Mondays to Fridays from 09:00 for 9 hours: until 18:00.
	w := window(t, "0 9 * * 1-5", "9h")
	// 2026-09-28 is a Monday; the day counts on from it (32 is Friday 2 October).
	at := func(day int, hour, min int) time.Time {
		return time.Date(2026, time.September, day, hour, min, 0, 0, time.UTC)
	}
	for _, tc := range []struct {
		name string
		t    time.Time
		want bool
	}{
		{"the moment it opens", at(28, 9, 0), true},
		{"a minute before", at(28, 8, 59), false},
		{"in the middle", at(28, 13, 0), true},
		{"the last minute", at(28, 17, 59), true},
		{"the end is exclusive", at(28, 18, 0), false},
		{"the night", at(29, 3, 0), false},
		{"Friday afternoon", at(32, 16, 0), true},
		{"Saturday", at(33, 12, 0), false},
		{"Sunday", at(34, 12, 0), false},
	} {
		if got := w.Open(tc.t); got != tc.want {
			t.Errorf("%s (%s): Open = %v, want %v", tc.name, tc.t.Format("Mon 15:04"), got, tc.want)
		}
	}
}

// A window that opens in the evening and lasts past midnight is open the next
// morning, which is the day the expression does not match.
func TestSyncWindowThatCrossesMidnight(t *testing.T) {
	w := window(t, "0 22 * * *", "8h") // 22:00 to 06:00
	for _, tc := range []struct {
		hour int
		want bool
	}{{21, false}, {22, true}, {23, true}, {0, true}, {5, true}, {6, false}, {12, false}} {
		at := time.Date(2026, time.September, 29, tc.hour, 0, 0, 0, time.UTC)
		if got := w.Open(at); got != tc.want {
			t.Errorf("%02d:00: Open = %v, want %v", tc.hour, got, tc.want)
		}
	}
}

// The expression is read in the zone of the time it is asked about.
func TestSyncWindowIsReadInTheZoneOfTheTime(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	w := window(t, "0 9 * * *", "2h")                                    // 09:00 to 11:00
	instant := time.Date(2026, time.September, 29, 8, 0, 0, 0, time.UTC) // 10:00 in Rome, CEST
	if !w.Open(instant.In(rome)) {
		t.Error("10:00 in Rome is outside a 09:00-11:00 window")
	}
	if w.Open(instant) {
		t.Error("08:00 UTC is inside a 09:00-11:00 window")
	}
}

func TestSyncWindowNextOpen(t *testing.T) {
	w := window(t, "0 9 * * 1-5", "9h")
	friday := time.Date(2026, time.October, 2, 18, 0, 0, 0, time.UTC)
	want := time.Date(2026, time.October, 5, 9, 0, 0, 0, time.UTC) // the Monday after
	if got := w.NextOpen(friday); !got.Equal(want) {
		t.Errorf("NextOpen(%s) = %s, want %s", friday.Format("Mon 15:04"), got, want)
	}
	if !reflect.DeepEqual(w.NextOpen(want.Add(-time.Second)), want) {
		t.Errorf("a second before it opens, NextOpen is not the opening")
	}
}

func TestSyncWindowNextClose(t *testing.T) {
	day := func(d, hour, min int) time.Time { return time.Date(2026, time.September, d, hour, min, 0, 0, time.UTC) }
	w := window(t, "0 9 * * *", "11h") // 09:00 to 20:00
	for _, tc := range []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"the moment it opens", day(28, 9, 0), day(28, 20, 0)},
		{"in the middle", day(28, 15, 30), day(28, 20, 0)},
		{"the last minute", day(28, 19, 59), day(28, 20, 0)},
		{"closed: there is nothing to close", day(28, 20, 0), time.Time{}},
		{"closed at night", day(28, 3, 0), time.Time{}},
	} {
		if got := w.NextClose(tc.at); !got.Equal(tc.want) {
			t.Errorf("%s: NextClose = %s, want %s", tc.name, got, tc.want)
		}
	}

	// Windows that run into each other are one.
	overlapping := window(t, "0 0,6 * * *", "8h")
	if got, want := overlapping.NextClose(day(28, 1, 0)), day(28, 14, 0); !got.Equal(want) {
		t.Errorf("overlapping windows: NextClose = %s, want %s (00:00-08:00 and 06:00-14:00 are one)", got, want)
	}

	// One that reopens before each has ended never closes.
	always := window(t, "* * * * *", "1h")
	if got := always.NextClose(day(28, 12, 0)); !got.IsZero() {
		t.Errorf("an always open window: NextClose = %s, want the zero time", got)
	}
}
