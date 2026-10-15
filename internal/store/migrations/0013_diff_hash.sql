-- The hash of the full plan diff a deployment was detected with, before
-- redaction. A live index that moves while the diff stays the same is a count
-- changed on a group with a scaling block, not an outside edit. NULL for a
-- deployment created before it was recorded: there any move of the index is an
-- edit, as it was then.
ALTER TABLE deployments ADD COLUMN diff_hash TEXT;
