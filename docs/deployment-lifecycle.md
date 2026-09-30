# Deployment lifecycle

Every deployment goes through a state machine persisted in SQLite: the states,
the rules that move a deployment between them, what holds a job back, the
tables behind it, and how a restart resumes. How the loops that apply these
rules work is in [architecture](architecture.md).

```
detected ──(policy=auto)──────────────┐
   │                                  ▼
   └─(policy=approval)→ pending_approval ──approve──→ [pre_hook] → applying → [post_hook] → completed
                             │                          │            │            │
                           reject                     fail/timeout  fail        fail/timeout
                             ▼                          ▼            ▼            ▼
                          rejected                    failed       failed       failed
```

- `none` creates no deployment: drift is only an observation in the dashboard.
- States in `[ ]` are skipped when the hook is not defined.
- Detection **creates a deployment in the state it waits in**, in one write: `failed`
  when a hook it declares is missing or invalid, `pending_approval` under
  `approval`, `detected` under `auto`. Its events read the same as if it had been
  created `detected` and moved on (`detected`, then `detected -> <state>`), but
  no apply step and no restart can ever find it `detected` in between. This
  matters most for `failed`: the deployment froze only the hooks it found, so
  one that could be applied would run without the rest.
- The `pre_hook` runs **after** approval: migrations and backups must not start
  before the human OK, and the backup ends up fresh.
- A post-hook failure does not undo the apply (the job is already live):
  `failed` with an explicit message and a notification, no automatic rollback.
- Terminal states: `completed`, `failed`, `rejected`, `superseded`. They have
  no outgoing transitions.

## Supersede

A newer commit on the same job moves a deployment that is still in `detected`
or `pending_approval` to `superseded`. If the deployment is already in
`pre_hook`/`applying`/`post_hook`, nothing is created and the next cycle
retries.

## Revalidating pending deployments

On every detection cycle, for each `detected` or `pending_approval`
deployment (in that order; see [architecture](architecture.md#per-job)
for the full per-job procedure):

- if the job's policy is now `none`, it moves to `superseded` (reason:
  "policy changed to none");
- else if the target spec changed, it moves to `superseded` (reason: "newer
  spec at commit `<sha>`") — this compares `spec_hash`, never `commit_sha`: an
  unrelated commit that does not touch the job's file must not disturb it;
- else if the live `JobModifyIndex` differs from `cas_index`, it moves to
  `superseded` (reason: "job modified outside Nops");
- else if the plan is empty, it moves to `completed` as a no-op (reason:
  "already in sync").

In every `superseded` case, a new deployment is created right after if there
is still drift and the job's policy allows one (see
[policies](policies.md)); creation follows the normal rules, including a
retry check below.

This way a pending deployment shown in the dashboard is always approvable.
There is no time-based expiry of pending deployments: this revalidation is
enough.

## Not retrying an unchanged failure

