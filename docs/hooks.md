# Pre/post deployment hooks

A hook is a Nomad job that nops **dispatches** around a deployment. The
`pre_hook` runs after approval and before apply; the `post_hook` runs once the
new version is healthy. If a hook fails or times out, the deployment goes to
`failed` with a notification; if it is a pre-hook, the live job is left
untouched.

Hooks are declared on the job being deployed with `nops_pre_hook`,
`nops_post_hook` and the related `_timeout` keys (see
[meta-keys](meta-keys.md)).

## Contract

A hook is a **`batch` + `parameterized`** job in the repo, with
`meta { nops_role = "hook" }`. It is inert until dispatched, and nops registers
it in Nomad itself, as a [revision](#hook-revisions), right before dispatching
it, regardless of the policy. Before dispatching, nops checks the job in Nomad:
if it is missing, is not marked `nops_role = "hook"`, is not `batch`, or is not
parameterized, the hook run is `failed` and nothing is dispatched.

You do not register hooks yourself. If you have (a hook under its plain ID),
nops leaves it alone and never runs it.

## Hook revisions

A deployment runs the hooks **as they were when it was detected**, not as they
are in git when it gets to run them: approving a deployment covers the target
and its hooks. At detection nops freezes, for each hook the target declares,
the hook's spec (as parsed from the same commit as the target) and its hash, and
the deployment's `spec_hash` is the hash of the target and of the hooks, in
order. So:

- a hook changed in git supersedes a deployment still waiting for approval,
  which is detected again and must be approved again;
- a job blocked because its hook was missing or broken is unblocked by
  fixing the hook, without a retry (its `spec_hash` changed);
- a change to a hook alone, with the target already in sync, creates no
  deployment.

Nothing of a hook is in Nomad until its deployment is approved and gets to the
hook step. There nops registers the frozen spec as a job named
**`<hook-id>-<first 8 hex of the hook's spec hash>`** (its `Name` stays the
hook's own), with a plan first and a CAS on the live index (`0` if it is not
there), and skips the register when the plan shows no change. That job is the
**revision**, and it is what is dispatched: the run is `<revision>/dispatch-…`
and `hook_runs.hook_job_id` holds the revision. Two deployments that share a
hook at the same spec share the revision; at different specs they use different
jobs, so neither can change what the other dispatches.

After every detection cycle nops **deregisters, without purging**, the
revisions no deployment that is still in progress needs. It only considers jobs
that are hooks (`nops_role = "hook"`), whose ID ends in `-` and 8 lowercase hex
digits, and that are not dispatched runs. A stopped revision stays visible in
Nomad, with the runs it dispatched and their logs (Nomad's own garbage
collection removes it later), and is registered again if a later deployment
needs it. The **only** consequence for you: **a hook you register by hand under
an ID that ends in `-` and 8 hex digits, with `nops_role = "hook"`, is stopped
by nops.** Nomad's job list shows the revisions next to your own jobs; they are
the price of registering a hook only when it is approved.

If the revision cannot be registered, nops logs an ERROR and tries again at the
next cycle; if it still cannot within the hook's timeout, the deployment
fails, as a hook that cannot be reached would.

At dispatch nops passes the meta below, but **only the ones declared** in the
hook job's `meta_required`/`meta_optional` (Nomad rejects undeclared meta with
a 500):

| Dispatch meta | Content |
|---|---|
| `nops_deployment_id` | Deployment ID (declare it in `meta_required`) |
| `nops_job_id` | Target job |
| `nops_commit` | SHA of the commit that produced the deployment |
| `nops_phase` | `pre` / `post` |
| `nops_image_<task>` | Full docker image reference of the **new** version, for each `docker` task (e.g. `nops_image_api`) |

The task name in `nops_image_<task>` is sanitized: every character that is not
a letter or a digit becomes `_` (task `side-car` → `nops_image_side_car`), so it
is a valid meta key and a valid `${NOMAD_META_...}` name. If two tasks (for
example in different groups) map to the same key, that is fine when they use
the same image; with different images the hook run is `failed`, since nops
cannot tell which one the hook wants.

A hook that lists in `meta_required` something nops cannot provide (for example
`nops_image_web` when the job has no docker task `web`) is `failed` with a
message naming the key.

## Rules for hook authors

- **Outcome = exit code.** nops decides from the dispatched job and its
  allocations:
  - any allocation `failed` or `lost` → `failed`, with the task's exit code or
    driver error in the message (the hook's own output stays in Nomad's logs);
  - the job is `dead` and every allocation is `complete` → `succeeded`;
  - an allocation that completed but that the server wanted stopped (someone
    stopped the job) → `failed`;
  - the job is `dead` with no allocation at all → `failed`;
  - anything else, including "no allocation yet" (for example while the
    scheduler looks for a node) → keep waiting, until the timeout.
