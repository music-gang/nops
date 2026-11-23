-- A person can dismiss an orphan: nops stops reporting a job it deployed that
-- is gone from git and still runs in Nomad. One row per job, holding the
-- job's latest completed deployment when it was dismissed: the dismissal
-- holds while that deployment is still the latest completed one.
CREATE TABLE orphan_dismissals (
    namespace     TEXT NOT NULL,
    job_id        TEXT NOT NULL,
    deployment_id TEXT NOT NULL REFERENCES deployments (id),
    dismissed_by  TEXT NOT NULL,
    dismissed_at  TEXT NOT NULL,
    PRIMARY KEY (namespace, job_id)
) WITHOUT ROWID;

-- A new deployment of the job that completes ends its dismissal: the job is
-- back in nops's hands, and a later removal from git is reported again.
CREATE TRIGGER orphan_dismissals_void AFTER UPDATE OF completed_id ON latest_deployments
    WHEN new.completed_id IS NOT old.completed_id BEGIN
    DELETE FROM orphan_dismissals
        WHERE namespace = new.namespace AND job_id = new.job_id
          AND deployment_id IS NOT new.completed_id;
END;
