-- A deployment applying a job whose Nomad deployment waits for a manual
-- promotion of its canaries is waiting for a person, not failing to become
-- healthy: the apply timeout does not run meanwhile. promotion_wait_since is
-- when nops first saw it wait, promoted_at when it saw the canaries promoted
-- (the apply timeout counts from there). Both are NULL for every other
-- deployment.
ALTER TABLE deployments ADD COLUMN promotion_wait_since TEXT;
ALTER TABLE deployments ADD COLUMN promoted_at          TEXT;
