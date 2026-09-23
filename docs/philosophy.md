# Philosophy and invariants

Why nops behaves the way it does. The invariants below are **non-negotiable**:
CLAUDE.md carries the short list, this page explains the reasoning behind each.

nops is a semi-automatic GitOps controller for Nomad, aimed at a self-hosted
cluster for personal use. **No overengineering**: if a feature only exists "to
scale", we don't build it.

## Invariants

1. **Plan before every write.** No `Register` without a `Jobs.Plan()` just run
   that confirms a difference. If the plan no longer shows any difference, the
   deployment closes as a no-op.
   *Why:* it avoids pointless registers (a new job version, a new evaluation)
   and gives us the same `JobDiff` to show in the dashboard.

2. **CAS on every register.** `RegisterOpts{EnforceIndex: true, ModifyIndex:
   <JobModifyIndex captured at detection>}`; 0 means "the job must not exist".
   A CAS conflict leads to `failed` and a re-detection; with policy `approval`
   a new approval is required.
   *Why:* a lot of time can pass between detection and apply (especially while
   waiting for approval). Without CAS we would apply a stale spec on top of
   changes made by hand in the meantime. It also makes repeating a register
   after a crash safe: if the first one already went through, the index has
   changed and Nomad rejects the second.

3. **Never auto-apply under policy `approval`.** The only transition from
   `pending_approval` towards apply is an authenticated human action, recorded
   in `events` with the actor. An approval is valid for
   `(deployment_id, spec_hash)` and never transfers to another spec.
   *Why:* approval is the whole point of the policy. If the spec changes after
   the OK, the OK no longer holds.

4. **Git is the source of truth for nops's behaviour.** Policy and hooks are
   read from the HCL in the repo, never from the live job. An invalid meta key
   leads to policy `none` + an ERROR log (conservative reading).
   *Why:* to change what nops does to a job you change the HCL and commit:
   reviewable and versioned. A stale meta key on the live job is just drift
   that converges.

5. **nops never writes to Git** and **never writes meta into the live job**.
   Tool state lives only in SQLite.
   *Why:* writing meta into the live job causes "meta-drift": the next
   `nomad job run` from HCL silently wipes it.

6. **One active deployment per job**, enforced by the DB (partial unique
   index), not by an in-memory mutex.
   *Why:* a mutex dies with the process; the index survives a crash and would
   still hold if two instances ever ran.

7. **State is persisted before acting.** Every side effect on Nomad (dispatch,
   register, stop) happens *after* the intent (state + idempotency token) has
   been saved to SQLite. If the DB write fails, we do not proceed.
   *Why:* after a crash we must know what we were about to do, otherwise
   recovery cannot be idempotent.

## Patterns reused from nomad-gitops

The reference is [gerrowadat/nomad-gitops](https://github.com/gerrowadat/nomad-gitops)
(`docs/philosophy.md`, `docs/design/`). nops is not a fork: it reuses only
these patterns.

| Pattern | Notes for nops |
|---|---|
| Parse HCL via `/v1/jobs/parse` (`Jobs().ParseHCL(src, true)`) | No local HCL parser: Nomad is the only interpreter. |
| Diff via `Jobs.Plan(job, diff=true)` | The same `JobDiff` drives the decision and the dashboard. |
| CAS register with `JobModifyIndex` | See invariant 2. |
| Detection and apply decoupled; the newest commit supersedes the old one | The queue is persistent (SQLite). |
| Secret redaction in the diff (`Env[...]`, templates, *password/token/secret* keys) | Applied **before** the diff is saved to the DB or rendered in HTML. |
| In-memory git clone (go-git) + poll + webhook with a coalescing trigger (buffer-1 channel) | Read-only. |
| `PreserveCounts`, and Count ignored for groups with a scaling policy | Don't fight the autoscaler. |

**Deliberately dropped:** the stateless design (nops needs state for approvals
and hooks), the `image-only` policy, flap guard and active rollback,
deregister (for now), Prometheus metrics (for now `slog` is enough).
