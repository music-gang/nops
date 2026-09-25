-- A phase can run several hooks, one after the other: a hook run is now
-- identified by (deployment, phase, position), the same position the frozen
-- hook has in deployment_hooks. SQLite cannot change a UNIQUE constraint, so
-- the table is rebuilt. Rows keep their idempotency token as it was
-- (`<deployment_id>:<phase>`): a run in flight is found again by it, and new
-- ones use `<deployment_id>:<phase>:<position>`.
CREATE TABLE hook_runs_new (
    id                TEXT PRIMARY KEY,
    deployment_id     TEXT    NOT NULL REFERENCES deployments (id),
    phase             TEXT    NOT NULL CHECK (phase IN ('pre', 'post')),
    position          INTEGER NOT NULL DEFAULT 0,
    hook_job_id       TEXT    NOT NULL,
    idempotency_token TEXT    NOT NULL,
    dispatched_job_id TEXT,
    state             TEXT    NOT NULL CHECK (state IN (
        'dispatching', 'running', 'succeeded', 'failed', 'timed_out')),
    timeout_s         INTEGER NOT NULL,
    error             TEXT,
    started_at        TEXT    NOT NULL,
    finished_at       TEXT,
    UNIQUE (deployment_id, phase, position)
);

INSERT INTO hook_runs_new
    (id, deployment_id, phase, position, hook_job_id, idempotency_token, dispatched_job_id,
     state, timeout_s, error, started_at, finished_at)
SELECT id, deployment_id, phase, 0, hook_job_id, idempotency_token, dispatched_job_id,
     state, timeout_s, error, started_at, finished_at
FROM hook_runs;

DROP TABLE hook_runs;
ALTER TABLE hook_runs_new RENAME TO hook_runs;