A new deployment is not created for a job whose most recent one is `failed`
or `rejected` with the same `spec_hash` **and** the same live index
(`cas_index`): nothing that produced that outcome has changed, so recreating
it would only repeat the same failure, the same event and the same
notification on every cycle. A new commit, or a change to the live job
(including Nops's own apply), makes a new deployment again.

A deployment that reached the register (`applied_index != 0`) and then
`failed` blocks for its `spec_hash` regardless of the live index: Nomad's own
`auto_revert`, for example, would otherwise move the live job back on its own
(a different index) right after an apply that never became healthy, letting
the rule above retry a spec Nomad has already rejected — apply, revert,
re-detect, apply again, forever. Only a new commit (a different `spec_hash`)
unblocks it. Either case is visible in the dashboard as `Observation.BlockedBy`/
`BlockedReason` for a job whose policy would otherwise create a deployment
(see [architecture](architecture.md#rules-detection-keeps)): the job is
not silently stuck.

A human can lift the block without a new commit: **retry** (`Engine.Retry`,
`POST /jobs/{namespace}/{job}/retry`). It marks the blocking deployment as
retried (`retried_by`, `retried_at`; the state does not change) and asks
detection for a cycle, and a *retried* deployment blocks nothing. The next
cycle creates a new deployment like any other, so the policy still decides
what happens: under `approval` it is `pending_approval` and waits for a
human, under `auto` it proceeds. A retry is one more attempt, not a promise:
if the new deployment fails the same way, the job is blocked again. See
[the retry](#not-retrying-an-unchanged-failure).

## Holding a job

A **hold** is a condition that keeps Nops from *starting* a deployment for a
job. It is one gate in the engine, asked one question (`Observation.Hold`), with
two sources: a person's **pause**, and a **sync window** that is closed
([policies](policies.md#sync-windows)). It only ever subtracts: it creates,
approves and advances nothing, so it cannot apply anything invariant 3 would not
allow. The window is read from git, and what a pause reads is a row in SQLite
that a person wrote, which changes neither the policy nor the hooks git gives
([philosophy](philosophy.md), invariant 4).

While a job is held:

- detection still plans it and shows the drift, and the job shows as **Paused**
  (whether or not it drifts: a forgotten pause must show), or **Held** for a
  closed window (only while it drifts), but **no deployment is created**. A
  pause holds under `auto` and under `approval` alike; a window only under
  `auto`, since under `approval` a person's OK is the gate;
- a deployment still `detected` (an `auto` one, created before the hold) is
  moved to `superseded` with the hold as the reason, and the apply loop does
  not start one it finds `detected` in the meantime (it asks detection for a
  cycle and leaves it). When the hold lifts, detection plans again, so what is
  applied is never a plan from before it: the state machine gains no state;
- a `pending_approval` deployment stays as it is, and **Approve is refused**
  (`ErrPaused`) until the job is resumed. *Reject* still works: it starts
  nothing. A window never holds an approval;
- a deployment already in `pre_hook`, `applying` or `post_hook` finishes: the
  hold gates the start, never what is running (a pre-hook is the start);
- a job whose drift a failed or rejected deployment
  [blocks](#not-retrying-an-unchanged-failure) stays blocked, and is listed as
  such in Needs attention with its *Retry*: the hold hides nothing. A window
  shows it as **Blocked**, not Held, since opening would not deploy it; a pause
  still shows as **Paused**. A retry while the job is held lifts the block and
  starts nothing until the hold lifts too.

A window has no actor and no row: it is computed from the job's meta and the
clock every cycle (in `-sync-window-time-zone`), and the apply loop asks it again
of the meta the deployment froze, so a deployment created inside the window is
not started once it has closed. It is noticed at the next detection cycle
(`-drift-interval`) after it opens or closes, and a cycle is also run at each
commit.

**Pause** and **resume** (`Engine.Pause`, `Engine.Resume`,
`POST /jobs/{namespace}/{job}/pause` and `/resume`) are any logged-in user's
action. Each is persisted before anything else (invariant 7), logged at INFO
with the actor, and put on the job's last observation at once, so the page the
person is sent back to already says so; it then asks detection for a cycle,
which recomputes the observation and puts any `detected` deployment aside. A job can be paused only while it is among the
managed jobs of the last detection cycle, and once at a time. There is no
instance-wide pause: stopping Nops does that. See the
[decision log](archive/decisions.md), 2026-09-30.

## Schema

The tables, and what each column that is not self-explanatory means. The
columns themselves are in the migrations, `internal/store/migrations/`.

| Table | What it holds |
|---|---|
| `deployments` | One row per deployment (ID: a ULID): the job (`namespace`, `job_id`), the commit it came from, `spec_hash`, the full spec (`job_spec`, JSON) and the redacted diff (`plan_diff`), the policy, the state, the live index at detection (`cas_index`) and after the register (`applied_index`, `eval_id`), the error, who decided and who retried, and when the Nomad deployment waited for a promotion. |
| `deployment_hooks` | The hooks a deployment runs, frozen when it is created, in the same transaction (invariant 7): per phase and position, the hook ID, its hash, its spec and its revision ([hook revisions](architecture.md#hook-revisions)). |
| `hook_runs` | One run of one hook of one deployment: the revision dispatched, the idempotency token, the dispatched job's ID, the state (`dispatching`, `running`, `succeeded`, `failed`, `timed_out`), the timeout in seconds, the error, and when it started and finished. |
| `job_pauses` | One row per pause of a job: who paused it, when and why, and who resumed it and when. Kept after the resume as the record of it, since `events` belongs to a deployment and a pause has none. |
| `events` | Append-only log of a deployment (`from_state`, `to_state`, `actor`, `message`, time). It feeds the dashboard's timelines and the Activity page. |

What the columns mean:

- `commit_subject` (first line of the message) and `commit_author` (name, no
  email) are copied from the commit when the deployment is created, because
  the git clone is shallow and only ever holds the head; they are empty for a
  deployment created before they were recorded.
- `retried_by`/`retried_at` are set by `Store.MarkRetried`, only on a `failed`
  or `rejected` deployment.
- `promotion_wait_since`/`promoted_at` are set by `Store.MarkAwaitingPromotion`
  and `Store.MarkPromoted`, only on an `applying` deployment whose Nomad
  deployment waited for a person to promote its canaries: the first time Nops
  saw it wait and the first time it saw the canaries promoted. Each is written
  once, and both are empty for a deployment that never waited.
- In `deployment_hooks`, `position` orders the hooks of a phase, from 0, as
  they are listed in the meta. A deployment made before this table existed has
  none: if it reaches a hook step it fails, saying so.
- In `hook_runs`, the idempotency token is `<deployment_id>:<phase>:<position>`;
  a row that predates positions keeps `<deployment_id>:<phase>`, the token it
  was dispatched with.
- `events` also records what moves no state: `Store.MarkRetried` appends one
  whose two states are the deployment's (terminal) one, message
  `retry requested`; `MarkAwaitingPromotion` and `MarkPromoted` append one
  `applying → applying` (`waiting for canary promotion in Nomad`, `canaries
  promoted in Nomad: the apply timeout counts again`); and
  `Store.MarkPromotionRequested` appends one `applying → applying` with the
  person who pressed *Promote* as `actor`, message `promotion requested`,
  before Nomad is asked.

The keys and indexes that carry a rule:

- `UNIQUE INDEX (namespace, job_id) WHERE state IN (detected,
  pending_approval, pre_hook, applying, post_hook)` on `deployments` is the
  per-job lock (invariant 6).
- `PRIMARY KEY (deployment_id, phase, position)` on `deployment_hooks` and
  `UNIQUE(deployment_id, phase, position)` on `hook_runs`.
- `UNIQUE INDEX (namespace, job_id) WHERE resumed_at IS NULL` on `job_pauses`
  allows one pause in force per job, like the per-job lock.

Store rules:

- `Store.Transition` is the only way to change `state`: it validates the
  transition table, requires the starting state (compare-and-set on the state)
  and writes the event in the same transaction.
- `Store.MarkRetried` is the only other write on a terminal deployment: it
  changes no state, is guarded on `state IN (failed, rejected) AND retried_at
  IS NULL`, and writes its event in the same transaction.
- `Store.MarkAwaitingPromotion` and `Store.MarkPromoted` change no state
  either: each is guarded on `state = applying` and on its own column still
  being empty, so it writes (and reports it wrote) once, and writes its event
  in the same transaction.
- `Store.MarkPromotionRequested` changes nothing but that event, guarded on
  `state = applying`.
- `Store.PauseJob` and `Store.ResumeJob` touch only `job_pauses`: a second
  pause of a paused job is `ErrAlreadyPaused` (the index), a resume of one that
  is not paused is `ErrNotPaused`.
- A single SQLite connection: writes are serialized.

## Recovery after a crash

Recovery is the first cycle of the engine loop (`Engine.RunApply`): it runs
right at startup, before the interval ticker, on every non-terminal deployment.
There is no separate recovery function: each step resumes from what is in the
store, so calling it again is the recovery. The cycle does not block the
startup, since a hook step waits for its hook to end, which can take minutes.
`detected` and `pending_approval` need nothing special: `detected` is routed as
usual, `pending_approval` still waits for a human. For the rest:

- `pre_hook`/`post_hook`: run the hook again (`hooks.Runner.Run` is
  idempotent). A run that is already terminal returns its stored result. If
  `dispatched_job_id` is known, go back to waiting for the dispatched job. If the run is
  `dispatching`, nothing was sent yet and it is dispatched. If it is `running`
  without a dispatched job ID, the dispatch may have been sent: look the dispatched job up by its
  idempotency token and adopt it; if there is none, the run is `failed` (outcome
  unknown), never dispatched again. A dispatched job that no longer exists is `failed`
  too. The timeout is counted from `started_at`, not from the restart.
- `applying`: re-read the live job. If `JobModifyIndex == cas_index`, repeat
  the CAS register. If the index has changed and the plan of our spec is empty,
  the apply had already happened and we move on. Otherwise `failed` (conflict).
  A Nomad error on the way is retried, until the apply timeout counted from the
  `→ applying` event, then `failed`.
  To record `applied_index`, re-read the live job: the index in the register
  response is not reliable (see the decision log).
- `applying` with `applied_index` set: wait for health as before, the timeout
  counted from the `→ applying` event, or from `promoted_at` if the Nomad
  deployment waited for a canary promotion; while it waits for one there is no
  timeout at all ([architecture](architecture.md#the-apply-timeout)). If the live job's index is no longer
  `applied_index` and no Nomad deployment tracks it, the job was modified
  outside Nops (for instance while it was down): the deployment is `failed`
  rather than judged on someone else's allocations.

The idempotency token is scoped to the parent job and lives as long as the
dispatched job: if Nomad garbage-collects the dispatched job, a new dispatch becomes possible.
For this reason recovery always prefers the `dispatched_job_id` saved in
`hook_runs`, and a run whose dispatched job has vanished is `failed` rather than
dispatched again. Hook run states: `dispatching` (row created, nothing sent to
Nomad), `running` (saved before the dispatch is sent; the dispatched job ID follows), then
`succeeded`, `failed` or `timed_out`.
