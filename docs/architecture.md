# Architecture

How Nops works inside. The states of a deployment are in
[deployment lifecycle](deployment-lifecycle.md); the rules every part keeps
are in [philosophy](philosophy.md).

## Components

| Package | Role |
|---|---|
| `cmd/nops` | Wires everything together and runs the loops and the dashboard. |
| `internal/config` | Flags and environment variables. |
| `internal/gitwatch` | Keeps the repository in memory and polls it. |
| `internal/nomadx` | Nomad client: parse, plan, CAS register, dispatch. |
| `internal/meta` | Reads and validates the `nops_*` keys. |
| `internal/store` | SQLite, the only place state lives. |
| `internal/engine` | Detection and apply: the state machine. |
| `internal/hooks` | Runs a hook job and waits for its outcome. |
| `internal/web` | The dashboard, the login, and the git webhook. |
| `internal/metrics` | `/metrics`. Each scrape reads the store and the engine. |
| `internal/notify` | Notification adapters. |
| `internal/redact` | Removes secrets from the plan diff. |
| `internal/version` | The build's version. |

## Loops

Nops moves deployments forward on its own:

1. **Detection** runs at startup, on every new commit, every
   `-drift-interval`, and after anything that can change a job's state (a
   deployment ending, a reject, a retry, a pause).
2. **Apply** runs every `-engine-interval` and advances active deployments.
3. **Recovery** is the first apply cycle after a start: it resumes whatever a
   restart interrupted ([deployment lifecycle](deployment-lifecycle.md#recovery-after-a-crash)).

## Reading the repository

`gitwatch` keeps a shallow, in-memory clone of `-git-branch` and fetches it
again when the head moves. Nops reads every `*.nomad.hcl` or `*.nomad` file
under `-git-path`, with its optional `<name>.vars.hcl`. Nomad parses each
file. Its meta decides what it is: `nops_role = "hook"` makes a hook,
`nops_managed = "true"` a managed job; Nops ignores anything else.

If a fetch fails, Nops keeps working on the last good snapshot and shows the
error on the Overview.

## Detection

Each cycle, for every managed job, Nops:

1. plans the job against the cluster;
2. freezes the hooks it declares ([hook revisions](#hook-revisions));
3. revalidates its waiting deployment: Nops supersedes it when git or the
   live job changed ([revalidation](deployment-lifecycle.md#revalidating-pending-deployments));
4. creates a deployment if there is drift and the policy, a block, or a hold
   don't stop it.

A job's drift, hooks and state live in memory and are rebuilt every cycle;
only deployments go to SQLite. A deployment stores the full spec to register,
so apply registers exactly what was planned and approved. The dashboard never
shows that spec, only the redacted diff.

### Rules detection keeps

- A job is identified by namespace and ID. Nops ignores jobs outside the
  managed namespaces.
- A new commit supersedes a waiting deployment only if it changes the job's
  spec or hooks.
- A task group with a `scaling` block keeps its live count, so Nops doesn't
  fight the autoscaler.
- A missing or invalid hook fails the deployment at detection.
- While any file doesn't parse, Nops doesn't treat missing jobs as removed:
  an unparsed file looks the same as a deleted one.

### Orphan jobs

Nops never stops a job it deployed. If you remove a deployed job from git and
it still runs in Nomad, the dashboard lists it as an orphan until you stop it
or put the file back.

## Apply

Each step saves its state before it calls Nomad, so recovery is the same step
run again. Only `Approve` and `Reject` move a deployment out of
`pending_approval`.

1. **Pre-hooks** run in order ([hooks](hooks.md#several-hooks)).
2. **Register:** Nops plans again, and registers only if the plan still shows
   a difference and the job's modify index is still the one read at
   detection. If someone changed the job, the deployment fails.
3. **Wait for health**, within the [apply timeout](#the-apply-timeout).
4. **Post-hooks** run in order, then the deployment completes.

Nomad carries out the update according to the job's `update` block, so
rolling updates, canaries and `auto_revert` work as usual. A deployment that
doesn't become healthy fails; Nops doesn't roll back.

### Healthy

- With a Nomad deployment for this register: healthy when it's `successful`.
- Without one, as for batch jobs: healthy when every allocation of the new
  version is `running` or `complete`.
- A job with no allocations, like a periodic or parameterized job: healthy
  once registered.

### The apply timeout

`-apply-timeout` counts from the start of the apply and covers the register
and the wait for health. It pauses while canaries wait for someone to promote
them, and restarts in full after the promotion. Promote from the deployment's
page or in Nomad.

## Hook revisions

A deployment runs its hooks exactly as they were when it was created, so an
approval covers them. At detection, Nops hashes each hook's spec and includes
it in the deployment's spec hash. Before running a hook, Nops registers it as
its own job, `<hook-id>-<first 8 hex digits of the hash>`. After each cycle,
it stops the revisions no active deployment needs.

Each hook run is dispatched with an idempotency token, so a dispatch repeated
after a crash returns the same run instead of starting a second one.

## What Nops does not do

- Write to git, or write meta into a live job.
- Roll back, or stop the jobs it deploys.
- Pick nodes for hooks: the scheduler does ([hooks](hooks.md#placement)).
