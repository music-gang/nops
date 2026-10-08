-- The access control list (docs/acl.md). The upgrade deletes the API tokens:
-- they cannot say which ACL policies they carry.
DROP TABLE api_tokens;

-- An ACL policy is a name and its rules in HCL. A token names the ACL policies
-- it carries without a foreign key: an ACL policy that no longer exists
-- grants nothing.
CREATE TABLE acl_policies (
    name         TEXT PRIMARY KEY,
    description  TEXT NOT NULL,
    rules        TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    modified_at  TEXT NOT NULL
);

-- A token is public by its accessor_id; only the SHA-256 of its secret is kept.
-- acl says whether it was created with the ACL on: a token works only in the
-- mode it was created in. expires_at is NULL for a token that never expires.
-- The creator is kept by value, so the record outlives the creator's token.
CREATE TABLE acl_tokens (
    accessor_id          TEXT PRIMARY KEY,
    secret_hash          TEXT NOT NULL UNIQUE,
    name                 TEXT NOT NULL,
    type                 TEXT NOT NULL CHECK (type IN ('management', 'client')),
    acl                  INTEGER NOT NULL CHECK (acl IN (0, 1)),
    created_at           TEXT NOT NULL,
    expires_at           TEXT,
    creator_accessor_id  TEXT NOT NULL,
    creator_name         TEXT NOT NULL
);

CREATE TABLE acl_token_policies (
    accessor_id  TEXT NOT NULL REFERENCES acl_tokens (accessor_id) ON DELETE CASCADE,
    policy_name  TEXT NOT NULL,
    PRIMARY KEY (accessor_id, policy_name)
);

-- Every change to an ACL policy or a token: who made it, and with which token.
CREATE TABLE acl_changes (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           TEXT NOT NULL,
    accessor_id  TEXT NOT NULL,
    actor        TEXT NOT NULL,
    action       TEXT NOT NULL,
    kind         TEXT NOT NULL,
    object       TEXT NOT NULL
);

-- The token an action was made with. Empty for Nops itself, and for what
-- happened before the ACL.
ALTER TABLE events ADD COLUMN accessor_id TEXT NOT NULL DEFAULT '';
