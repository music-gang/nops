# Meta keys reference

The canonical list of the meta keys nops reads from a job's HCL. The source of
truth is [`internal/meta`](../internal/meta/meta.go): this page and that
package must be updated together.

nops uses **flat meta keys with the `nops_` prefix** in the job's `meta {}`
block. A structured block (`nops { ... }`) is not possible: verified on Nomad
2.0.3, `/v1/jobs/parse` answers `Unsupported block type`.

Keys are read **from the HCL in the repo** and nops never writes them.

## All keys

| Meta key | Values | Default | Meaning |
|---|---|---|---|
| `nops_managed` | `"true"` / `"false"` | not managed | Opt-in for the job. Without it, nops ignores the job. |
| `nops_policy` | `auto` / `approval` / `none` | `none` | See [policies](policies.md). |
| `nops_pre_hook` | ID of a hook job, or several separated by commas (`"backup,migrate"`) | — | Runs after approval, before apply, in the order listed, one after the other. If one fails, the ones after it do not run and the live job is left untouched. See [hooks](hooks.md). |
| `nops_post_hook` | Same | — | Runs once the new version is healthy, in the order listed. |
| `nops_role` | `"hook"` | — | Set on hook jobs: marks them as inert and as something nops registers itself, as a [revision](hooks.md#hook-revisions), when a deployment needs it. |
| `nops_timeout` | Go duration (`90s`, `10m`), on a hook job | `5m` | How long the hook may run; on expiry the dispatch is stopped and the deployment is `failed`. Must be > 0. Only with `nops_role = "hook"`. |

Values are **case-sensitive** strings (`"True"` is not valid).

```hcl
job "api" {
  meta {
    nops_managed   = "true"
    nops_policy    = "approval"
    nops_pre_hook  = "api-backup, api-migrate"
    nops_post_hook = "api-smoke"
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
  `none` for the whole job**: no deployment until the HCL is fixed.
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

**HCL2 variables.** Verified on Nomad 2.0.3: `/v1/jobs/parse` accepts the
contents of a var-file in the `Variables` field; with no value and no default
it answers `Unset variable`. Rule: if a `<name>.vars.hcl` exists next to the
job file `<name>.nomad.hcl` (or `<name>.nomad`), nops passes it as
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
