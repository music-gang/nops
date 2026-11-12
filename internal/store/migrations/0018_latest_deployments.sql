-- The latest deployment of each job and its latest completed one, by
-- (created_at, id), so the Jobs page and the orphan check read one row per job
-- instead of the whole history. Triggers keep it: deployments are never
-- deleted, and `completed` has no exits.
CREATE TABLE latest_deployments (
    namespace    TEXT NOT NULL,
    job_id       TEXT NOT NULL,
    latest_id    TEXT NOT NULL REFERENCES deployments (id),
    completed_id TEXT REFERENCES deployments (id),
    PRIMARY KEY (namespace, job_id)
) WITHOUT ROWID;

INSERT INTO latest_deployments (namespace, job_id, latest_id)
    SELECT namespace, job_id, id FROM deployments d
    WHERE id = (SELECT x.id FROM deployments x
        WHERE x.namespace = d.namespace AND x.job_id = d.job_id
        ORDER BY x.created_at DESC, x.id DESC LIMIT 1);
UPDATE latest_deployments SET completed_id = (SELECT x.id FROM deployments x
    WHERE x.namespace = latest_deployments.namespace AND x.job_id = latest_deployments.job_id
      AND x.state = 'completed'
    ORDER BY x.created_at DESC, x.id DESC LIMIT 1);

-- A deployment is created before it can complete, so the job's row exists by
-- then. Each trigger moves its pointer only forward.
CREATE TRIGGER latest_deployments_insert AFTER INSERT ON deployments BEGIN
    INSERT INTO latest_deployments (namespace, job_id, latest_id)
        VALUES (new.namespace, new.job_id, new.id)
        ON CONFLICT (namespace, job_id) DO UPDATE SET latest_id = new.id
        WHERE (new.created_at, new.id) >
            (SELECT created_at, id FROM deployments WHERE id = latest_deployments.latest_id);
END;
CREATE TRIGGER latest_deployments_completed AFTER UPDATE OF state ON deployments
    WHEN new.state = 'completed' AND old.state <> 'completed' BEGIN
    UPDATE latest_deployments SET completed_id = new.id
        WHERE namespace = new.namespace AND job_id = new.job_id
          AND (completed_id IS NULL OR (new.created_at, new.id) >
            (SELECT created_at, id FROM deployments WHERE id = latest_deployments.completed_id));
END;
