-- What a deployment says about the commit it came from (gitwatch keeps only
-- the head, so it is stored when the deployment is created) and whether a
-- human asked to retry it after it failed or was rejected.
ALTER TABLE deployments ADD COLUMN commit_subject TEXT;
ALTER TABLE deployments ADD COLUMN commit_author  TEXT;
ALTER TABLE deployments ADD COLUMN retried_by     TEXT;
ALTER TABLE deployments ADD COLUMN retried_at     TEXT;

-- The dashboard lists the deployments of one job and the latest of every job.
CREATE INDEX deployments_by_job ON deployments (namespace, job_id, created_at DESC);
