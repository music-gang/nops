-- The lists of deployments go a page at a time, newest first, by (created_at,
-- id): each order a filter narrows to has its index, so a page walks only the
-- rows of its filter.
DROP INDEX deployments_history;
DROP INDEX deployments_by_job;
CREATE INDEX deployments_page ON deployments (created_at, id);
CREATE INDEX deployments_by_job ON deployments (namespace, job_id, created_at, id);
CREATE INDEX deployments_by_namespace ON deployments (namespace, created_at, id);
CREATE INDEX deployments_by_state ON deployments (state, created_at, id);
CREATE INDEX deployments_by_commit ON deployments (commit_sha);

-- The search: parts of the job, the namespace and the commit, three
-- characters or more. It keeps the deployment's ID, not the rowid, which
-- VACUUM INTO (a backup) may renumber. The columns it copies never change once
-- a deployment is created, and deployments are never deleted.
CREATE VIRTUAL TABLE deployments_search USING fts5(
    id UNINDEXED, job_id, namespace, commit_subject, commit_author, tokenize = 'trigram');
INSERT INTO deployments_search (id, job_id, namespace, commit_subject, commit_author)
    SELECT id, job_id, namespace, coalesce(commit_subject, ''), coalesce(commit_author, '') FROM deployments;
CREATE TRIGGER deployments_search_insert AFTER INSERT ON deployments BEGIN
    INSERT INTO deployments_search (id, job_id, namespace, commit_subject, commit_author)
        VALUES (new.id, new.job_id, new.namespace, coalesce(new.commit_subject, ''), coalesce(new.commit_author, ''));
END;
