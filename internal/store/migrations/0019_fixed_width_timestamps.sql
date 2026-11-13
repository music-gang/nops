-- Timestamps were written with the trailing zeros of the fraction removed, so
-- within one second their text order was the reverse of time order. Rewrite
-- them with all nine digits, then find the latest deployments again on the
-- rewritten values. No trigger fires: those on deployments fire on INSERT and
-- on UPDATE OF state.

UPDATE deployments SET created_at = substr(created_at, 1, 19) || '.' || substr(CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';
UPDATE deployments SET updated_at = substr(updated_at, 1, 19) || '.' || substr(CASE WHEN substr(updated_at, 20, 1) = '.' THEN substr(updated_at, 21, length(updated_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';
UPDATE deployments SET decided_at = substr(decided_at, 1, 19) || '.' || substr(CASE WHEN substr(decided_at, 20, 1) = '.' THEN substr(decided_at, 21, length(decided_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z'
    WHERE decided_at IS NOT NULL AND decided_at <> '';
UPDATE deployments SET retried_at = substr(retried_at, 1, 19) || '.' || substr(CASE WHEN substr(retried_at, 20, 1) = '.' THEN substr(retried_at, 21, length(retried_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z'
    WHERE retried_at IS NOT NULL AND retried_at <> '';
UPDATE deployments SET promotion_wait_since = substr(promotion_wait_since, 1, 19) || '.' || substr(CASE WHEN substr(promotion_wait_since, 20, 1) = '.' THEN substr(promotion_wait_since, 21, length(promotion_wait_since) - 21) ELSE '' END || '000000000', 1, 9) || 'Z'
    WHERE promotion_wait_since IS NOT NULL AND promotion_wait_since <> '';
UPDATE deployments SET promoted_at = substr(promoted_at, 1, 19) || '.' || substr(CASE WHEN substr(promoted_at, 20, 1) = '.' THEN substr(promoted_at, 21, length(promoted_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z'
    WHERE promoted_at IS NOT NULL AND promoted_at <> '';

UPDATE hook_runs SET started_at = substr(started_at, 1, 19) || '.' || substr(CASE WHEN substr(started_at, 20, 1) = '.' THEN substr(started_at, 21, length(started_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';
UPDATE hook_runs SET finished_at = substr(finished_at, 1, 19) || '.' || substr(CASE WHEN substr(finished_at, 20, 1) = '.' THEN substr(finished_at, 21, length(finished_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z'
    WHERE finished_at IS NOT NULL AND finished_at <> '';

UPDATE events SET ts = substr(ts, 1, 19) || '.' || substr(CASE WHEN substr(ts, 20, 1) = '.' THEN substr(ts, 21, length(ts) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';

UPDATE job_pauses SET paused_at = substr(paused_at, 1, 19) || '.' || substr(CASE WHEN substr(paused_at, 20, 1) = '.' THEN substr(paused_at, 21, length(paused_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';
UPDATE job_pauses SET resumed_at = substr(resumed_at, 1, 19) || '.' || substr(CASE WHEN substr(resumed_at, 20, 1) = '.' THEN substr(resumed_at, 21, length(resumed_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z'
    WHERE resumed_at IS NOT NULL AND resumed_at <> '';

UPDATE nops_pauses SET paused_at = substr(paused_at, 1, 19) || '.' || substr(CASE WHEN substr(paused_at, 20, 1) = '.' THEN substr(paused_at, 21, length(paused_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';
UPDATE nops_pauses SET resumed_at = substr(resumed_at, 1, 19) || '.' || substr(CASE WHEN substr(resumed_at, 20, 1) = '.' THEN substr(resumed_at, 21, length(resumed_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z'
    WHERE resumed_at IS NOT NULL AND resumed_at <> '';

UPDATE window_lifts SET requested_at = substr(requested_at, 1, 19) || '.' || substr(CASE WHEN substr(requested_at, 20, 1) = '.' THEN substr(requested_at, 21, length(requested_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';

UPDATE acl_policies SET created_at = substr(created_at, 1, 19) || '.' || substr(CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';
UPDATE acl_policies SET modified_at = substr(modified_at, 1, 19) || '.' || substr(CASE WHEN substr(modified_at, 20, 1) = '.' THEN substr(modified_at, 21, length(modified_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';

UPDATE acl_tokens SET created_at = substr(created_at, 1, 19) || '.' || substr(CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';
UPDATE acl_tokens SET expires_at = substr(expires_at, 1, 19) || '.' || substr(CASE WHEN substr(expires_at, 20, 1) = '.' THEN substr(expires_at, 21, length(expires_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z'
    WHERE expires_at IS NOT NULL AND expires_at <> '';

UPDATE acl_binding_rules SET created_at = substr(created_at, 1, 19) || '.' || substr(CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';
UPDATE acl_binding_rules SET modified_at = substr(modified_at, 1, 19) || '.' || substr(CASE WHEN substr(modified_at, 20, 1) = '.' THEN substr(modified_at, 21, length(modified_at) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';

UPDATE acl_changes SET ts = substr(ts, 1, 19) || '.' || substr(CASE WHEN substr(ts, 20, 1) = '.' THEN substr(ts, 21, length(ts) - 21) ELSE '' END || '000000000', 1, 9) || 'Z';

UPDATE latest_deployments SET latest_id = (SELECT x.id FROM deployments x
    WHERE x.namespace = latest_deployments.namespace AND x.job_id = latest_deployments.job_id
    ORDER BY x.created_at DESC, x.id DESC LIMIT 1);
UPDATE latest_deployments SET completed_id = (SELECT x.id FROM deployments x
    WHERE x.namespace = latest_deployments.namespace AND x.job_id = latest_deployments.job_id
      AND x.state = 'completed'
    ORDER BY x.created_at DESC, x.id DESC LIMIT 1);
