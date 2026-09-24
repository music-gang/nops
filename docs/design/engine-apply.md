# engine-apply design

This is the second half of `internal/engine`: moving a deployment from
`detected` (policy `auto`) or from an approved `pending_approval` through
`pre_hook` → `applying` → `post_hook` to `completed`, and every failure path.
Detection ([design](engine-detection.md)) never touches these states; this is
where they are advanced. Recovery after a restart reuses the same
resume-safe steps (decision 8).

## Scope

One apply cycle (`Engine.RunApply`, ticking every `-engine-interval`) picks up
every non-terminal deployment (`Store.ListActive`) that is in `detected`,
`pre_hook`, `applying` or `post_hook`, and advances each one, one goroutine
per deployment, until it reaches a terminal state or `ctx` is cancelled.
`pending_approval` is never advanced by this loop (invariant 3 in
[philosophy.md](../philosophy.md)): the only way out of it is a human
decision, through `Engine.Approve`/`Reject` (see
[Decisions](#decisions), 1).

Every step follows invariant 7 (state persisted before acting on Nomad): a
`Store.Transition` (or the new `SetApplied`, see [Decisions](#decisions), 2) is
saved first, and only then does nops call Nomad. This makes every step
resume-safe purely from what is in the store, which is what lets
`engine-recovery` reuse it instead of duplicating the logic.

## Interfaces

Following the pattern in [architecture.md](../architecture.md) (a consumer
declares its own small interface), `internal/engine/engine.go` extends what it
already declares over `nomadx` and `store`, rather than duplicating them:

- `Nomad` gains what apply needs beyond detection's `ParseHCL`/`Job`/`Plan`/
  `RegisterCAS`: reading a Nomad deployment status and listing allocations of
  the target job (both already exist on `nomadx.Client` in some form —
  `Allocations` is reused as is; a deployment-status method is new, see
  [Decisions](#decisions), 3).
- A new `Hooks` interface with one method, `Run(ctx, hooks.Request) (hooks.Result, error)`,
  implemented by `*hooks.Runner` — the same shape `hooks.md` already documents.
- `Store` gains `SetApplied` and `AppliedSince` (see [Decisions](#decisions),
  2 and 4); `Transition`, `GetDeployment`, `ListActive` are reused as they are.
- `Options` gains `Hooks Hooks`, `EngineInterval time.Duration` and
  `ApplyTimeout time.Duration` (the last two already exist on
  `config.Config`, unused until now).

## Design

### Loop

`Engine.RunApply(ctx)` ticks every `EngineInterval`. Each tick calls
`Store.ListActive`, keeps an in-memory `map[string]struct{}` (guarded by a
mutex) of deployment IDs already being worked on, and starts a goroutine per
deployment not already in that set; the goroutine removes its ID when the
deployment reaches a terminal state or the step returns a retryable error
(next tick picks it up again). This mirrors the "no in-memory mutex, the DB is
the lock" rule (invariant 6) for *which job* has an active deployment, while
still needing an in-process guard against starting the *same* deployment
twice from two overlapping ticks.

### Per-deployment step

One call, driven by the deployment's current `State` (a `switch`, one case per
state below); every failure path notifies once, right after the `failed`
transition commits (`Notifier.Notify` in a goroutine, per
[error-handling.md](../error-handling.md#notifications)). A Nomad or SQLite
error is never swallowed: it is logged at ERROR and the deployment is left in
its current state for the next tick to retry (the "fail loud, retry" pattern
`hooks.Runner.Run`'s own contract already uses) — only a hook `Result` or a
CAS conflict actually closes the deployment.

- **`detected`** (only reached here for `auto`; `pending_approval` is moved by
  `Engine.Approve`, see [Decisions](#decisions), 1, which runs the same rule
  below): read `nops_pre_hook` from `meta.Parse` on the stored `job_spec`'s
  meta. Declared → `Transition` to `pre_hook`. Not declared → `Transition`
  straight to `applying`.
- **`pre_hook`**: `Hooks.Run(ctx, hooks.Request{DeploymentID, Phase: "pre",
  HookJobID, Commit, Timeout, Target: job_spec})`. An error from `Run` (Nomad
  or SQLite) is retried next tick, unchanged. A terminal `Result`:
  `succeeded` → `Transition` to `applying`; `failed`/`timed_out` → `Transition`
  to `failed` (message from `Result.Error`) — the live job is never touched
  (`state-machine.md`'s "a pre-hook leaves the live job untouched").
- **`applying`**, register not yet done (`AppliedIndex == 0`): re-read the
  live job (`Nomad.Job`). If `JobModifyIndex == CASIndex`: run a fresh
  `Plan` (invariant 1 applies again here, not just at detection, since time
  passed waiting for approval or for the pre-hook); an empty diff → `Transition`
  to `completed` ("already in sync", a no-op apply); otherwise
  `RegisterCAS(job_spec, CASIndex, false)` (counts were already substituted
  for `scaling` groups at detection, so `job_spec` is registered as stored).
  `ErrCASConflict` → `Transition` to `failed` ("job modified outside nops");
  the retry rule then allows a new deployment once detection sees the
  changed index. If the live index differs from `CASIndex` *and* a plan of
  `job_spec` against the live job is already empty, the register already
  happened (a crash between `RegisterCAS` succeeding and the next step) — carry
  on as below without registering again. Any other mismatch → `Transition` to
  `failed` ("conflict: live job changed and our spec no longer applies").
  Once registered (or found already applied): re-read the live job once more
  and call `SetApplied(id, JobModifyIndex, EvalID)` — never the index from the
  register response (decision log, 2026-09-23: "the register response can
  carry a different index").
- **`applying`**, register already done (`AppliedIndex != 0`): wait for the
  Nomad deployment (or the allocations) to be healthy — see
  [Decisions](#decisions), 3 — bounded by `ApplyTimeout` counted from
  `AppliedSince(id)` (see [Decisions](#decisions), 4). Healthy → declared
  `nops_post_hook`? `Transition` to `post_hook` : `Transition` to `completed`.
  Nomad deployment `failed`/`cancelled`, or the timeout expires →
  `Transition` to `failed` ("apply did not become healthy: <reason>"), and
  nothing more: no rollback, no `nomad deployment fail` (see
  [Decisions](#decisions), 5).
- **`post_hook`**: same as `pre_hook`, but a `failed`/`timed_out` result's
  message notes explicitly that the apply is already live and is not undone
  (`state-machine.md`: "a post-hook failure does not undo the apply").

### Races with detection

Detection only ever transitions a deployment out of `detected` or
`pending_approval` (supersede, or completing an empty-plan no-op); it never
touches `pre_hook`/`applying`/`post_hook` (already true today, per
`engine-detection.md`'s per-job reconciliation step 1). The one overlap is a
`detected` deployment that a detection cycle supersedes at the same moment
this loop starts working it: both call `Store.Transition` with `From:
detected`, and `ErrStateConflict` (compare-and-set on the state) makes exactly
one of them win. Whichever call in the apply step gets `ErrStateConflict`
simply drops that deployment for this tick (logged at INFO, not ERROR: this is
an expected, harmless race) instead of retrying — the deployment is
`superseded` now, so there is nothing left to apply.

## Decisions

These are recorded in the [decision log](decisions.md); the reasoning is
expanded here.

1. **`Engine.Approve`/`Reject` move `pending_approval` forward.**
   `Engine.Approve(ctx, id, specHash, actor string) error` and
   `Reject(ctx, id, actor string) error`. `Approve` refuses (without touching
   Nomad or the store) if `specHash` no longer matches the deployment's
   current `SpecHash` — invariant 3's "an approval is valid for
   `(deployment_id, spec_hash)` and never transfers to another spec" — then
   applies the same hook-declared rule as `detected` above (`pre_hook` if
   `nops_pre_hook` is declared, else `applying`) with `DecidedBy: actor` on
   that `Transition`. `Reject` is a plain `Transition` to `rejected`. `web`
   calls only these two methods; it never decides the target state or calls
   `Store.Transition` itself for an approval.
2. **`Store.SetApplied` persists `applied_index` (and `eval_id` when known)
   mid-state.** `SetApplied(ctx, id string, appliedIndex uint64, evalID string) error`,
   guarded by `WHERE state = 'applying'` (no schema change: the columns
   already exist). This is needed because invariant 7 requires them
   persisted *before* nops starts waiting for health, while the deployment is
   still `applying` — `Store.Transition` only writes on a state change.
   `evalID` may be empty: the crash-recovery path in decision 3's register
   step (the register already happened before the restart) has no fresh eval
   to record, and health does not need one (see decision 3) — a call with an
   empty `evalID` keeps whatever was recorded before, rather than blanking it.
3. **"Healthy" is a Nomad deployment tracking our applied index, or the
   allocations of our applied version when there is none.** Implementation
   note (a refinement made while coding, not in the original proposal
   answered above): rather than going through the register's `EvalID` and
   `Evaluations().Info` to find a Nomad deployment ID, `nomadx.Client` gains
   `LatestDeployment(ctx, jobID) (*api.Deployment, error)` wrapping
   `Jobs().LatestDeployment`, which returns the job's current Nomad
   deployment directly (`nil` when it has none — Nomad answers with an empty
   body, not a 404). Apply's health check compares its `JobModifyIndex` to
   the deployment's `applied_index`: a match means it is tracking exactly
   this apply, and its `Status` decides (`successful` → healthy;
   `failed`/`cancelled` → failed; anything else → still waiting). No match
   (including no deployment at all, for a `batch` job or an `update` stanza
   that produces none) falls back to the allocations of the applied job
   version: `nomadx.Alloc` gains `JobVersion`, matched against the live job's
   own `Version` re-read after the register (and only while the live job's
   index is still `applied_index`: otherwise it was modified outside nops, its
   allocations are not ours, and the deployment is `failed`, see decision 8),
   and health follows the same
   `failed`/`lost` → failed, `running`/`complete` → healthy, anything else →
   still waiting logic already used for hook outcomes (`hooks.md`). This
   avoids requiring an `EvalID` at all on the path where the register already
   happened before a restart (decision 2's crash-recovery case, where there
   is no fresh eval to poll), and needs no new `nomadx.Evaluation` wrapper.
   Both paths are covered by an integration test against `nomad agent -dev`.
4. **The apply timeout counts from the `→ applying` event.**
   `Store.AppliedSince(ctx, id string) (time.Time, error)` reads the
   timestamp of that deployment's `→ applying` row in `events` (already
   written by `Transition`, no schema change). This survives a restart, same
   rule as hook timeouts ("a restart does not extend it").
5. **An apply that does not become healthy in time is passive.** Only
   `Transition` to `failed` with a message, exactly like a hook timeout — no
   `nomad deployment fail` call, no automatic revert. This follows from
   [architecture.md](../architecture.md#what-nops-does-not-do) ("no rollback,
   no explicit stops") and, more generally, from nops's role: it automates a
   manual GitOps process, so what Nomad already does better (rolling update,
   canary, `auto_revert` on the job's own `update` stanza) is left entirely
   to Nomad — nops never steps in.
6. **A `failed` deployment past the register blocks retries for its
   `spec_hash`, regardless of the live index.** Without this, a job with
   `auto_revert` on can loop forever: nops applies, the apply fails to become
   healthy, Nomad reverts the job on its own (changing the live index),
   detection sees drift again with a *different* index so the existing "same
   `spec_hash` **and** same `cas_index`" retry rule no longer applies, and
   nops re-applies the same spec that already failed — repeating the failure,
   the event and the notification every cycle. The rule: once a deployment's
   `AppliedIndex != 0` (the register was reached) and it ends `failed`, no new
   deployment is created for the same job while the latest one has that same
   `spec_hash`, whatever the live index is now. Only a new commit (a
   different `spec_hash`) unblocks it — consistent with invariant 4 (git is
   the source of truth) and with decision 5 above: nops does not retry a
   spec Nomad has already rejected, it waits for a human to change it. This
   extends, rather than replaces, the existing rule for a `failed`/`rejected`
   deployment that never reached the register (state-machine.md's "not
   retrying an unchanged failure"), which still compares the live index too.
7. **Blocked drift is visible in the dashboard.** `Observation` (`engine.go`)
   gains `BlockedBy` (the ID of the deployment whose retry rule — either the
   existing one or decision 6 above — currently suppresses a new deployment
   for this job) and `BlockedReason` (a message naming that deployment and
   what unblocks it: "the deployment `<id>` failed after applying this spec;
   a new commit is needed to retry" or "... failed on the same live job;
   nothing changed since"). Detection fills them for free, since it already
   evaluates the retry rule at that point; `web` renders them next to the
   plain drift so a job that is not converging is never silently stuck.
   Retrying without a new commit (an explicit dashboard action) is out of
   scope here: nothing above prevents adding one later, and doing so would be
   a thin wrapper over the same "create a deployment" path detection already
   has.
8. **Recovery is `RunApply`'s first cycle.** Every apply step above is
   resume-safe on its own — the goroutine loop just calls the same
   per-deployment step function again after a restart, and `ListActive`
   already returns every non-terminal deployment. `RunApply` already runs one
   cycle before its ticker starts, so a crash is noticed immediately rather
   than after up to one `EngineInterval`: `engine-recovery` added no startup
   function, exported nothing and needed no locking (`startApply` keeps two
   overlapping callers off the same deployment). The cycle stays asynchronous,
   one goroutine per deployment: a hook step blocks until its hook ends, and a
   blocking startup pass would delay the dashboard and detection. What the task
   added is the crash-window tests (`recovery_test.go`) and one fix they
   exposed: the allocations fallback of decision 3 read the live job's
   allocations even when the live job had been modified outside nops after our
   register (for instance while nops was down), so someone else's healthy
   version could complete our deployment. The fallback now requires the live
   index to still be `applied_index`, and fails the deployment otherwise.

## Tests

`internal/engine`: a fake `Nomad`/`Hooks` and a real, temp-file `store.Store`,
table- and scenario-driven over every case in
[Per-deployment step](#per-deployment-step) above (both hooks
skipped/declared/failed/timed out, CAS conflict, already-applied-after-crash,
Nomad deployment failed, apply timeout, the detection race in
[Races with detection](#races-with-detection)), the anti-loop rule and
`BlockedBy`/`BlockedReason` from decisions 6 and 7 (a job whose latest
deployment failed after applying stays blocked across a live-index change,
and unblocks on a new `spec_hash`), plus the goroutine loop never starting the
same deployment twice. `recovery_test.go` restarts the engine (a fresh
`Engine` over the same store and fake Nomad) and checks where the first
`RunApply` cycle takes a deployment left in every state a crash can leave: a
table for `detected`, `pre_hook`, `applying` (before the register, after it
before `applied_index`, a conflict found only after the restart, a CAS conflict
on the resumed register, healthy or timed out or modified outside nops while
down) and `post_hook`; `pending_approval` and other namespaces left alone; and
a real `hooks.Runner` over a fake hook Nomad for a crash between `Dispatch` and
the saved dispatched job ID (the child is adopted, or the hook fails, and is
never dispatched again). `tests/integration`: one full cycle against a real
`nomad agent -dev` for a job with no hooks (register → healthy →
`completed`) and one with both hooks, plus whichever health path from
decision 3 is not already covered by an existing `nomadx`/`hooks` integration
test.
