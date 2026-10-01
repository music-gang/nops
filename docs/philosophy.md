# Philosophy and invariants

Why Nops behaves the way it does. The invariants below are **non-negotiable**,
and this page explains the reasoning behind each.

Nops is a semi-automatic GitOps controller for Nomad, aimed at a self-hosted
cluster for personal use. **No overengineering**: if a feature only exists "to
scale", we don't build it.

## Invariants

The tests that try to break each one are listed in
[development](development.md#invariant-tests).

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

4. **Git is the source of truth for Nops's behaviour.** Policy and hooks are
   read from the HCL in the repo, never from the live job. An invalid meta key
   leads to policy `none` + an ERROR log (conservative reading).
   *Why:* to change what Nops does to a job you change the HCL and commit:
   reviewable and versioned. A stale meta key on the live job is just drift
   that converges.

   *The one thing Nops reads from SQLite that decides whether it acts* is a
   person's **pause** of a job ([deployment lifecycle](deployment-lifecycle.md#holding-a-job)).
   It is not a second source of policy: it changes neither the policy nor the
   hooks git gives, and it can only make Nops do *less*. A paused job gets no
   new deployment and no approval, never a different one; a deployment already
   running finishes; resuming gives the job back to whatever git says. It
   exists because the brake has to work during an incident, when a commit that
   sets `nops_policy = "none"` is one more round trip through git.

5. **Nops never writes to Git** and **never writes meta into the live job**.
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
   *Two stops have no intent of their own*, because what they act on is already
   persisted: the stop of a hook run that timed out happens **before**
   `timed_out` is saved (the deadline is `started_at` + timeout, both in
   SQLite, and stopping is idempotent, so a crash in between just stops it
   again; [decision log](archive/decisions.md), 2026-09-23), and the garbage
   collection of hook revisions derives what to stop from the non-terminal
   deployments in the store.

## Fail loud

Nops prefers to stop and say so rather than carry on with uncertain state.

- **Never swallow an error** from Nomad or SQLite. The only "soft" exception is
  the notification: if it fails, it is logged at WARN and does not block the
  state machine.
- **If a SQLite write fails, the reconciler stops** for that cycle: Nops does
  not act on Nomad with state that has not been persisted (invariant 7).
- **When in doubt, take the conservative path:**
  - invalid meta key → policy `none` for the whole job;
  - hook declared but not found → `failed` (never "proceed without the hook");
  - CAS conflict → `failed` + re-detection.
- Messages in `error` and `events` must be useful to whoever looks at the
  dashboard: what failed, where, and what to do next.

What is logged and notified is in
[logs and notifications](logs-and-notifications.md).

## Patterns reused from nomad-gitops

The reference is [gerrowadat/nomad-gitops](https://github.com/gerrowadat/nomad-gitops)
(`docs/philosophy.md`, `docs/design/`). Nops is not a fork: it reuses only
these patterns.

| Pattern | Notes for Nops |
|---|---|
| Parse HCL via `/v1/jobs/parse` (`Jobs().ParseHCLOpts`, canonicalized, with the vars file as `Variables`) | No local HCL parser: Nomad is the only interpreter. |
| Diff via `Jobs.Plan(job, diff=true)` | The same `JobDiff` drives the decision and the dashboard. |
| CAS register with `JobModifyIndex` | See invariant 2. |
| Detection and apply decoupled; the newest commit supersedes the old one | The queue is persistent (SQLite). |
| Secret redaction in the diff (`Env[...]`, templates, *password/token/secret* keys) | Applied **before** the diff is saved to the DB or rendered in HTML. Nops also covers headers, URL credentials and more names (*auth*, *credential*, *privatekey*, *apikey*): see [dashboard](dashboard.md#secret-redaction). |
| In-memory git clone (go-git) + poll + webhook with a coalescing trigger (buffer-1 channel) | Read-only. |
| Count ignored for groups with a scaling policy | Don't fight the autoscaler: before the plan, such a group takes its `Count` from the live job ([architecture](architecture.md#rules-detection-keeps)). Nops does **not** use Nomad's `PreserveCounts` register option: it keeps the count of *every* group, so a count changed on purpose in git would be silently undone. |

**Deliberately dropped:** the stateless design (Nops needs state for approvals
and hooks), the `image-only` policy, flap guard and active rollback,
deregister of jobs (for now: the only thing Nops stops is a hook run that timed out and the hook revisions no deployment needs).
Prometheus metrics were on this list too, while `slog` seemed enough. They
are back, in [metrics](metrics.md), for what no notification says: Nops
alive but stuck (a git fetch or a store failing every cycle, an approval
forgotten), which the log alone leaves to someone reading it.
