-- A person can pause a job: nops starts no new deployment for it until someone
-- resumes it. One row per pause, kept after the resume as the record of who
-- paused and resumed and when (the events table belongs to a deployment, and a
-- pause has none). resumed_at is NULL while the pause is in force.
CREATE TABLE job_pauses (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    namespace   TEXT NOT NULL,
    job_id      TEXT NOT NULL,
    paused_by   TEXT NOT NULL,
    paused_at   TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    resumed_by  TEXT,
    resumed_at  TEXT
);

-- At most one pause in force per job, enforced by the database like
-- one_active_per_job.
CREATE UNIQUE INDEX one_active_pause_per_job ON job_pauses (namespace, job_id)
    WHERE resumed_at IS NULL;
