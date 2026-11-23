-- When Nops dropped the spec of a deployment that had ended, with its job file,
-- its var-file and its hooks' specs, after the retention period: job_spec is
-- then '' (it is NOT NULL), job_source and job_vars NULL. NULL while the
-- deployment keeps them.
ALTER TABLE deployments ADD COLUMN specs_dropped_at TEXT;

-- What is left to drop, oldest first, so a sweep reads only the deployments it
-- drops and the latest completed one of each job, which keeps its specs.
CREATE INDEX deployments_specs_to_drop ON deployments (updated_at)
    WHERE specs_dropped_at IS NULL AND state IN ('completed', 'failed', 'rejected', 'superseded');