- **Fail fast.** `restart { attempts = 0  mode = "fail" }` and
  `reschedule { attempts = 0  unlimited = false }` (on separate lines: HCL
  does not allow more than one argument in a single-line block). Retries are
  decided by nops, not by Nomad.
- **Idempotency.** A hook may be dispatched again with the same
  `nops_deployment_id` after a nops crash. It must tolerate that: for example
  "migrate" must be a no-op if already applied, or use `nops_deployment_id` as
  a key. Two deployments can run the same hook at the same time (each one its
  own revision, or the same revision): it must tolerate that too.
- **Timeout.** It is handled by nops, not by the job. It counts from the moment
  the run was first recorded (`started_at`), so a nops restart does not give
  the hook more time. The store keeps whole seconds, so a sub-second part of
  the timeout is rounded up (`1500ms` → `2s`), never down. On expiry nops stops the dispatched job (without purging
  it, so it stays visible in Nomad) and moves the hook to `timed_out`. If the
  hook has a timeout of its own (for example `image_pull_timeout`), keep it ≥
  `nops_*_hook_timeout`. A hook that finishes in the same poll in which the
  timeout expires counts as finished.

## Placement

nops **does not resolve nodes**. If the hook must run on the target job's node
(image cache, local volumes), use the same `constraint`, or mount the same
host volume read-only: the scheduler places the hook where the volume is,
without hardcoding a node ID.

## Dispatch idempotency

nops dispatches with idempotency token `<deployment_id>:<phase>`. Verified on
Nomad 2.0.3: the same token returns the same dispatched job without a new
evaluation, even after the dispatched job has finished. The token lives as long as the
dispatched job; for recovery see
[state machine](state-machine.md#recovery-after-a-crash).

nops saves the run as `running` **before** it sends the dispatch, and the dispatched job
ID right after. A run found `running` without a dispatched job ID may or may not have
reached Nomad, so nops looks the dispatched job up by its idempotency token (Nomad
records it on the dispatched job) and carries on with it, timeout included. If there
is none, the run is `failed` ("outcome unknown") instead of being dispatched
again: the dispatched job may have run and been garbage-collected, the token no longer
deduplicates at that point, and the hook would run a second time. The same
happens when the dispatched job of a saved ID disappears before nops saw its outcome.

## Examples

The files are in [`examples/`](../examples/), one runnable directory per
scenario (the target job and its hook together, with a `.vars.hcl` for what
you would tweak for your own cluster). They are the standard cases nops must
cover; none of them is "the" reference case. `TestExamplesParse` (integration)
keeps them parsing, with valid meta, with the hooks they declare and needing
nothing but Nomad (see below).

| Case | Files | Notes |
|---|---|---|
| DB migration before deploy | [`examples/migrate/`](../examples/migrate/) | Uses the same image as the new version; `migrate up` must be a no-op if already applied. |
| Post-deploy smoke test | [`examples/approval/`](../examples/approval/) | HTTP check on the service that was just updated. |
| Pre-pull of a heavy image on a host volume | [`examples/prepull-hostvolume/`](../examples/prepull-hostvolume/) | See below. |
| Backup before a stateful deploy | [`examples/backup-stateful/`](../examples/backup-stateful/) | A `pre` hook that dumps the volume (`pg_dump` to a separate backup volume) with a generous timeout. It runs after approval, so the backup is fresh. |

### Pre-pull on a host volume

A service with a host volume has a single write mount, so no canary or
blue-green: the update is stop+start, and with a heavy image stop+pull+start
causes a long downtime. The hook mounts the same host volume read-only (so it
lands on the same node) and the docker driver pulls the new image, with
`entrypoint = ["true"]` so the app does not run. Apply then finds the image
already in the node's cache and downtime shrinks to stop+start. Requirement on
the target job: `force_pull = false`.

A real-world case is an n8n warm-up: `entrypoint` (not `command`) fully
replaces the image's ENTRYPOINT, which would otherwise do real setup and get
OOM-killed with little memory.

### Reaching the service from a hook

The examples need nothing but Nomad, not even Consul: the deployed job
registers its service with Nomad's own discovery (`provider = "nomad"` in the
`service` block; a service without it defaults to Consul, and the job is not
even placed on a cluster that has none), and the hook reads the address in a
`template`:

```hcl
template {
  destination = "local/url"
  data        = <<-EOT
    {{ with nomadService "web" }}{{ with index . 0 }}http://{{ .Address }}:{{ .Port }}/{{ end }}{{ end }}
  EOT
}
```

With `env = true` the same idea gives environment variables (the backup
example renders `PGHOST` and `PGPORT` for `pg_dump`). A hook runs after the
new version is healthy, or after approval for a pre-hook, so the service of an
already running job is registered by then.
