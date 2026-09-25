# engine-detection design

The detection half of `internal/engine` (the [`engine-detection`](../roadmap.md)
task): comparing the repository with Nomad, and creating, superseding or
revalidating deployments. It never applies anything — that is
[`engine-apply`](../roadmap.md#engine-apply).

## Scope

One detection cycle (`Engine.Detect`) runs once at startup, then again on
every `gitwatch.Watcher.Changed()` signal and on every `-drift-interval` tick
(`Engine.RunDetection`), and it is **asked for** (`Engine.kickDetection`, a
channel of size 1: a cycle already queued is enough) when a deployment ends
through apply (`completed` or `failed`), when a human rejects one and on a
retry. Not from the moves detection makes itself (supersede, "already in
sync"), which would only queue a cycle behind the one that is running. The
reason: what the dashboard says about a job (in sync, blocked, its diff) is
the last cycle's observation, taken before the apply, so without it a job
that has just completed reads **Drift** until the next tick (five minutes by
default) and one that has just failed reads Drift instead of Blocked. A cycle:

1. Parses every file of the current `gitwatch.Snapshot` through Nomad
   (`nomadx.ParseHCL`), with a cache keyed by the hash of `(Content, Vars)` so
   an unchanged file is not re-sent on every drift tick.
2. Classifies each parsed job with `meta.Parse`: `nops_role = "hook"` is a
   hook, `nops_managed = "true"` is a managed job, anything else is ignored.
   Two files parsing to the same job ID are both ignored, with an ERROR (see
   [gitwatch](gitwatch.md#job-hook-or-neither-decided-by-content)). Every
   `meta.Issue` found is logged at its severity, whatever the file turns out
   to be.
3. For every managed job: plans it, freezes the hooks it declares (see
   [Hook revisions](#hook-revisions)), revalidates or supersedes its existing
   deployment if there is one, and creates a new one if there is still drift
   to apply.
4. Supersedes the deployment of any managed job that left the repository
   entirely.
5. Deregisters the hook revisions no deployment in progress needs.

## Interfaces

`engine` declares its own small interfaces over the packages it consumes
(the pattern used everywhere else in nops, see
[architecture.md](../architecture.md)):

- `Nomad`: `ParseHCL`, `Job`, `Plan`, `RegisterCAS`, `ListJobs`, `StopJob` (and,
  for apply, `Allocations` and `LatestDeployment`). `*nomadx.Client` implements it.
- `Store`: `CreateDeployment`, `Transition`, `GetDeployment`,
  `ActiveDeployment`, `ListActive`, `LatestDeployment`, `DeploymentHooks`,
  `HookRevisionsInUse` (and a few more for apply and retry). `*store.Store`
  implements it.
- `Snapshots`: `Snapshot`, `Changed`. `*gitwatch.Watcher` implements it.
- `Notifier`: `Notify`. `*notify.Notifier` implements it.

## Decisions

These are recorded in the [decision log](decisions.md); the reasoning is
expanded here.

### `job_spec` keeps the full, unredacted spec

`deployments.job_spec` holds the JSON of the parsed job nops will register at
apply time, exactly as read from git (after the scaling-count substitution
below). It is not redacted, so it can carry the same secrets as the plan
diff. This is accepted, not worked around: the database is local to the host
(same trust boundary as the HCL files and as Nomad's own copy of the job),
apply needs the exact spec that was planned and, for `approval`, the one a
human saw; re-reading it from git at apply time would not work in general,
since `gitwatch` only keeps the current head and a later, unrelated commit
can move past what a still-pending deployment needs.

Mitigation: the database file (and its `-wal`/`-shm` sidecars) are created and
kept at permission `0600` (`store.Open`), and `job_spec` is never rendered by
the dashboard (see [dashboard.md](../dashboard.md#secret-redaction)).

### Drift of a `none` policy job lives only in memory

`nops_policy = "none"` never creates a deployment (see
[policies.md](../policies.md)), but the dashboard still needs to show that
such a job has drifted, with its redacted diff. The schema has no table for
this, and it is not given one: `Engine.Observations()` returns the last
cycle's drift, one `Observation` per managed job (whatever its policy, with
the hooks its meta declares, so a job with no deployment can still show them),
rebuilt from scratch on every cycle. A job that leaves the repository (or
starts failing to parse) simply disappears from the next call: there is
nothing to expire or garbage-collect. Recomputing it costs nothing extra,
since the plan was already run to decide whether to create a deployment.

Not chosen: computing it on demand when the dashboard page loads (a Nomad
round trip per job per page view, and it duplicates this cycle's logic), or a
new table (persistent, but there is nothing here that must survive a
restart: the next cycle, seconds later, recomputes it, and a table would need
its own row lifecycle — created, updated, expired — for a value that is
always a function of the current git head and the current live job).

### A hook declared but missing, or invalid, fails at detection

If `nops_pre_hook` or `nops_post_hook` names (any of the jobs in its list) a job
ID that is not a hook file in the current snapshot, or whose meta has an error
(an invalid `nops_timeout`, say), the deployment is created (freezing the hooks that do
exist) and immediately moved `detected → failed`, with a notification, rather
than waiting for approval first. Asking a human to approve a deployment that is guaranteed to fail at
the hook step serves nobody; the fix (add the hook file, or fix its meta) is
the same either way. This is in addition to, not instead of, the runtime
check `hooks.Runner` already does at dispatch time (the revision must be
registered, a hook, of type `batch` and parameterized), which stays: a hook can
be valid HCL and still not be a valid hook. The message names the first such hook (`pre-hook "x" not found in repo` or
`has invalid meta`). It is part of the deployment's `spec_hash` (as an absence),
so the file appearing, or its meta being fixed, changes it and unblocks the job
by itself.

### Hook revisions

`spec_hash` must cover what runs, and what runs includes the hooks. For every
hook a managed job declares (a comma-separated list per phase, each at its
position) and that is in the snapshot with valid meta, detection computes the
hash of the hook's spec (`specHash`, the same function as for the target, on the
job as parsed from git) and freezes, in `deployment_hooks`, next to the
deployment (same transaction, invariant 7): the hook ID, the hash, the spec and
the **revision** `<hook-id>-<first 8 hex of the hash>`. The deployment's
`spec_hash` is then the SHA-256 of the target's hash followed by one line
`phase:hook-id:hash` per declared hook, pre before post and in the order listed, with `missing` (or
`invalid`, for a hook whose meta has an error) in place of the hash of a hook
that cannot be used; with no hooks declared it is the target's own hash,
so a job without hooks does not change. Everything that compares `spec_hash`
(revalidation, the retry rule and the anti-loop rule) therefore also compares
the hooks, with three consequences that are the point of it:

- a hook changed in git **supersedes** a deployment waiting for approval, which
  has to be approved again: what was approved is not what would run;
- a job blocked by a failed deployment is **unblocked** by fixing its hook,
  without a retry;
- a hook changed while the target is in sync creates **no** deployment: a
  deployment needs drift on the target (invariant 1 is about the target, and
  the hooks only run around an apply).

Detection registers **nothing** of a hook: the revision is registered by apply,
right before the hook is dispatched (see
[engine-apply](engine-apply.md#registering-a-hook-revision)), so a hook that was
never approved never reaches the cluster, and what Nomad holds is not decided by
whichever commit was last seen. This replaces the per-cycle sync of every hook
job, which had two flaws: the approval did not cover the hook (whatever was
registered under its fixed ID at dispatch time ran), and two deployments sharing
a hook could overwrite each other's version between register and dispatch (Nomad
has no CAS on dispatch).

The end of every cycle **garbage collects** revisions (`gcHookRevisions`): the
jobs Nomad lists that have `nops_role = "hook"`, an ID matching
`^.+-[0-9a-f]{8}$` and no parent (a dispatched run has the ID of its revision
plus `/dispatch-…`, and inherits its meta, so it would otherwise match) and are
not already stopped, minus the revisions of `deployment_hooks` of non-terminal
deployments (`Store.HookRevisionsInUse`), are stopped **without purge**. The
list of jobs asks for the meta (Nomad leaves it out otherwise). The store is
read after Nomad: a deployment created in between and needing a revision that
this pass stops registers it again by itself at its hook step. A Nomad failure is
an ERROR on that job (or on the listing), retried next cycle; a store failure
aborts the cycle. Verified on Nomad 2.0.3: deregistering a parameterized parent
without purge leaves its dispatched runs and their logs readable, even one still
running; a stopped parent cannot be dispatched, shows as a plan difference and is
registered again at its own index; a job registers under an ID different from its
`Name`; and an ID of several hundred characters is accepted, so the suffix hits no
limit.

### Retry rule after a `failed` or `rejected` deployment

Without a rule, a job whose deployment keeps failing (a broken pre-hook, for
example) would get a brand new `failed` deployment, event and notification
every single cycle. A new deployment is not created when the job's latest
one (`Store.LatestDeployment`) is `failed` or `rejected` with the same
`spec_hash` **and** the same live index (`cas_index`): nothing that produced
the earlier outcome has changed. A new commit (different `spec_hash`) or a
change to the live job (a different index — including nops's own apply, once
`engine-apply` exists) makes a new deployment again, which matches invariant
2's "a CAS conflict leads to `failed` and a re-detection".

### `spec_hash` is computed before the live-cluster adjustment

A task group with a `scaling` block takes its `Count` from the *live* job
before the plan is run (see below), so nops does not fight the autoscaler
(see [philosophy](../philosophy.md#patterns-reused-from-nomad-gitops)).
`spec_hash` is
computed on the job exactly as parsed from git, before that substitution, so
the autoscaler moving a count never supersedes a pending approval or
triggers the retry rule to reset.

### Supersede is keyed on `spec_hash`, not on the commit

A commit that does not touch a job's file must not disturb its pending
deployment. Every "does the spec still hold" check compares `spec_hash`
(and, separately, the live index), never `commit_sha`: `CommitSHA` on the row
is informational (shown in the dashboard and in notifications), not part of
the identity of what was approved.

### Orphan jobs

A job nops deployed and that is then removed from the repository stays live
in Nomad: nops never deregisters a job it deploys. Rather than let that pass
unseen, every cycle ends by looking for **orphans** and keeping them in
memory (`Engine.Orphans()`, rebuilt every cycle like `Observations()`, with
`Status.Orphans`). They are reported, never acted on: the operator stops the
job in Nomad, or puts the file back, and the report clears by itself.

A job is an orphan when all of these hold:

- nops has a **`completed` deployment** for it in its namespace
  (`Store.LatestCompletedPerJob`): only what nops put into production is its
  business, the rest of the cluster is not;
- it is **not among the jobs parsed from the snapshot**, whatever their
  classification. A job still in the repository without `nops_managed` means
  "hands off", not "removed"; two files with the same job ID are both still
  in git; a renamed file is the same job;
- in Nomad it **exists, is not stopped (`Stop`) and is not `dead`**. A purged
  job (a 404) is not reported, nor is one stopped by hand (that is what the
  report asks for), nor a batch job that has finished and, having no schedule,
  will not run again.

Two things keep it from lying:

- **A file that does not parse suspends the check.** A broken file looks
  exactly like a removed one, so a cycle with any file Nomad could not parse
  does not look for orphans: `Status.OrphanCheckSkipped` is set, the Overview
  says so, and the orphans of the last complete check stay listed as they
  were (clearing them would read as "all fine" exactly when something is
  wrong; the cost is that one stopped meanwhile stays listed until the file
  is fixed).
- **A Nomad failure on one candidate is an ERROR and skips that job**, not the
  cycle; a store failure is returned like any other (never swallow a SQLite
  error).

The cost is one `GET /v1/job/<id>` per job that nops deployed, is not in git
and is not yet purged, every cycle. It is not cached: the list is short and
what matters is what Nomad says now.

### Not covered by a rule (accepted, documented)

- **A job stopped by hand** (`nomad job stop`) plans as a difference from
  git. Under `auto` nops restarts it: git is the source of truth (invariant
  4). Use `approval` or `none` for a job you intend to stop by hand.
- **A hook registered by hand under an ID that looks like a revision** (a
  hook file named `backup-1a2b3c4d`, registered in Nomad by someone) is
  stopped by the GC; it would only be dispatched by nops as a revision, and it
  never registers hooks under their plain ID. Name hooks otherwise.
- **A deployment made before hook revisions existed** and already in a hook
  phase has no frozen hook: it fails at its hook step, saying so.
- **A Nomad parse failure on a file that used to parse** does not, by
  itself, supersede that job's pending deployment: the job simply does not
  appear in this cycle's "seen" set, which only matters once nothing at all
  in the snapshot still produces its job ID (see "job removed from
  repository" below). A transient or persistent parse error is logged at
  ERROR every cycle until the file is fixed.

## Per-job reconciliation

For each managed job, in order:

1. If there is an active deployment in `pre_hook`, `applying` or `post_hook`,
   it is left untouched: an apply in progress is `engine-apply`'s concern.
2. If there is one in `detected` or `pending_approval`:
   - policy is now `none` → `superseded` ("policy changed to none");
   - else `spec_hash` differs → `superseded` ("newer spec at commit `<sha>`");
   - else the live index differs from `cas_index` → `superseded` ("job
     modified outside nops");
   - else there is no more drift → `completed` ("already in sync");
   - else it is left as is (still approvable).
3. If the policy is `none`, or there is no drift, nothing more happens.
4. If the retry rule applies (see above), nothing more happens.
5. Otherwise a deployment is created in `detected`, with the redacted diff,
   the full spec and the live index as `cas_index`. If a declared hook is
   missing from the repo it is immediately moved to `failed` (with a
   notification); otherwise `approval` moves it to `pending_approval` (with a
   notification) and `auto` is left in `detected` for `engine-apply`. The
   hooks the job declares are frozen with it.

Finally, every deployment still in `detected` or `pending_approval` whose job
is no longer seen anywhere in the snapshot is `superseded` ("job removed from
repository"): nops never deregisters a job on its own.

A store failure aborts the whole cycle (invariant 7: never act on Nomad with
unpersisted state); a `redact.Diff` failure aborts it too (never store an
unredacted diff). Every other failure — a bad file, a job Nomad cannot plan,
a revision the GC cannot stop — is scoped to the one file or job it came from:
logged, and the cycle continues with the rest.

### The commit is recorded on the deployment; the cycle status lives in memory

A deployment stores the subject and author of its commit
(`commit_subject`, `commit_author`) next to `commit_sha`, taken from the
`gitwatch.Snapshot` the cycle read: the clone is shallow and only ever holds
the head, so a page about a deployment from three commits ago cannot ask git
what that commit said. `Engine.Status()` reports how the last cycle went (when
it ended, how long it took, the error that aborted it, how many managed jobs it
found, how many it could not plan because of a Nomad failure, how many files
Nomad could not parse), in memory like `Observations()` and for the same reason:
it is recomputed every cycle. It exists because a cycle that logs an ERROR for
every job and produces no observation looks, from the dashboard, exactly like a
cluster with nothing to do.

## Tests

`internal/engine`: a fake Nomad and a real, temp-file `store.Store`, table-
and scenario-driven over the cases in [Decisions](#decisions) and
[Per-job reconciliation](#per-job-reconciliation) above, plus the parse cache
and the observation map never growing across cycles. `tests/integration`:
one cycle against a real `nomad agent -dev` — real parse and plan, the hook
frozen and nothing of it in Nomad, a redacted diff with the secret confirmed
absent, and revalidation once the live job changes outside nops; the hook
revision lifecycle end to end (`e2e_hookrev_test.go`: not in Nomad before
approval, registered and dispatched after it, deregistered without purge once
unused, a changed hook superseding a pending deployment) and the Nomad
behaviours the revisions rely on (`nomadx_test.go`).
