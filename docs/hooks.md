# Hooks

A hook is a Nomad job that Nops runs around a deployment: a pre-hook before
the apply, for a backup or a migration, and a post-hook once the new version
is healthy, for a smoke test. If a hook fails or times out, the deployment
fails. A failed pre-hook leaves the live job untouched.

Declare hooks on the job you deploy, with `nops_pre_hook` and
`nops_post_hook` ([meta keys](meta-keys.md)).

## Several hooks

`nops_pre_hook = "backup,migrate"` runs the hooks one after the other, in
order. The first failure stops the phase: the hooks after it don't run. After
a Nops restart, the deployment resumes at the first hook that hadn't finished.

## Contract

A hook is a `batch`, `parameterized` job in git with
`meta { nops_role = "hook" }`. Don't register it yourself: Nops registers it
as a [revision](#hook-revisions) when a deployment needs it.

Nops passes these values at dispatch, but only the ones the hook declares in
`meta_required` or `meta_optional`:

| Dispatch meta | Content |
|---|---|
| `nops_deployment_id` | Deployment ID. |
| `nops_job_id` | Target job. |
| `nops_commit` | Commit SHA of the deployment. |
| `nops_phase` | `pre` or `post`. |
| `nops_image_<task>` | Image of the new version, for each `docker` task. Characters other than letters and digits in the task name become `_`. |

## Namespace

A hook runs in the namespace of the job that declares it. A hook defined only
in another namespace counts as missing.

## Rules for hook authors

- **The exit code is the outcome.** A failed or lost allocation fails the
  hook. All allocations complete means success.
- **Fail fast.** Turn off Nomad's retries, Nops decides them:

  ```hcl
  restart {
    attempts = 0
    mode     = "fail"
  }
  reschedule {
    attempts  = 0
    unlimited = false
  }
  ```

- **Be idempotent.** After a crash, Nops can dispatch a hook again for the
  same deployment, and two deployments can run the same hook at once.
- **Set a timeout** with `nops_timeout` on the hook job (default `5m`). When
  it expires, Nops stops the hook and the deployment fails.

## Placement

Nops doesn't pick nodes. To run a hook on the target job's node, give it the
same `constraint`, or mount the same host volume read-only.

## Hook revisions

A deployment runs its hooks as they were when Nops created it: approving a
deployment approves its hooks too. Changing a hook in git replaces a
deployment that waits for approval.

Nops registers each hook version as its own job,
`<hook-id>-<first 8 hex digits of its spec hash>`, and stops it once no
deployment needs it. So Nops stops any hook job you register by hand with a
name ending in `-` and 8 hex digits.

## Examples

Each directory in [`examples/`](../examples/) holds a job and its hooks:

| Case | Files |
|---|---|
| Database migration before deploying | [`examples/migrate/`](../examples/migrate/) |
| Smoke test after deploying | [`examples/approval/`](../examples/approval/) |
| Pull a heavy image before deploying | [`examples/prepull-hostvolume/`](../examples/prepull-hostvolume/) |
| Backup before a stateful deploy | [`examples/backup-stateful/`](../examples/backup-stateful/) |
| Backup, then migrate | [`examples/backup-then-migrate/`](../examples/backup-then-migrate/) |

### Pre-pull on a host volume

A job with a host volume restarts in place, so a heavy image means a long
downtime while it pulls. A pre-hook that mounts the same volume read-only runs
on the same node and pulls the new image with `entrypoint = ["true"]`. The
apply then finds the image in the cache. The target job needs
`force_pull = false`.

### Reaching the service from a hook

Register the service with `provider = "nomad"` and read its address in a
`template`, with no Consul needed:

```hcl
template {
  destination = "local/url"
  data        = <<-EOT
    {{ with nomadService "web" }}{{ with index . 0 }}http://{{ .Address }}:{{ .Port }}/{{ end }}{{ end }}
  EOT
}
```
