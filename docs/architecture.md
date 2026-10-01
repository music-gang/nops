# Architecture

How Nops works inside: what each package does, the loops that move a
deployment forward, and the reason behind each choice, in a sentence or two.
The states a deployment goes through and the rules between them are in
[deployment lifecycle](deployment-lifecycle.md); the principles and
invariants every part keeps, in [philosophy](philosophy.md). The plans these
parts were built from, with every alternative that was weighed, are in the
[archive](archive/).

## Components

| Package | Role |
|---|---|
| `cmd/nops` | Entrypoint: wiring of config, store, engine, web. Starts the git watcher, detection and apply loops in their own goroutines and the dashboard's `http.Server`, all off one `context.Context` cancelled on `SIGINT`/`SIGTERM`, and waits for them to unwind before exiting. |
| `internal/config` | Flags and env vars (`NOPS_*`), validation. |
| `internal/gitwatch` | In-memory clone, polling, webhook trigger ([below](#reading-the-repository)). Read-only: Nops never writes to git. |
| `internal/nomadx` | Nomad client wrapper: parse, plan, CAS register, dispatch (and lookup of a dispatched job by token), allocations, deployments, promote, stop. Register has no variant without the index check, and Nomad's plain HTTP 500 errors become sentinels (`ErrCASConflict`, `ErrJobNotFound`). It is not bound to a namespace: every call names its own. |
| `internal/meta` | Parsing and validation of the `nops_*` meta keys: the [source of truth](meta-keys.md) for the HCL syntax. |
| `internal/store` | SQLite, embedded migrations: see [deployment lifecycle](deployment-lifecycle.md#schema). |
| `internal/engine` | [Detection](#detection) parses, plans, and creates, supersedes or revalidates deployments; [apply](#apply) advances them, and its first cycle is recovery. It declares its own small interfaces over the packages it uses (`Nomad`, `Store`, `Snapshots`, `Hooks`, `Notifier`), implemented by `nomadx`, `store`, `gitwatch`, `hooks` and `notify`. |
| `internal/hooks` | Dispatch, wait, timeout and stop of hook jobs: `Runner.Run` is blocking, idempotent and resumable, and is driven by `engine`. |
| `internal/web` | Login, OIDC or local users depending on `-auth-mode` ([dashboard](dashboard.md#authentication)), the dashboard (`net/http` + `html/template`) and the git webhook. |
| `internal/metrics` | [`/metrics`](metrics.md) for Prometheus: reads the store and the in-memory state of the engine, the git watcher and the notifier on every scrape, and keeps nothing of its own. The only package that imports the Prometheus client. |
| `internal/notify` | [Notifications](logs-and-notifications.md#notifications) (which transitions send one is defined there), through built-in adapters (generic webhook, Discord, Slack, ntfy, Gotify). A failed delivery is a WARN, never an error for the engine. |
| `internal/version` | Which build is running: the release tag linked at build time, else what the Go toolchain recorded ([releasing](development.md#releasing)). |
| `internal/redact` | Removes secret values from the plan diff before it is saved or shown ([rules](dashboard.md#secret-redaction)). A pure function, called by `engine` at detection. |

## Execution model: active, not lazy

Nops moves deployments forward on its own, without waiting for anyone to open
the dashboard. There are three loops, all with configurable intervals:

1. **Detection.** Runs once at startup, then on every new commit, on every
   `-drift-interval` tick (for drift on the Nomad side), and whenever it is
   asked for: when a deployment ends through apply (`completed` or `failed`),
   when someone rejects one, retries a job, or pauses or resumes it. A request
   while a cycle is already queued is enough (a channel of size 1). Without
   those requests, what the dashboard says about a job (in sync, blocked, its
   diff) would be the last cycle's, taken before the apply, for up to five
   minutes.
2. **Apply.** Every `-engine-interval`, picks up the non-terminal deployments
   and advances them: hooks, register, waiting for healthy, timeouts.
3. **Recovery** at startup: apply's first cycle, which resumes whatever was
   left half-way ([deployment lifecycle](deployment-lifecycle.md#recovery-after-a-crash)).

The only state that waits for an external event with no time limit is
`pending_approval`, by design (invariant 3 in [philosophy.md](philosophy.md)).
The other is a part of `applying`: while a job's Nomad deployment waits for a
person to promote its canaries, the apply timeout does not run
([promotion wait](glossary.md#deployment-lifecycle)).

## Reading the repository

`gitwatch` keeps the repository in memory, read-only (invariant 5), and tells
the engine which files exist at which commit. It does not parse HCL and does
not decide what a file is.

**Which files are read.** Every file named `*.nomad.hcl` or `*.nomad` (the
older name `nomad job init` used), in any subdirectory of `-git-path` (default:
the repository root). The **vars file**, optional, is `<name>.vars.hcl` in the
same directory, `<name>` being the file name without `.nomad.hcl` or `.nomad`:
`api.nomad.hcl` or `api.nomad` ↔ `api.vars.hcl` (when both job files exist,
they share it). Every other file is ignored without a log line; a `*.vars.hcl`
with no job file of the same name is a WARN, once per commit.

**What the vars file is for.** A job can declare HCL2 variables
(`variable "image_tag" {}`, used as `var.image_tag`). The CLI takes their
values from `-var-file`; Nops runs no CLI, it sends the job text to
`/v1/jobs/parse`, which takes the values in its `Variables` field
([meta keys](meta-keys.md#syntax-and-parsing)). Without a vars file every
variable needs a default, otherwise the parse answers `Unset variable` and the
job is skipped with an ERROR. The file is in git, so it never holds a secret:
secrets come from Nomad Variables or Vault through a `template`.

**Job, hook or neither is decided by content.** The engine parses each file
through Nomad (the only HCL interpreter) and classifies it by its meta:
`nops_role = "hook"` is a hook, `nops_managed = "true"` a managed job,
anything else is ignored. A hook named by `nops_pre_hook`/`nops_post_hook` is
looked up by its **parsed job ID** among the hooks of the declaring job's
namespace, never by file name. Two files that parse to the same job (same
namespace and ID) are both ignored, with an ERROR: Nops cannot tell which one
is meant.

**Fetching.** On every tick or trigger, `gitwatch` lists the remote refs (cheap)
and compares the branch head with its snapshot's commit; nothing more happens
when they match. When they differ it makes a **fresh shallow clone** (depth 1,
single branch, no checkout, in memory), reads the tree, and drops the old one:
memory stays constant, and a force-push is just a new head. A token is sent as
HTTP basic auth, never in the URL, so it never appears in an error or a log
line. Triggers from the webhook coalesce (a channel of size 1).

**Failures.** A failed initial clone stops Nops: there is nothing to work on.
A later failed fetch is an ERROR; the last good snapshot is kept, the failure
is shown on the Overview with how stale the snapshot is, and the next tick
retries. Detection keeps running on that snapshot, so drift on the Nomad side
is still seen. A `-git-path` missing from a new commit is a failed fetch,
never "every job was removed".

## Detection

A cycle:

1. **Parses** every file of the current snapshot through Nomad, with a cache
   keyed by the hash of the file and its vars, so an unchanged file is not sent
   again on every drift tick. Only a success is cached: a failure may be Nomad
   unreachable for a moment, and is retried every cycle.
2. **Classifies** each parsed job (above). Every meta issue is logged at its
   severity, whatever the file turns out to be.
3. For every managed job: **plans** it, **freezes** the hooks it declares
   ([hook revisions](#hook-revisions)), **revalidates** or supersedes its
   existing deployment, and **creates** a new one if there is still drift to
   apply.
4. **Supersedes** the waiting deployment of any managed job that left the
   repository ("job removed from repo"): Nops never deregisters a job on its
   own.
5. **Looks for orphans**, and **collects** the hook revisions no deployment in
   progress needs.

A store failure aborts the whole cycle (invariant 7), and so does a failure to
redact a diff (an unredacted diff is never stored). Any other failure (a file
that does not parse, a job Nomad cannot plan, a revision that cannot be
stopped) is logged and scoped to that one file or job; the cycle goes on.

### Per job

In order, for each managed job:

1. A deployment in `pre_hook`, `applying` or `post_hook` is left alone: it
   belongs to [apply](#apply).
2. A deployment in `detected` or `pending_approval` is
   [revalidated](deployment-lifecycle.md#revalidating-pending-deployments):
   superseded when the policy became `none`, when `spec_hash` changed, or when
   the live job changed outside Nops; completed when there is no drift any
   more. A `detected` one under `approval` (left by a Nops that created it in
   two writes) is moved now to `pending_approval`, or to `failed` when a hook
   it declares is missing; a `detected` one under `auto` whose job is
   [held](deployment-lifecycle.md#holding-a-job) is superseded with the hold
   as the reason. Otherwise it stays, still approvable.
3. With policy `none`, or no drift, nothing more happens.
4. If the job is [blocked](deployment-lifecycle.md#not-retrying-an-unchanged-failure),
   nothing more happens.
5. If the job is held, nothing more happens: the drift and the hold are shown,
   no deployment is created. The block comes first, so a job that is blocked
   and held shows its block and its *Retry*.
6. Otherwise a deployment is created with the redacted diff, the full spec and
   the live index as `cas_index`, **in the state it waits in**: `failed` if a
   declared hook is missing or invalid, `pending_approval` under `approval`,
   `detected` under `auto`. One write, with its frozen hooks: created
   `detected` and moved in a second write, a deployment with a missing hook
   could be applied in between.

### What a cycle keeps, and where

- **Observations live in memory.** `Engine.Observations()` is the last cycle's
  view of every managed job, whatever its policy: its drift (redacted), its
  hooks, its hold, its block. It is rebuilt from scratch every cycle, so a job
  that leaves the repository disappears from it with nothing to expire. This
  is how the drift of a `none` job is shown without a table for it: the next
  cycle, seconds later, recomputes it anyway. `Engine.Status()` says how the
  last cycle went (when, how long, the error that aborted it, how many jobs,
  plans that failed, files that did not parse), because a cycle that logs an
  ERROR for every job looks, from the dashboard, exactly like a cluster with
  nothing to do.
- **The commit is recorded on the deployment** (`commit_subject`,
  `commit_author` next to `commit_sha`): the clone is shallow, so a page about
  a deployment from three commits ago cannot ask git.
- **`job_spec` keeps the full, unredacted spec** Nops will register, exactly as
  read from git (after the scaling substitution below). Apply needs the exact
  spec that was planned and, under `approval`, the one a person saw; re-reading
  git at apply time would not work, since `gitwatch` keeps only the head. The
  database is on the same host as Nomad's own copy of the job; it and its
  `-wal`/`-shm` files are kept at permission `0600`, and the dashboard never
  renders `job_spec`.

### Rules detection keeps

- **A job's namespace is the one Nomad parses from its HCL**, `default` when
  there is none. A job in a namespace that is not in `-nomad-namespaces` is an
  ERROR, counts as a file that did not parse, and nothing about it reaches
  Nomad. A job is `(namespace, ID)` everywhere, so the same ID in two
  namespaces is two jobs. Apply, supersede, the orphan check and the revision
  GC only look at managed namespaces: a namespace taken off the list is left
  as it is ([running Nops](running-nops.md#namespaces)).
- **Supersede is keyed on `spec_hash`, never on the commit**, so a commit that
  does not touch a job's file does not disturb its pending deployment.
- **`spec_hash` is computed before the live-cluster adjustment.** A task group
  with a `scaling` block takes its count from the live job before the plan, so
  Nops does not fight the autoscaler; the hash is computed on the job as git
  has it, so an autoscaler never changes the hash of a pending approval.
  *Known limit:* a scale through Nomad's API also bumps the live job's index
  (`TestScaleChangesTheLiveJobsIndex`), so a scale between detection and
  approval still supersedes the pending deployment, and an apply that meets
  it fails once with a conflict before the next cycle creates a fresh one.
  Accepted while the scaled jobs are under `auto`, where it costs at most one
  spurious `failed`; to revisit before such a job moves to `approval`.
- **A hook declared but missing, or with invalid meta, fails the deployment at
  detection**, with a notification, rather than after an approval that could
  not succeed. It is part of `spec_hash` as an absence, so adding the file or
  fixing its meta unblocks the job by itself. The check `hooks.Runner` does at
  dispatch (a registered, parameterized `batch` hook) stays: a file can be
  valid HCL and still not a valid hook.
- **A file that does not parse suspends everything that reads an absence.** It
  looks exactly like a removed one, so a cycle with any file Nomad could not
  parse (or in an unmanaged namespace) neither supersedes "removed" jobs nor
  looks for orphans. The removal is acted on by the first cycle in which every
  file parses. Meanwhile `Approve` refuses a deployment whose job is not among
  the jobs the last cycle could read and plan (`ErrNotInRepo`): removed from
  git while a broken file elsewhere suspends the removal, that file or the
  job's own not parsing, two files with the job's ID, a Nomad call for the job
  failing, or no cycle run yet after a start. Approving it would register a job
  git no longer asks for. It can still be rejected.

### Orphan jobs

A job Nops deployed and that was then removed from the repository stays live
in Nomad: Nops never deregisters a job it deploys. Rather than let that pass
unseen, every cycle lists **orphans** (`Engine.Orphans()`, in memory like the
observations), reported on the dashboard and never acted on: stopping the job
in Nomad, or putting the file back, clears the report. A job is an orphan when
all of these hold:

- Nops has a `completed` deployment for it: only what Nops put into production
  is its business;
- it is **not among the jobs parsed from the snapshot**, whatever their
  classification (a job still in git without `nops_managed` means "hands off",
  not "removed"; a renamed file is the same job), matched on `(namespace, ID)`;
- in Nomad it exists, is not stopped and is not `dead` (a purged job, one
  stopped by hand, or a finished batch job with no schedule is not reported).

A cycle with a file that does not parse keeps the orphans of the last complete
check, and the Overview says the check was skipped: clearing them would read
"all fine" exactly when something is wrong. A Nomad failure on one candidate
skips that job, not the cycle. The cost is one read per candidate per cycle,
not cached: the list is short and what matters is what Nomad says now.

### Not covered by a rule

- **A job stopped by hand** (`nomad job stop`) plans as a difference from git.
  Under `auto` Nops starts it again: git is the source of truth (invariant 4).
  Use `approval` or `none` for a job you mean to stop by hand.
- **A hook registered by hand under an ID that looks like a revision** is
  stopped by the revision GC ([hooks](hooks.md#hook-revisions)).
- **A deployment made before hook revisions existed** and already in a hook
  phase has no frozen hook: it fails at its hook step, saying so.

## Apply

Every `-engine-interval` the apply loop lists the non-terminal deployments and
starts one goroutine per deployment it is not already working on (an
in-process set guards against two overlapping ticks; which *job* has an active
deployment is the database's to say, invariant 6). `pending_approval` is never
advanced by this loop (invariant 3): only `Approve` or `Reject` moves it.

Every step saves its state first and only then calls Nomad (invariant 7), so
it can be resumed from the store alone: that is what makes recovery nothing
more than running the same step again. A Nomad or SQLite error is logged at
ERROR and the deployment stays where it is for the next tick; only a hook's
result, a CAS conflict or a timeout closes it. Every `failed` notifies once,
right after the transition commits.

### The steps

- **`detected`** (policy `auto`): to `pre_hook` if the job declares pre-hooks,
  else to `applying`. A `detected` deployment under `approval` is left alone
  (detection moves it), and one whose job is paused or outside its sync window
  is left `detected` while detection is asked for a cycle, which puts it
  aside: a failed read of the pause waits for the next tick rather than start
  on a guess.
- **`Approve`** refuses a `spec_hash` other than the deployment's (an approval
  never transfers to another spec), a paused job (`ErrPaused`) and a job not in
  git (`ErrNotInRepo`, above); then applies the same rule as `detected`, with
  the person as `decided_by`. **`Reject`** is a plain move to `rejected`. The
  dashboard calls only these two.
- **`pre_hook`** / **`post_hook`**: the frozen hooks of the phase run in order
  ([hooks](hooks.md#several-hooks)); each is registered as its revision
  ([below](#hook-revisions)) and run by `hooks.Runner`, with its frozen
  `nops_timeout`. A step retried after an error starts again from the first
  hook, and a hook that already finished answers from its stored result
  without touching Nomad, so the phase resumes at the first that had not. A
  failed or timed-out pre-hook fails the deployment with the live job
  untouched; a failed post-hook says the apply is already live and is not
  undone.
- **`applying`, before the register**: re-read the live job. If its index is
  still `cas_index`, plan again (invariant 1 applies here too: time passed
  waiting for approval or the pre-hook); an empty plan completes the
  deployment as a no-op, otherwise register at `cas_index` (invariant 2).
  A CAS conflict fails it ("job modified outside Nops"). If the index moved
  *and* the plan of our spec is already empty, the register went through
  before a crash: carry on without registering again. Any other mismatch
  fails it. After the register, the live job is read again for
  `applied_index`: the index in the register's own response is not reliable.
  A Nomad error before the register goes through is retried until the apply
  timeout, then fails the deployment: an error that never heals (a token
  without `submit-job` or without a volume's rule, a spec Nomad refuses)
  would otherwise hold the job's only active slot for good.
- **`applying`, after the register**: wait until the job is
  [healthy](#healthy), within the apply timeout. Healthy → `post_hook`, or
  `completed` without post-hooks. A Nomad deployment `failed` or `cancelled`,
  or the timeout, fails it, and nothing more: no rollback, no
  `nomad deployment fail` (see [below](#apply-and-downtime)).

### Healthy

- **With a Nomad deployment** that tracks this apply (its `JobModifyIndex` is
  `applied_index`): `successful` is healthy, `failed` or `cancelled` fails,
  anything else keeps waiting (`TestEngineApplyWaitsForTheNomadDeployment`).
- **Without one** (a `batch` job, an `update` stanza that produces none): every
  allocation of the applied job version is `running` or `complete`, and only
  while the live job's index is still `applied_index` (otherwise it was
  modified outside Nops, perhaps while Nops was down, and its allocations are
  not ours: the deployment fails). `failed` or `lost` fails, anything else
  keeps waiting (`TestEngineApplyAgainstRealNomad`). This is deliberately weak:
  no health check, no comparison with the counts (a group only partly placed
  reads as healthy).
- **A job that never has allocations** (periodic, parameterized, or with every
  group at count 0) is healthy once registered at `applied_index`: waiting for
  one would fail it at the timeout.
- `system` and `sysbatch` jobs take the same paths
  (`TestApplyOfASystemOrSysbatchJobCompletes`).

### The apply timeout

`-apply-timeout` counts from the deployment's `→ applying` event, so a restart
does not extend it, and bounds the register and the wait for health alike.

**A canary waiting for a person is a wait of its own, with no timeout.** A job
with `canary > 0` and `auto_promote = false` leaves its Nomad deployment
`running` until someone promotes it, and Nomad does not fail it for that
(`TestCanaryWaitsForManualPromotion`). Counted like any other wait, the
timeout would fail the deployment, block the job, and a later promotion would
find a `failed` deployment whose post-hooks never run. So when every group
with canaries that does not auto-promote has all of them placed and healthy
(or already promoted), and at least one is not promoted yet, the deployment is
in a **promotion wait**: recorded once (`promotion_wait_since`, with its
notification), and the timeout does not run. When the canaries are promoted
(`promoted_at`) the timeout **restarts**, a full `-apply-timeout` for the rest
of the rollout: canaries healthy at 9m50s of 10m would otherwise leave seconds
for it. Canaries still starting keep the timeout. The wait has no ceiling, so
it holds the job's only active slot until someone promotes or fails the Nomad
deployment (accepted, like `pending_approval` having no expiry).

**Promote** (the button on a deployment in a promotion wait) promotes every
group of the Nomad deployment, like `nomad deployment promote`. It changes no
spec, so plan and CAS do not apply; it follows invariant 7 the way a hook stop
does: the request is recorded first (an event with the person), then Nomad is
asked. It refuses unless the deployment is waiting and Nomad still says so
now (its latest deployment tracks `applied_index`, is `running`, and the
canaries are healthy and not promoted), so a page a few seconds old cannot
promote what someone already promoted. Once Nomad accepts, the promotion is
recorded at once, so the timeout restarts from the click.

### Races with detection

Detection only moves a deployment out of `detected` or `pending_approval`;
apply only moves it out of the others. The one overlap is a `detected`
deployment that detection supersedes as apply starts it: both change the
state with a compare-and-set on the old state, exactly one wins, and the
loser drops it for this tick (INFO, not ERROR: an expected race).

## Hook revisions

An approval must cover what runs, and what runs includes the hooks.

**Freezing.** For every hook a job declares, detection hashes the hook's spec
as parsed from the same commit and freezes it next to the deployment, in the
same transaction: the hook ID, the hash, the spec, and the **revision**
`<hook-id>-<first 8 hex of the hash>`. The deployment's `spec_hash` is then the
SHA-256 of the target's hash followed by one line `phase:hook-id:hash` per
declared hook (pre before post, in the order listed), with `missing` or
`invalid` for a hook that cannot be used; with no hooks, it is the target's own
hash. Everything that compares `spec_hash` therefore compares the hooks too
(what that means for a hook's author: [hooks](hooks.md#hook-revisions)).

**Registering right before the dispatch.** Detection registers nothing of a
hook. When a deployment reaches a hook, apply reads the revision's job, plans
the frozen spec under the revision's ID (invariant 1) and, if the plan shows a
difference, registers it at the index it just read, `0` when there is none
(invariant 2). A revision already there and identical costs nothing; one the
GC stopped shows as a difference and is registered again. Two deployments
registering the same revision at once are safe: one gets a CAS conflict, is
retried and finds nothing to do. A revision that cannot be registered is an
ERROR retried next cycle, and fails the deployment once the hook's timeout
has passed, counted from when the previous hook finished (or the phase
started). So a hook that was never approved never reaches the cluster, and two
deployments sharing a hook at different specs cannot change what the other
dispatches: Nomad has no CAS on a dispatch.

**Collecting.** Every detection cycle ends by stopping, **without purge**, the
revisions no deployment in progress needs: the jobs with `nops_role = "hook"`,
an ID ending in `-` and 8 lowercase hex digits, no parent (a dispatched run
inherits its revision's meta) and not already stopped, minus the revisions of
the non-terminal deployments. The store is read after Nomad, so a deployment
created in between that needs a revision this pass stops registers it again
at its hook step. A Nomad failure is an ERROR on that job, retried next cycle;
a store failure aborts the cycle. What this relies on is checked against a
real Nomad: stopping a parameterized parent leaves the runs it dispatched
readable, even a running one (`TestStoppingAHookRevisionLeavesItsRunningRun`);
a stopped parent shows as a plan difference, and a job registers under an ID
different from its `Name` (`TestHookRevisionRegistration`); an ID of several
hundred characters is accepted (`TestLongJobIDIsAccepted`).

**Dispatching.** Nops dispatches with the idempotency token
`<deployment_id>:<phase>:<position>` (a run created before positions existed
keeps `<deployment_id>:<phase>`). The same token returns the same dispatched
job, even after it has finished (`TestDispatch`), for as long as the
dispatched job exists. Nops saves the run as `running` **before** it sends the
dispatch, and the dispatched job's ID right after. A run found `running`
without that ID may or may not have reached Nomad, so Nops looks the
dispatched job up by its token and carries on with it; if there is none, the
run is `failed` ("outcome unknown") rather than dispatched again, since a
dispatched job that ran and was garbage-collected no longer deduplicates, and
the hook would run twice. The same holds when a saved dispatched job
disappears before Nops saw its outcome.

## Apply and downtime

Apply is **just a CAS register**, the same effect as `nomad job run`: the
update (rolling, canary or destructive) is carried out by Nomad according to
the job's `update` stanza. Nops issues no explicit stops: they would add
downtime and break the CAS sequence. What Nomad already does better (rolling
updates, canaries, `auto_revert`) is left to Nomad, which is also why an apply
that does not become healthy only fails, and never reverts.

## What Nops does not do

- It does not write to Git, and does not write meta into the live job.
- It does not roll back automatically and does not deregister jobs (for now): a job removed from the repository is reported as an [orphan](#orphan-jobs), never stopped. The only things it stops are a hook run that timed out and the [hook revisions](#hook-revisions) no deployment needs.
- It does not resolve nodes for hooks: placement is decided by the scheduler
  (see [hooks](hooks.md#placement)).
