package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// HoldKind says what holds a job.
type HoldKind string

const (
	// HoldPaused is a person's pause (Engine.Pause).
	HoldPaused HoldKind = "paused"
	// HoldWindow is the time being outside the job's sync window
	// (nops_sync_window): a scheduled pause, which nobody set and nobody lifts.
	HoldWindow HoldKind = "window"
)

// Hold is a condition that keeps nops from starting a deployment for a job
// (docs/state-machine.md, "holding a job"): a person's pause, or a sync window
// that is closed. It only ever subtracts: it creates,
// approves and advances nothing, so it cannot break invariant 3. It gates the
// start only: a deployment already in pre_hook, applying or post_hook is never
// touched. Drift is still detected and shown while the job is held.
type Hold struct {
	Kind HoldKind
	// Reason says why, for a person: "paused by alice: db incident".
	Reason string
	// By and Since are who set the hold and when; zero when no person did.
	By    string
	Since time.Time
	// Note is what the person wrote when they set it, if anything.
	Note string
}

// ErrPaused is returned by Approve when the deployment's job is paused: the
// pause is a brake on the whole job, the approval included. The deployment stays
// pending_approval and can be approved again once the job is resumed.
var ErrPaused = errors.New("the job is paused")

// ErrNotPausable is returned by Pause and Resume for a job that is not among the
// managed jobs of the last detection cycle: there is nothing to hold.
var ErrNotPausable = errors.New("the job is not in the repository as of the last detection cycle")

// maxPauseNote is the longest reason a pause takes, in bytes: it is shown on
// several pages and written to the log.
const maxPauseNote = 500

// ErrPauseNoteTooLong is returned by Pause when the reason is longer than
// maxPauseNote.
var ErrPauseNoteTooLong = fmt.Errorf("the reason is longer than %d bytes", maxPauseNote)

// pauseHold is the Hold a person's pause makes.
func pauseHold(p store.Pause) *Hold {
	reason := "paused by " + p.PausedBy
	if p.Reason != "" {
		reason += ": " + p.Reason
	}
	return &Hold{Kind: HoldPaused, Reason: reason, By: p.PausedBy, Since: p.PausedAt, Note: p.Reason}
}

// WindowStatus is where a job's sync window stands right now, for the page and
// for the hold. It is computed from the job's meta, the clock and the
// instance's time zone, never stored.
type WindowStatus struct {
	// Spec and Duration are the window as the job declares it.
	Spec     string
	Duration time.Duration
	// Zone is the IANA name of the time zone Spec is read in, and Until is in it.
	Zone string
	// Open says whether the window is open now. Until is when it closes if it
	// is, when it opens next if it is not; zero when it never does (a window
	// that reopens before each has ended never closes).
	Open  bool
	Until time.Time
}

// windowStatus is the status of a job's sync window now, or nil when it has
// none or it does not apply: a window gates only what Nops starts on its own, so
// only a job under policy auto. The window is read in the instance's time zone
// (Options.SyncWindowLocation).
func (e *Engine) windowStatus(cfg meta.Config) *WindowStatus {
	if cfg.Policy != meta.PolicyAuto || cfg.SyncWindow == nil {
		return nil
	}
	now := e.now().In(e.syncLoc)
	w := cfg.SyncWindow
	ws := &WindowStatus{Spec: w.Spec, Duration: w.Duration, Zone: e.syncLoc.String(), Open: w.Open(now)}
	if ws.Open {
		ws.Until = w.NextClose(now).In(e.syncLoc)
	} else {
		ws.Until = w.NextOpen(now).In(e.syncLoc)
	}
	return ws
}

// windowTimeFormat is how a time of a window is written for a person: the day,
// so "next opens" is not read as today, and the zone, so a window is never read
// in the wrong one.
const windowTimeFormat = "Mon 2006-01-02 15:04 MST"

// Format writes t, in the window's zone, as windowTimeFormat does.
func (ws *WindowStatus) Format(t time.Time) string { return t.Format(windowTimeFormat) }

// hold is the Hold of a window that is closed, nil when it is open or there is
// none.
func (ws *WindowStatus) hold() *Hold {
	if ws == nil || ws.Open {
		return nil
	}
	reason := "outside its sync window"
	if !ws.Until.IsZero() {
		reason += ", next opens " + ws.Format(ws.Until)
	}
	return &Hold{Kind: HoldWindow, Reason: reason}
}

