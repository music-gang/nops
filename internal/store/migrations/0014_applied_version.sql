-- The job version a deployment registered. While it waits for health, the
-- allocations of that version and of the scales after it are the ones it
-- waits for. NULL for a deployment registered before it was recorded: there
-- any move of the live index is an edit, as it was then.
ALTER TABLE deployments ADD COLUMN applied_version INTEGER;
