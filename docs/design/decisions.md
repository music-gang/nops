# Decision log

Design decisions in chronological order. A row is added **in the same commit**
that takes or changes a decision. The extended reasoning lives in
[philosophy](../philosophy.md) and the other documents.

| Date | Decision |
|---|---|
| 2026-09-23 | Go, rewritten from scratch. Reused from nomad-gitops: parse via `/v1/jobs/parse`, plan-then-write, CAS register, supersede, diff redaction. |
| 2026-09-23 | State in SQLite (`modernc.org/sqlite`); per-job lock through a partial unique index. |
| 2026-09-23 | Policy and hooks as flat `nops_*` meta keys (custom blocks do not pass `/v1/jobs/parse`). |
| 2026-09-23 | Hook = a `parameterized` job in the repo, synced by nops; idempotent dispatch with token `<deployment_id>:<phase>`. |
| 2026-09-23 | **pre_hook runs AFTER approval** (order changed from the initial spec): with migrations and backups nothing must run before the human OK. For pre-pulling the order does not change downtime. |
| 2026-09-23 | Apply = just a CAS register; the update is carried out by Nomad, no explicit stops. |
| 2026-09-23 | Active engine (not lazy). `pending_approval` deployments are revalidated on every cycle: index changed → `superseded` (+ a new deployment if there is drift); empty plan → `completed` no-op. No time-based expiry. |
| 2026-09-23 | Dashboard auth via the reverse proxy header (`Remote-User`); notifications via a generic webhook. |
| 2026-09-23 | Verified on `nomad agent -dev` 2.0.3: (1) `/v1/jobs/parse` rejects a `nops {}` block ("Unsupported block type"); (2) `Variables` works, with no value → "Unset variable"; (3) dispatch with undeclared meta → 500 "unpermitted metadata keys", with a missing required meta → 500; (4) `-idempotency-token` (API `IdempotencyToken`) returns the same child job without a new evaluation, **even after the child has completed** (`dead`) → redispatching after a crash does not rerun the hook. |
| 2026-09-23 | Invalid meta (any ERROR) → policy `none` for the whole job, not just for the wrong key. |
| 2026-09-23 | Store: a single SQLite connection (serialized writes); `Transition` is the only way to change `state`, validates the transition table; `From` is required (compare-and-set on the state). |
| 2026-09-23 | Go module: `github.com/music-gang/nops`. |
| 2026-09-23 | CLAUDE.md holds only the working rules; all system documentation lives in `docs/`. |
| 2026-09-23 | Everything is written in English: code, comments, docs, examples, README, CLAUDE.md and commit messages. |
| 2026-09-23 | Commit messages follow the Angular convention (`type(scope): subject`). |
| open | The idempotency token lives as long as the child job: if Nomad garbage-collects the child, a new dispatch is possible. Recovery must prefer the `dispatched_job_id` saved in `hook_runs`. |
