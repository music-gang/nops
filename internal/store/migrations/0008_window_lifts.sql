-- A person can ask to deploy a job held by its sync window now. The request
-- waits here, one per job, until a detection cycle uses it (the deployment it
-- creates deletes it) or drops it: nothing else can carry it, since a held job
-- has no deployment to write it on. spec_hash is the spec the person saw.
CREATE TABLE window_lifts (
    namespace    TEXT NOT NULL,
    job_id       TEXT NOT NULL,
    spec_hash    TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    requested_at TEXT NOT NULL,
    PRIMARY KEY (namespace, job_id)
);

-- Who asked, on the deployment a Deploy now led to. NULL for every other one.
ALTER TABLE deployments ADD COLUMN window_lifted_by TEXT;
