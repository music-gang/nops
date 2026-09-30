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
| `nops_sync_window` | A cron expression (5 fields, as Nomad's `periodic`: `"0 9 * * 1-5"`), with `nops_sync_window_duration` | — | When Nops may deploy this job **on its own**: the time the window **opens**. Outside the window the job is **held**: drift is still detected and shown, but no deployment is created; when it opens, detection plans again as usual. Only with policy `auto` (see [policies](policies.md#sync-windows)). Read in the time zone of [`-sync-window-time-zone`](configuration.md#storage-intervals-and-timeouts) (UTC by default). |
| `nops_sync_window_duration` | Go duration (`9h`, `90m`), with `nops_sync_window` | — | How long the window stays open from each time it opens, the end exclusive. May run past midnight (`"0 22 * * *"` with `8h` is 22:00 to 06:00). Must be > 0. Declared together with `nops_sync_window`: one without the other is an error. |

Values are **case-sensitive** strings (`"True"` is not valid).

**The sync window is read in UTC** unless the instance sets
[`-sync-window-time-zone`](configuration.md#storage-intervals-and-timeouts): a
window of `"0 9 * * *"` for `11h` is 11:00 to 22:00 in Rome in summer, not 09:00
to 20:00. The Job page says where the window stands and in which zone.

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

job "web" {
  meta {
    nops_managed              = "true"
    nops_policy               = "auto"
    nops_sync_window          = "0 9 * * 1-5" # Mondays to Fridays, from 09:00
    nops_sync_window_duration = "9h"          # until 18:00
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
  that is not a hook: WARN, the key is ignored. So is `nops_sync_window` on a
  job that is not managed, or whose policy is not `auto`: WARN.
- A `nops_sync_window` without `nops_sync_window_duration` (or the other way
  round), a cron expression that does not parse or never matches, or a
  duration that is not positive: ERROR, policy `none`.
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
it answers `Unset variable` (`TestParseHCL`). Rule: if a `<name>.vars.hcl` exists next to the
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
