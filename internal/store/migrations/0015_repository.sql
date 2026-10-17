-- The name of the repository a deployment's job file was read from, so its
-- commit links to the right place. A deployment created before Nops read
-- several repositories belongs to the one named default, the -git-* options.
ALTER TABLE deployments ADD COLUMN repository TEXT NOT NULL DEFAULT 'default';
