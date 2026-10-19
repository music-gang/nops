-- The job file and the var-file a deployment's spec was parsed from, so apply
-- registers them with the spec as the version's source. NULL for a deployment
-- created before they were recorded: it registers without a source.
ALTER TABLE deployments ADD COLUMN job_source TEXT;
ALTER TABLE deployments ADD COLUMN job_vars TEXT;
