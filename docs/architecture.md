# Architecture

nops reads a git repo of HCL jobs, compares them with the Nomad cluster and
moves changes forward according to each job's [policy](policies.md), with
persistent state in SQLite, pre/post deployment [hooks](hooks.md) and an
approval [dashboard](dashboard.md).

## Components

| Package | Role |
|---|---|
| `cmd/nops` | Entrypoint: wiring of config, store, engine, web. |
| `internal/config` | Flags and env vars (`NOPS_*`), validation. |
| `internal/gitwatch` | In-memory clone, polling, webhook trigger. |
| `internal/nomadx` | Nomad client wrapper: parse, plan, CAS register, dispatch (and lookup of a dispatched job by token), allocations, stop. Register has no variant without the index check, and Nomad's plain HTTP 500 errors become sentinels (`ErrCASConflict`, `ErrJobNotFound`). Consumers such as `engine` define their own small interfaces over it. |
| `internal/meta` | Parsing and validation of the `nops_*` meta keys: the [source of truth](meta-keys.md) for the HCL syntax. |
| `internal/store` | SQLite, embedded migrations: see [state machine](state-machine.md). |
| `internal/engine` | State machine, reconciler, recovery on restart. |
| `internal/hooks` | Dispatch, wait, timeout and stop of hook jobs: `Runner.Run` is blocking, idempotent and resumable, and is driven by `engine`. |
| `internal/web` | Dashboard (`net/http` + `html/template`) and git webhook. |
| `internal/notify` | Notifications via a generic webhook (JSON POST). |
| `internal/redact` | Removes secret values from the plan diff before it is saved or shown ([rules](dashboard.md#secret-redaction)). A pure function, called by `engine` at detection. |

## Execution model: active, not lazy

nops moves deployments forward on its own, without waiting for anyone to open
the dashboard. There are three loops, all with configurable intervals:

1. **Detection.** Git polling, webhook, and a fixed interval for Nomad-side
   drift. Parse, plan, creation and superseding of deployments, revalidation
   of `pending_approval` ones.
2. **Engine.** Picks up non-terminal deployments and advances them: hook
   dispatch and waiting, apply, waiting for healthy, timeouts.
3. **Recovery** at startup: resumes whatever was left half-way (see
   [state machine](state-machine.md#recovery-after-a-crash)).

The only state that waits for an external event is `pending_approval`, by
design (invariant 3 in [philosophy.md](philosophy.md)).

## Apply and downtime

Apply is **just a CAS register**, the same effect as `nomad job run`: the
update (rolling, canary or destructive) is carried out by Nomad according to
the job's `update` stanza. nops issues no explicit stops: they would add
downtime and break the CAS sequence.

`applying` ends when the Nomad deployment is `successful`; if the job produces
none, when the allocations of the new version are `running`. The timeout is
configurable.

## What nops does not do

- It does not write to Git, and does not write meta into the live job.
- It does not roll back automatically and does not deregister jobs (for now). The only thing it stops is a hook job that timed out.
- It does not resolve nodes for hooks: placement is decided by the scheduler
  (see [hooks](hooks.md#placement)).
