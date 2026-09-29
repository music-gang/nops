# Meta keys reference

The canonical list of the meta keys Nops reads from a job's HCL. The source of
truth is [`internal/meta`](../internal/meta/meta.go): this page and that
package must be updated together (a test checks that the table below lists the
keys the package reads, no more and no fewer, and the defaults it can check).

Nops uses **flat meta keys with the `nops_` prefix** in the job's `meta {}`
block. A structured block (`nops { ... }`) is not possible: `/v1/jobs/parse` answers
`Unsupported block type` (`TestParseHCL`).

Keys are read **from the HCL in the repo** and Nops never writes them.

## All keys

| Meta key | Values | Default | Meaning |
|---|---|---|---|
| `nops_managed` | `"true"` / `"false"` | not managed | Opt-in for the job. Without it, Nops ignores the job. |
| `nops_policy` | `auto` / `approval` / `none` | `none` | See [policies](policies.md). |
| `nops_pre_hook` | ID of a hook job, or several separated by commas (`"backup,migrate"`) | — | Runs after approval, before apply, in the order listed, one after the other. If one fails, the ones after it do not run and the live job is left untouched. See [hooks](hooks.md). |
| `nops_post_hook` | Same | — | Runs once the new version is healthy, in the order listed. |
| `nops_role` | `"hook"` | — | Set on hook jobs: marks them as inert and as something Nops registers itself, as a [revision](hooks.md#hook-revisions), when a deployment needs it. |
| `nops_timeout` | Go duration (`90s`, `10m`), on a hook job | `5m` | How long the hook may run; on expiry the dispatch is stopped and the deployment is `failed`. Must be > 0. Only with `nops_role = "hook"`. |
| `nops_notify_completed` | `"true"` / `"false"` | `"false"` | Opt-in for a [notification](error-handling.md#notifications) when a deployment of this job becomes `completed`. Read from the spec a deployment froze at detection, so a later edit only affects the *next* deployment. Meaningless under policy `none` (no deployment ever reaches `completed`). |

Values are **case-sensitive** strings (`"True"` is not valid).

```hcl
job "api" {
  meta {
    nops_managed          = "true"
    nops_policy           = "approval"
    nops_pre_hook         = "api-backup, api-migrate"
    nops_post_hook        = "api-smoke"
    nops_notify_completed = "true"
  }
  # ...
}

job "api-backup" {
  meta {
    nops_role    = "hook"
    nops_timeout = "30m"
  }
  # ...
}
```

The timeout is a property of the hook, not of the job that uses it: whoever
writes the hook knows how long it takes, and every job that uses it gets the
same limit. The old `nops_pre_hook_timeout` and `nops_post_hook_timeout` are
gone; they are reported as unknown keys (WARN) that say what replaces them.

## Validation

- An **unknown key** under `nops_` (for example a typo like `nops_polcy`): a
  **WARN** log. It does not change behaviour.
- An **invalid value** for a recognised key: an **ERROR** log and **policy
  `none` for the whole job**: no deployment until the HCL is fixed. The
  Overview lists it as *Invalid meta*. The one exception is `nops_managed`
  itself: with a value that is neither `"true"` nor `"false"` (`"True"`, say)
  the job is not managed, so Nops **ignores it** like any job without the key:
  an ERROR in the log and nothing on the dashboard.
- `nops_policy` without `nops_managed = "true"`, or `nops_timeout` on a job
  that is not a hook: WARN, the key is ignored.
- A list of hooks with an **empty item** (`"backup,,migrate"`, a trailing
  comma) or the **same hook twice** in one phase: ERROR, policy `none`. Spaces
  around a name are ignored. The same hook may be in both phases.
- An invalid `nops_timeout` (not a duration, or not positive) is an ERROR on
  the hook job; a deployment that needs that hook fails at detection, saying
  its meta is invalid, until it is fixed.
- A hook that is declared but missing from the repo makes the deployment
  **fail**, rather than proceeding without the hook.

## Syntax and parsing

**HCL2 variables.** `/v1/jobs/parse` accepts the
contents of a var-file in the `Variables` field; with no value and no default
it answers `Unset variable`. Rule: if a `<name>.vars.hcl` exists next to the
job file `<name>.nomad.hcl` (or `<name>.nomad`), Nops passes it as
`Variables`; otherwise every variable must have a default. It is what
`nomad job run -var-file=...` would do: a generic job keeps its changing
values (image tag, count, domain) in that small file, which is in git and so
never holds a secret. Which files are read is described in
[gitwatch](design/gitwatch.md#which-files-are-read). A job that cannot be parsed creates no deployment and is logged at
ERROR. Hooks do not need variables: they receive everything through dispatch
meta.

**Meta keys containing dots.** HCL does not allow mixing the block form with
the object form: a job that also carries keys such as `diun.enable` must use
`meta = { "nops_managed" = "true" ... }` for the whole block.
