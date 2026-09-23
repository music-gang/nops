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
`meta { nops_role = "hook" }`. It is inert until dispatched, so nops registers
or updates it on its own (with plan + CAS) before dispatching, regardless of
the policy.

At dispatch nops passes the meta below, but **only the ones declared** in the
hook job's `meta_required`/`meta_optional` (Nomad rejects undeclared meta with
a 500):

| Dispatch meta | Content |
|---|---|
| `nops_deployment_id` | Deployment ID (declare it in `meta_required`) |
| `nops_job_id` | Target job |
| `nops_commit` | SHA of the commit that produced the deployment |
| `nops_phase` | `pre` / `post` |
| `nops_image_<task>` | Full docker image reference of the **new** version, for each task (e.g. `nops_image_api`) |

## Rules for hook authors

- **Outcome = exit code.** All allocations `complete` → success; any
  `failed`/`lost` → failure.
- **Fail fast.** `restart { attempts = 0  mode = "fail" }` and
  `reschedule { attempts = 0  unlimited = false }` (on separate lines: HCL
  does not allow more than one argument in a single-line block). Retries are
  decided by nops, not by Nomad.
- **Idempotency.** A hook may be dispatched again with the same
  `nops_deployment_id` after a nops crash. It must tolerate that: for example
  "migrate" must be a no-op if already applied, or use `nops_deployment_id` as
  a key.
- **Timeout.** It is handled by nops, not by the job. On expiry nops stops the
  dispatch and moves the hook to `timed_out`. If the hook has a timeout of its
  own (for example `image_pull_timeout`), keep it ≥ `nops_*_hook_timeout`.

## Placement

nops **does not resolve nodes**. If the hook must run on the target job's node
(image cache, local volumes), use the same `constraint`, or mount the same
host volume read-only: the scheduler places the hook where the volume is,
without hardcoding a node ID.

## Dispatch idempotency

nops dispatches with idempotency token `<deployment_id>:<phase>`. Verified on
Nomad 2.0.3: the same token returns the same child job without a new
evaluation, even after the child has finished. The token lives as long as the
child; for recovery see
[state machine](state-machine.md#recovery-after-a-crash).

## Examples

The files are in [`examples/`](../examples/). They are the standard cases nops
must cover; none of them is "the" reference case.

| Case | File | Notes |
|---|---|---|
| DB migration before deploy | [`migrate-hook.nomad.hcl`](../examples/migrate-hook.nomad.hcl) | Uses the same image as the new version; `migrate up` must be a no-op if already applied. |
| Post-deploy smoke test | [`smoke-hook.nomad.hcl`](../examples/smoke-hook.nomad.hcl) | HTTP check on the service that was just updated. |
| Pre-pull of a heavy image on a host volume | [`prepull-hostvolume.nomad.hcl`](../examples/prepull-hostvolume.nomad.hcl) | See below. |
| Backup before a stateful deploy | — | A `pre` hook that dumps or snapshots the volume (e.g. `pg_dump` to backup storage) with a generous timeout. It runs after approval, so the backup is fresh. |

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
