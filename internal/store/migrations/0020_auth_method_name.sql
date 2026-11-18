-- A binding rule names its auth method by the label of the auth block, which
-- the configuration chooses, not by its type. SQLite cannot drop a CHECK
-- constraint, so the table is rebuilt without the one on auth_method.
CREATE TABLE acl_binding_rules_new (
    id           TEXT PRIMARY KEY,
    description  TEXT NOT NULL,
    auth_method  TEXT NOT NULL CHECK (auth_method <> ''),
    selector     TEXT NOT NULL,
    bind_type    TEXT NOT NULL CHECK (bind_type IN ('policy', 'management')),
    bind_name    TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    modified_at  TEXT NOT NULL
);

INSERT INTO acl_binding_rules_new
    (id, description, auth_method, selector, bind_type, bind_name, created_at, modified_at)
SELECT id, description, auth_method, selector, bind_type, bind_name, created_at, modified_at
FROM acl_binding_rules;

DROP TABLE acl_binding_rules;
ALTER TABLE acl_binding_rules_new RENAME TO acl_binding_rules;
