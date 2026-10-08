-- A login ends in a token (docs/acl.md#binding-rules). origin says how a token
-- came to be: a login made it (a session), or a person or a token created it.
-- identity is the person a session stands for and creator_identity the person
-- who created a token: the issuer and sub of an OIDC login, or the method and
-- username of a basic one. Empty when no person is behind it.
ALTER TABLE acl_tokens ADD COLUMN origin TEXT NOT NULL DEFAULT 'created' CHECK (origin IN ('login', 'created'));
ALTER TABLE acl_tokens ADD COLUMN identity TEXT NOT NULL DEFAULT '';
ALTER TABLE acl_tokens ADD COLUMN creator_identity TEXT NOT NULL DEFAULT '';

-- A binding rule says what a login gets: it matches the claims of a login by
-- auth method and selector, and binds an ACL policy or management.
CREATE TABLE acl_binding_rules (
    id           TEXT PRIMARY KEY,
    description  TEXT NOT NULL,
    auth_method  TEXT NOT NULL CHECK (auth_method IN ('oidc', 'basic')),
    selector     TEXT NOT NULL,
    bind_type    TEXT NOT NULL CHECK (bind_type IN ('policy', 'management')),
    bind_name    TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    modified_at  TEXT NOT NULL
);

-- The person behind a change or an action, next to the token it was made with.
ALTER TABLE acl_changes ADD COLUMN identity TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN identity TEXT NOT NULL DEFAULT '';
