# Pre/post deployment hooks

A hook is a Nomad job that Nops **dispatches** around a deployment. The
`pre_hook` runs after approval and before apply; the `post_hook` runs once the
new version is healthy. If a hook fails or times out, the deployment goes to
`failed` with a notification; if it is a pre-hook, the live job is left
untouched.

Hooks are declared on the job being deployed with `nops_pre_hook` and
`nops_post_hook`, each one hook or a comma-separated list (see
[meta-keys](meta-keys.md)); how long a hook may run is `nops_timeout` on the
hook job itself.

## Several hooks

A phase with several hooks (`nops_pre_hook = "backup,migrate"`) runs them **in
the order listed, one after the other**, never in parallel: a backup must have
finished before the migration starts. The next hook is dispatched only once the
previous one has succeeded. **The first one to fail or time out stops the
phase**:

- a failing pre-hook fails the deployment with the live job untouched, and the
  hooks after it are neither registered nor dispatched;
- a failing post-hook fails the deployment with the apply already live, and the
  ones after it do not run.

The message names the hook that failed. Each hook is its own run (a row per
phase and position in `hook_runs`), with its own [revision](#hook-revisions),
its own timeout and its own idempotency token, so after a Nops crash the
deployment resumes at the first hook that had not finished: the ones that
succeeded are not run again. The same hook cannot be listed twice in one phase.
A hook that is in the repo but has invalid meta counts as unusable, like one
that is missing: the deployment fails at detection.

## Contract

A hook is a **`batch` + `parameterized`** job in the repo, with
`meta { nops_role = "hook" }`. It is inert until dispatched, and Nops registers
it in Nomad itself, as a [revision](#hook-revisions), right before dispatching
it, regardless of the policy. Before dispatching, Nops checks the job in Nomad:
if it is missing, is not marked `nops_role = "hook"`, is not `batch`, or is not
parameterized, the hook run is `failed` and nothing is dispatched.

You do not register hooks yourself. If you have (a hook under its plain ID),
Nops leaves it alone and never runs it.

At dispatch Nops passes the meta below, but **only the ones declared** in the
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
the same image; with different images the hook run is `failed`, since Nops
cannot tell which one the hook wants.

A hook that lists in `meta_required` something Nops cannot provide (for example
`nops_image_web` when the job has no docker task `web`) is `failed` with a
message naming the key.

## Namespace

A hook lives in the namespace of the job that declares it: `nops_pre_hook =
"backup"` on a job of namespace `apps` means the hook `backup` whose HCL says
`namespace = "apps"`. Its revision is registered and dispatched there, so the
ACL of that namespace applies to it. A hook declared only in another namespace
counts as missing (`pre-hook "backup" not found in repo`). Two hooks with the
same ID in two namespaces are two hooks, each used by the jobs of its own.

## Rules for hook authors

- **Outcome = exit code.** Nops decides from the dispatched job and its
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
  decided by Nops, not by Nomad.
- **Idempotency.** A hook may be dispatched again with the same
  `nops_deployment_id` after a Nops crash. It must tolerate that: for example
  "migrate" must be a no-op if already applied, or use `nops_deployment_id` as
  a key. Two deployments can run the same hook at the same time (each one its
  own revision, or the same revision): it must tolerate that too.
- **Timeout.** Set with `nops_timeout` in the hook job's meta (default `5m`).
  It is enforced by Nops, not by the job's own settings, and frozen with the
  hook revision, so it is approved with the rest. It counts from the moment
  the run was first recorded (`started_at`), so a Nops restart does not give
  the hook more time. The store keeps whole seconds, so a sub-second part of
  the timeout is rounded up (`1500ms` → `2s`), never down. On expiry Nops stops the dispatched job (without purging
  it, so it stays visible in Nomad) and moves the hook to `timed_out`. If the
  hook has a timeout of its own (for example `image_pull_timeout`), keep it ≥
  `nops_timeout`. A hook that finishes in the same poll in which the
  timeout expires counts as finished.

## Placement

Nops **does not resolve nodes**. If the hook must run on the target job's node
(image cache, local volumes), use the same `constraint`, or mount the same
host volume read-only: the scheduler places the hook where the volume is,
without hardcoding a node ID.

## Hook revisions

A deployment runs the hooks **as they were when it was detected**, not as they
are in git when it gets to run them: approving a deployment covers the target
and its hooks. At detection Nops freezes, for each hook the target declares,
the hook's spec (as parsed from the same commit as the target) and its hash, and
the deployment's `spec_hash` is the hash of the target and of the hooks, in
order. So:

- a hook changed in git supersedes a deployment still waiting for approval,
  which is detected again and must be approved again;
- a job blocked because its hook was missing or broken is unblocked by
  fixing the hook, without a retry (its `spec_hash` changed);
- a change to a hook alone, with the target already in sync, creates no
  deployment.

A revision is registered as its own job, named
**`<hook-id>-<first 8 hex of the hook's spec hash>`**, and Nomad's job list
shows the revisions next to your own jobs. Once no deployment in progress needs
it, Nops stops it (without purging, so its runs and their logs stay visible).
The **only** consequence for you: **a hook you register by hand under an ID that
ends in `-` and 8 hex digits, with `nops_role = "hook"`, is stopped by Nops.**
How revisions are registered, collected and dispatched is in
[architecture](architecture.md#hook-revisions).

## Examples

The files are in [`examples/`](../examples/), one runnable directory per
scenario (the target job and its hook together, with a `.vars.hcl` for what
you would tweak for your own cluster). They are the standard cases Nops must
cover; none of them is "the" reference case. `TestExamplesParse` (integration)
keeps them parsing, with valid meta, with the hooks they declare and needing
nothing but Nomad (see below).

| Case | Files | Notes |
|---|---|---|
| DB migration before deploy | [`examples/migrate/`](../examples/migrate/) | Uses the same image as the new version; `migrate up` must be a no-op if already applied. |
| Post-deploy smoke test | [`examples/approval/`](../examples/approval/) | HTTP check on the service that was just updated. |
| Pre-pull of a heavy image on a host volume | [`examples/prepull-hostvolume/`](../examples/prepull-hostvolume/) | See below. |
| Backup before a stateful deploy | [`examples/backup-stateful/`](../examples/backup-stateful/) | A `pre` hook that dumps the volume (`pg_dump` to a separate backup volume) with a generous timeout. It runs after approval, so the backup is fresh. |
| Backup, then migrate | [`examples/backup-then-migrate/`](../examples/backup-then-migrate/) | Two `pre` hooks in a row: the dump (30m) and then the migration (5m), each with its own `nops_timeout`. If the dump fails, the migration never runs. |

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
