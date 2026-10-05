-- A person's API token: a secret that lets a script act as them. Only the
-- SHA-256 of the secret is kept, so a leaked database does not leak a token.
-- expires_at is NULL for a token that never expires. A revoked token is deleted.
CREATE TABLE api_tokens (
    id          TEXT PRIMARY KEY,
    owner       TEXT NOT NULL,
    name        TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  TEXT NOT NULL,
    expires_at  TEXT
);