// holdFrom is what holds a job right now: a person's pause (a row in SQLite,
// as read once per detection cycle, which wins) or else its closed sync window.
func holdFrom(pause *Hold, ws *WindowStatus) *Hold {
	if pause != nil {
		return pause
	}
	return ws.hold()
}

// holds indexes the pauses in force this cycle, by job. A job absent from it is
// not paused.
func holds(pauses []store.Pause) map[jobKey]*Hold {
	out := make(map[jobKey]*Hold, len(pauses))
	for _, p := range pauses {
		out[jobKey{p.Namespace, p.JobID}] = pauseHold(p)
	}
	return out
}

// pauseOf reads a job's pause straight from the store, for the paths that act
// between two detection cycles (Approve, the apply loop's first step). It
// returns nil when the job is not paused.
func (e *Engine) pauseOf(ctx context.Context, namespace, jobID string) (*Hold, error) {
	p, err := e.store.PauseOf(ctx, namespace, jobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return pauseHold(p), nil
}

// Pause holds a job: nops starts no new deployment for it and does not advance
// a `detected` one, until Resume. It is for the moment a person needs nops to
// leave a job alone (a hand fix in Nomad, a `nomad job revert`) without a round
// trip through git. It never creates or approves anything, and a deployment
// already running finishes. The pause is persisted before anything else
// (invariant 7) and the detection loop is asked for a cycle, so the job shows
// as paused, and a `detected` deployment is put aside, now.
//
// It returns ErrNotPausable for a job that is not in the last cycle's
// observations, store.ErrAlreadyPaused if it is already paused and
// ErrPauseNoteTooLong for a reason over maxPauseNote bytes.
func (e *Engine) Pause(ctx context.Context, namespace, jobID, actor, note string) error {
	if actor == "" {
		return errors.New("pause: actor is required")
	}
	note = strings.TrimSpace(note)
	if len(note) > maxPauseNote {
		return fmt.Errorf("pause %s/%s: %w", namespace, jobID, ErrPauseNoteTooLong)
	}
	if !e.managedNS[namespace] || !e.observed(namespace, jobID) {
		return fmt.Errorf("pause %s/%s: %w", namespace, jobID, ErrNotPausable)
	}
	if err := e.store.PauseJob(ctx, namespace, jobID, actor, note); err != nil {
		return err
	}
	e.log.InfoContext(ctx, "job paused", "job", jobID, "namespace", namespace, "actor", actor, "reason", note)
	// The page the person is sent back to reads the last cycle's observation:
	// say so in it now rather than when the cycle asked for below ends.
	if p, err := e.store.PauseOf(ctx, namespace, jobID); err != nil {
		e.log.ErrorContext(ctx, "read the pause just written", "job", jobID, "namespace", namespace, "error", err)
	} else {
		e.setHold(jobKey{namespace, jobID}, pauseHold(p))
	}
	e.kickDetection()
	return nil
}

// setHold puts the hold (nil: none) on a job's last observation, ahead of the
// next cycle, which recomputes it from the store anyway. A job with no
// observation has nothing to update.
func (e *Engine) setHold(key jobKey, h *Hold) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o, ok := e.observations[key]; ok {
		o.Hold = h
		e.observations[key] = o
	}
}

// Resume lifts a job's pause: the next detection cycle, asked for now, treats it
// as any other job, planning it afresh (what is applied is never a plan from
// before the pause). It returns store.ErrNotPaused if there is no pause.
func (e *Engine) Resume(ctx context.Context, namespace, jobID, actor string) error {
	if actor == "" {
		return errors.New("resume: actor is required")
	}
	if !e.managedNS[namespace] {
		return fmt.Errorf("resume %s/%s: %w", namespace, jobID, store.ErrNotFound)
	}
	if err := e.store.ResumeJob(ctx, namespace, jobID, actor); err != nil {
		return err
	}
	e.log.InfoContext(ctx, "job resumed", "job", jobID, "namespace", namespace, "actor", actor)
	e.setHold(jobKey{namespace, jobID}, nil)
	e.kickDetection()
	return nil
}
