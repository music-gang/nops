-- A deployment a person's retry led to says which one it retries: the page of
-- each shows the other. NULL for every deployment that is not a retry.
ALTER TABLE deployments ADD COLUMN retry_of TEXT REFERENCES deployments(id);
