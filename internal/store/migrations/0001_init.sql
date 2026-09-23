CREATE TABLE deployments (
    id           TEXT PRIMARY KEY,
    job_id       TEXT NOT NULL,
    namespace    TEXT NOT NULL,
    commit_sha   TEXT NOT NULL,
    spec_hash    TEXT NOT NULL,
    job_spec     TEXT NOT NULL,
    plan_diff    TEXT,
    policy       TEXT NOT NULL CHECK (policy IN ('auto', 'approval')),
    state        TEXT NOT NULL CHECK (state IN (
        'detected', 'pending_approval', 'pre_hook', 'applying', 'post_hook',
        'completed', 'failed', 'rejected', 'superseded')),
    cas_index    INTEGER NOT NULL,
    applied_index INTEGER,
    eval_id      TEXT,
    error        TEXT,
    decided_by   TEXT,
    decided_at   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

-- Per-job lock: at most one active deployment per (namespace, job).
CREATE UNIQUE INDEX one_active_per_job ON deployments (namespace, job_id)
    WHERE state IN ('detected', 'pending_approval', 'pre_hook', 'applying', 'post_hook');

CREATE INDEX deployments_history ON deployments (created_at DESC);

CREATE TABLE hook_runs (
    id                TEXT PRIMARY KEY,
    deployment_id     TEXT NOT NULL REFERENCES deployments (id),
    phase             TEXT NOT NULL CHECK (phase IN ('pre', 'post')),
    hook_job_id       TEXT NOT NULL,
    idempotency_token TEXT NOT NULL,
    dispatched_job_id TEXT,
    state             TEXT NOT NULL CHECK (state IN (
        'dispatching', 'running', 'succeeded', 'failed', 'timed_out')),
    timeout_s         INTEGER NOT NULL,
    error             TEXT,
    started_at        TEXT NOT NULL,
    finished_at       TEXT,
    UNIQUE (deployment_id, phase)
);

CREATE TABLE events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    deployment_id TEXT NOT NULL REFERENCES deployments (id),
    ts            TEXT NOT NULL,
    from_state    TEXT NOT NULL,
    to_state      TEXT NOT NULL,
    actor         TEXT NOT NULL,
    message       TEXT NOT NULL
);

CREATE INDEX events_by_deployment ON events (deployment_id, id);
