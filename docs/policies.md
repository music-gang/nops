# Policies

A job's policy says what nops does when it finds a difference between the repo
and the cluster. It is declared with `nops_policy` (see
[meta-keys](meta-keys.md)).

| Value | Behaviour |
|---|---|
| `auto` | nops applies on its own: plan → (pre-hook) → CAS register → (post-hook). |
| `approval` | A `pending_approval` deployment is created with the diff; a human OK from the [dashboard](dashboard.md) is required. **Never** auto-applied. |
| `none` | Drift is shown but never applied, and no deployment is created. This is the default. |

An invalid meta key leads to `none` for the whole job (conservative reading).

## Which policy for which job

| Kind of job | Typical policy |
|---|---|
| Stateless service (frontend, API, proxy) | `auto` |
| Stateful service / with a volume / database | `approval` |
| Delicate or hand-managed jobs | `none` (observation only) |
| Batch / periodic | `auto`, or `approval` if the change touches data |

## Effect on apply

Apply is always the same, whatever the policy: a CAS register preceded by a
plan (see [architecture](architecture.md#apply-and-downtime)). The policy only
decides *who* gives the go-ahead, not *how* it is applied.

## Approval

- Approving or rejecting is an authenticated human action, recorded in
  `events` with the actor and in `decided_by`/`decided_at`.
- An approval is valid for `(deployment_id, spec_hash)`. If the spec changes,
  the old pending deployment becomes `superseded` and a new one is created to
  approve.
- If the live job changes outside nops, the pending deployment is
  revalidated automatically (see
  [state machine](state-machine.md#revalidating-pending-deployments)).
