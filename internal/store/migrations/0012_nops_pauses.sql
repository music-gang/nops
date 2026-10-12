-- A person can pause every job at once: nops starts no new deployment for any
-- job, the ones added to git meanwhile included, until someone resumes. Like
-- job_pauses, one row per pause, kept after the resume as the record of who
-- paused and resumed and when. resumed_at is NULL while the pause is in force.
CREATE TABLE nops_pauses (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    paused_by   TEXT NOT NULL,
    paused_at   TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    resumed_by  TEXT,
    resumed_at  TEXT
);

-- At most one pause in force: every row in force has the same indexed value.
CREATE UNIQUE INDEX one_active_nops_pause ON nops_pauses ((resumed_at IS NULL))
    WHERE resumed_at IS NULL;
