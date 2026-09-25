-- The hooks a deployment runs, frozen when it is created: which hook, the hash
-- of its spec, the spec itself, and the ID of the Nomad job that is registered
-- from it (`<hook-id>-<8 hex of the hash>`) right before the hook is dispatched.
-- position orders the hooks of one phase; it is always 0 until a phase can have
-- more than one hook.
CREATE TABLE deployment_hooks (
    deployment_id TEXT    NOT NULL REFERENCES deployments (id),
    phase         TEXT    NOT NULL CHECK (phase IN ('pre', 'post')),
    position      INTEGER NOT NULL,
    hook_id       TEXT    NOT NULL,
    revision      TEXT    NOT NULL,
    spec_hash     TEXT    NOT NULL,
    job_spec      TEXT    NOT NULL,
    PRIMARY KEY (deployment_id, phase, position)
);

-- Which revisions are still needed: a lookup by revision, joined to the state
-- of the deployment.
CREATE INDEX deployment_hooks_by_revision ON deployment_hooks (revision);
