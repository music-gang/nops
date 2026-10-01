package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/hooks"
	"github.com/music-gang/nops/internal/store"
)

// duplicateKeys names the keys that appear more than once at the top level of a
// JSON object. Decoding into a map would hide them, so the tokens are read.
func duplicateKeys(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %q (%v)", line, err)
	}
	seen := map[string]bool{}
	var dups []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("reading a key of %q: %v", line, err)
		}
		key := tok.(string)
		if seen[key] {
			dups = append(dups, key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			t.Fatalf("reading the value of %q in %q: %v", key, line, err)
		}
	}
	return dups
}

// jsonLog gives the engine the JSON handler, the default of the binary, writing
// to a buffer.
func (h *harness) jsonLog() *bytes.Buffer {
	var buf bytes.Buffer
	h.engine.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

// noKeyTwice fails on a log line with a key more than once: a parser that
// rejects duplicate keys drops the line, and a query on the key counts twice.
// It also requires the lines of msgs to be there, so a test that stopped
// reaching them cannot pass by having nothing to check.
func noKeyTwice(t *testing.T, buf *bytes.Buffer, msgs ...string) {
	t.Helper()
	found := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		if dups := duplicateKeys(t, line); len(dups) > 0 {
			t.Errorf("keys %v appear twice in %s", dups, line)
		}
		var rec struct{ Msg string }
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		found[rec.Msg] = true
	}
	for _, m := range msgs {
		if !found[m] {
			t.Errorf("no %q line was logged: the test does not reach it (lines: %v)", m, found)
		}
	}
}

// Every line the engine logs while it detects, approves and applies a
// deployment, or fails one, has each key once.
func TestNoLogLineCarriesAKeyTwice(t *testing.T) {
	t.Run("an auto deployment from detection to completed", func(t *testing.T) {
		h := newHarness(t)
		buf := h.jsonLog()
		h.lifecycle(t, "auto", 0)
		noKeyTwice(t, buf, "deployment pre_hook", "deployment applying", "deployment post_hook", "deployment completed")
	})

	t.Run("a deployment approved by a person", func(t *testing.T) {
		h := newHarness(t)
		buf := h.jsonLog()
		h.lifecycle(t, "approval", 0)
		noKeyTwice(t, buf, "deployment pending_approval", "deployment approved", "deployment completed")
	})

	t.Run("a deployment that fails at detection", func(t *testing.T) {
		h := newHarness(t)
		buf := h.jsonLog()
		h.snapshotWithMissingHook("auto", map[string]string{"nops_pre_hook": "migrate"}, nil)
		h.detect()
		noKeyTwice(t, buf, "deployment failed")
	})

	t.Run("a deployment that fails in a hook", func(t *testing.T) {
		h := newHarness(t)
		buf := h.jsonLog()
		d := h.multi(store.StatePreHook)
		h.hooks.setResultAt(d.ID, "pre", 0, hooks.Result{State: store.HookFailed, Error: "exit 1"})
		h.step(d)
		noKeyTwice(t, buf, "deployment failed")
	})

	t.Run("a transition that is skipped, and a rejection", func(t *testing.T) {
		h := newHarness(t)
		buf := h.jsonLog()
		h.nomad.setDrift("web", &api.JobDiff{Type: "Edited", ID: "web"})
		d := h.pendingApproval("web", managed("web", "approval", nil), "h1")
		// Someone else moved it first: the step's own move is a conflict.
		stale := *d
		if err := h.engine.Reject(context.Background(), d.ID, "alice"); err != nil {
			t.Fatal(err)
		}
		h.engine.applyTransition(context.Background(), h.engine.log.With("deployment_id", d.ID), &stale, store.StateApplying, "")
		noKeyTwice(t, buf, "deployment rejected", "transition skipped")
	})
}

// A failed deployment's notification says where it failed, with the same
// names as the log; any other notification has no phase.
func TestNotificationCarriesTheFailedPhase(t *testing.T) {
	h := newHarness(t)
	d := h.multi(store.StatePreHook)
	log := h.engine.log.With("deployment_id", d.ID)
	steps := []struct {
		from, to store.State
		want     string
	}{
		{store.StateDetected, store.StateFailed, "detection"},
		{store.StatePreHook, store.StateFailed, "pre"},
		{store.StateApplying, store.StateFailed, "apply"},
		{store.StatePostHook, store.StateFailed, "post"},
		{store.StateDetected, store.StatePendingApproval, ""},
	}
	for i, s := range steps {
		h.engine.announce(context.Background(), log, d, s.from, s.to, "boom")
		h.notifier.waitFor(t, i+1)
		h.notifier.mu.Lock()
		got := h.notifier.phases[i]
		h.notifier.mu.Unlock()
		if got != s.want {
			t.Errorf("%s -> %s: phase = %q, want %q", s.from, s.to, got, s.want)
		}
	}
}
