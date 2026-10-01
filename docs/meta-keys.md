# Meta keys

Nops reads its settings for a job from `nops_*` keys in the job's `meta`
block, in the HCL in git. It never writes them.

## All keys

| Meta key | Values | Default | Meaning |
|---|---|---|---|
| `nops_managed` | `"true"` / `"false"` | not managed | Opts the job in. Nops ignores jobs without it. |
| `nops_policy` | `auto` / `approval` / `none` | `none` | Who gives the go-ahead: see [policies](policies.md). |
| `nops_pre_hook` | Hook job IDs, comma-separated | — | [Hooks](hooks.md) to run, in order, before the apply. |
| `nops_post_hook` | Same | — | Hooks to run, in order, once the new version is healthy. |
| `nops_role` | `"hook"` | — | Marks a [hook job](hooks.md). |
| `nops_timeout` | Go duration, on a hook job | `5m` | How long the hook may run. |
| `nops_notify_completed` | `"true"` / `"false"` | `"false"` | Also [notify](logs-and-notifications.md#notifications) when a deployment completes. |
| `nops_sync_window` | Cron expression | — | When an `auto` job may deploy: the time its [sync window](policies.md#sync-window) opens. |
| `nops_sync_window_duration` | Go duration | — | How long the sync window stays open. Required with `nops_sync_window`. |

Values are case-sensitive: `"True"` isn't valid.

```hcl
job "api" {
  meta {
    nops_managed   = "true"
    nops_policy    = "approval"
    nops_pre_hook  = "api-backup, api-migrate"
    nops_post_hook = "api-smoke"
  }
}

job "web" {
  meta {
    nops_managed              = "true"
    nops_policy               = "auto"
    nops_sync_window          = "0 9 * * 1-5" # weekdays from 09:00
    nops_sync_window_duration = "9h"          # until 18:00
  }
}

job "api-backup" {
  meta {
    nops_role    = "hook"
    nops_timeout = "30m"
  }
}
```

## Validation

- An unknown `nops_` key logs a WARN and changes nothing.
- An invalid value logs an ERROR and sets the job's policy to `none` until you
  fix it. The dashboard shows the job as *Invalid meta*.
- A key that doesn't apply to the job, like `nops_timeout` on a job that isn't
  a hook, logs a WARN and is ignored.
- A deployment whose hook is missing from the repository fails.

## Syntax and parsing

**Variables.** If a `<name>.vars.hcl` file sits next to `<name>.nomad.hcl`,
Nops passes it as the job's variables, like `nomad job run -var-file`.
Otherwise every variable needs a default.

**Keys with dots.** HCL can't mix the block and object forms of `meta`. If a
job also has keys like `diun.enable`, write the whole block as
`meta = { "nops_managed" = "true" }`.
