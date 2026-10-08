package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The two types of token (docs/acl.md#tokens).
const (
	TokenManagement = "management"
	TokenClient     = "client"
)

// Audit says who made a change to the ACL: the accessor ID of the token used,
// and who it stands for. It stays readable after the token is gone.
type Audit struct {
	AccessorID string
	Actor      string
}

type accessorKey struct{}

// WithAccessor returns a context that makes the events of an action carry the
// accessor ID of the token that asked for it.
func WithAccessor(ctx context.Context, accessorID string) context.Context {
	return context.WithValue(ctx, accessorKey{}, accessorID)
}

// AccessorFrom returns the accessor ID WithAccessor put in the context.
func AccessorFrom(ctx context.Context) string {
	id, _ := ctx.Value(accessorKey{}).(string)
	return id
}

// ACLPolicy is an ACL policy as saved: its name and its rules in HCL.
type ACLPolicy struct {
	Name        string
	Description string
	Rules       string
	CreatedAt   time.Time
	ModifiedAt  time.Time
}

// ACLToken is a token as listed. The secret is not kept, only its hash.
type ACLToken struct {
	AccessorID        string
	Name              string
	Type              string
	Policies          []string // names of the ACL policies a client token carries
	ACL               bool     // created with the ACL on
	CreatedAt         time.Time
	ExpiresAt         time.Time // zero for a token that never expires
	CreatorAccessorID string
	CreatorName       string
}

// ACLChange is one row of the log of changes to ACL policies and tokens.
type ACLChange struct {
	ID         int64
	Time       time.Time
	AccessorID string
	Actor      string
	Action     string // "create", "update", "delete" or "revoke"
	Kind       string // "acl-policy" or "token"
	Object     string // the name of an ACL policy, or the accessor ID of a token
}

func (s *Store) insertChange(ctx context.Context, tx *sql.Tx, by Audit, action, kind, object string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO acl_changes (ts, accessor_id, actor, action, kind, object)
		VALUES (?, ?, ?, ?, ?, ?)`, s.ts(), by.AccessorID, by.Actor, action, kind, object)
	if err != nil {
		return fmt.Errorf("log %s of %s %s: %w", action, kind, object, err)
	}
	return nil
}

// PutACLPolicy creates the ACL policy or replaces its description and rules,
// and logs the change in the same transaction. The caller has checked the rules.
func (s *Store) PutACLPolicy(ctx context.Context, p ACLPolicy, by Audit) error {
	if p.Name == "" || p.Rules == "" || by.Actor == "" {
		return errors.New("put acl policy: name, rules and who are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("put acl policy %s: %w", p.Name, err)
	}
	defer tx.Rollback()

	now := s.ts()
	res, err := tx.ExecContext(ctx, `UPDATE acl_policies SET description = ?, rules = ?, modified_at = ? WHERE name = ?`,
		p.Description, p.Rules, now, p.Name)
	if err != nil {
		return fmt.Errorf("put acl policy %s: %w", p.Name, err)
	}
	action := "update"
	if n, _ := res.RowsAffected(); n == 0 {
		action = "create"
		if _, err := tx.ExecContext(ctx, `INSERT INTO acl_policies (name, description, rules, created_at, modified_at)
			VALUES (?, ?, ?, ?, ?)`, p.Name, p.Description, p.Rules, now, now); err != nil {
			return fmt.Errorf("put acl policy %s: %w", p.Name, err)
		}
	}
	if err := s.insertChange(ctx, tx, by, action, "acl-policy", p.Name); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("put acl policy %s: commit: %w", p.Name, err)
	}
	return nil
}

const aclPolicyColumns = `name, description, rules, created_at, modified_at`

func scanACLPolicy(r scanner) (ACLPolicy, error) {
	var p ACLPolicy
	var created, modified string
	if err := r.Scan(&p.Name, &p.Description, &p.Rules, &created, &modified); err != nil {
		return ACLPolicy{}, err
	}
	var err error
	if p.CreatedAt, err = parseTime(created); err != nil {
		return ACLPolicy{}, fmt.Errorf("parse acl policy time %q: %w", created, err)
	}
	if p.ModifiedAt, err = parseTime(modified); err != nil {
		return ACLPolicy{}, fmt.Errorf("parse acl policy time %q: %w", modified, err)
	}
	return p, nil
}

// ACLPolicy returns the ACL policy, or ErrNotFound.
func (s *Store) ACLPolicy(ctx context.Context, name string) (ACLPolicy, error) {
	p, err := scanACLPolicy(s.db.QueryRowContext(ctx, `SELECT `+aclPolicyColumns+` FROM acl_policies WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return ACLPolicy{}, fmt.Errorf("acl policy %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return ACLPolicy{}, fmt.Errorf("read acl policy %s: %w", name, err)
	}
	return p, nil
}

// ACLPolicies returns every ACL policy by name.
func (s *Store) ACLPolicies(ctx context.Context) ([]ACLPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+aclPolicyColumns+` FROM acl_policies ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list acl policies: %w", err)
	}
	defer rows.Close()
	var out []ACLPolicy
	for rows.Next() {
		p, err := scanACLPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan acl policy: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list acl policies: %w", err)
	}
	return out, nil
}

// DeleteACLPolicy deletes the ACL policy and returns ErrNotFound if there is
// none. A token that carries it keeps the name, and gets nothing from it.
func (s *Store) DeleteACLPolicy(ctx context.Context, name string, by Audit) error {
	if by.Actor == "" {
		return errors.New("delete acl policy: who is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete acl policy %s: %w", name, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM acl_policies WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete acl policy %s: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete acl policy %s: %w", name, ErrNotFound)
	}
	if err := s.insertChange(ctx, tx, by, "delete", "acl-policy", name); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete acl policy %s: commit: %w", name, err)
	}
	return nil
}

// CreateACLToken stores a token identified by the hash of its secret, and
// returns it with its accessor ID and its creation time. The creator is
// t.CreatorAccessorID and t.CreatorName. Only a client token carries ACL
// policies.
func (s *Store) CreateACLToken(ctx context.Context, t ACLToken, secretHash string, by Audit) (ACLToken, error) {
	if t.Name == "" || secretHash == "" || by.Actor == "" {
		return ACLToken{}, errors.New("create token: name, hash and who are required")
	}
	if t.Type != TokenManagement && t.Type != TokenClient {
		return ACLToken{}, fmt.Errorf("create token: type %q is not %s or %s", t.Type, TokenManagement, TokenClient)
	}
	if t.Type == TokenManagement && len(t.Policies) > 0 {
		return ACLToken{}, errors.New("create token: a management token carries no ACL policies")
	}

	t.AccessorID = s.newID()
	now := s.now()
	t.CreatedAt = now
	var expires any
	if !t.ExpiresAt.IsZero() {
		expires = t.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ACLToken{}, fmt.Errorf("create token %s: %w", t.Name, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO acl_tokens
		(accessor_id, secret_hash, name, type, acl, created_at, expires_at, creator_accessor_id, creator_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.AccessorID, secretHash, t.Name, t.Type, t.ACL, now.UTC().Format(time.RFC3339Nano), expires,
		t.CreatorAccessorID, t.CreatorName); err != nil {
		return ACLToken{}, fmt.Errorf("create token %s: %w", t.Name, err)
	}
	for _, p := range t.Policies {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO acl_token_policies (accessor_id, policy_name) VALUES (?, ?)`,
			t.AccessorID, p); err != nil {
			return ACLToken{}, fmt.Errorf("create token %s: %w", t.Name, err)
		}
	}
	if err := s.insertChange(ctx, tx, by, "create", "token", t.AccessorID); err != nil {
		return ACLToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return ACLToken{}, fmt.Errorf("create token %s: commit: %w", t.Name, err)
	}
	return t, nil
}

const aclTokenColumns = `accessor_id, name, type, acl, created_at, expires_at, creator_accessor_id, creator_name`

func scanACLToken(r scanner) (ACLToken, error) {
	var t ACLToken
	var created string
	var expires sql.NullString
	if err := r.Scan(&t.AccessorID, &t.Name, &t.Type, &t.ACL, &created, &expires, &t.CreatorAccessorID, &t.CreatorName); err != nil {
		return ACLToken{}, err
	}
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return ACLToken{}, fmt.Errorf("parse token time %q: %w", created, err)
	}
	if t.ExpiresAt, err = parseTime(expires.String); err != nil {
		return ACLToken{}, fmt.Errorf("parse token expiry %q: %w", expires.String, err)
	}
	return t, nil
}

// loadPolicies fills in the ACL policy names of the tokens.
func (s *Store) loadPolicies(ctx context.Context, tokens []ACLToken) error {
	for i := range tokens {
		rows, err := s.db.QueryContext(ctx, `SELECT policy_name FROM acl_token_policies WHERE accessor_id = ? ORDER BY policy_name`,
			tokens[i].AccessorID)
		if err != nil {
			return fmt.Errorf("read the acl policies of token %s: %w", tokens[i].AccessorID, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return fmt.Errorf("scan acl policy name: %w", err)
			}
			tokens[i].Policies = append(tokens[i].Policies, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read the acl policies of token %s: %w", tokens[i].AccessorID, err)
		}
	}
	return nil
}

// ACLTokens returns every token, newest first, expired ones included: they
// stay listed until someone revokes them.
func (s *Store) ACLTokens(ctx context.Context) ([]ACLToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+aclTokenColumns+` FROM acl_tokens ORDER BY created_at DESC, accessor_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	var out []ACLToken
	for rows.Next() {
		t, err := scanACLToken(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan token: %w", err)
		}
		out = append(out, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return out, s.loadPolicies(ctx, out)
}

// ACLToken returns the token with this accessor ID, or ErrNotFound. An
// expired one is still returned.
func (s *Store) ACLToken(ctx context.Context, accessorID string) (ACLToken, error) {
	t, err := scanACLToken(s.db.QueryRowContext(ctx, `SELECT `+aclTokenColumns+` FROM acl_tokens WHERE accessor_id = ?`, accessorID))
	if errors.Is(err, sql.ErrNoRows) {
		return ACLToken{}, fmt.Errorf("token %s: %w", accessorID, ErrNotFound)
	}
	if err != nil {
		return ACLToken{}, fmt.Errorf("read token %s: %w", accessorID, err)
	}
	out := []ACLToken{t}
	if err := s.loadPolicies(ctx, out); err != nil {
		return ACLToken{}, err
	}
	return out[0], nil
}

// ACLTokenBySecret returns the token with this secret hash, or ErrNotFound if
// there is none, it has expired, or it was created while the ACL was in the
// other mode: acl is whether the ACL is on now.
func (s *Store) ACLTokenBySecret(ctx context.Context, hash string, acl bool) (ACLToken, error) {
	t, err := scanACLToken(s.db.QueryRowContext(ctx, `SELECT `+aclTokenColumns+` FROM acl_tokens WHERE secret_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return ACLToken{}, ErrNotFound
	}
	if err != nil {
		return ACLToken{}, fmt.Errorf("read token: %w", err)
	}
	// Compared as times, not as text: RFC3339Nano drops trailing zeros, so two
	// stamps of one moment do not always sort alike.
	if t.ACL != acl || (!t.ExpiresAt.IsZero() && !s.now().Before(t.ExpiresAt)) {
		return ACLToken{}, ErrNotFound
	}
	out := []ACLToken{t}
	if err := s.loadPolicies(ctx, out); err != nil {
		return ACLToken{}, err
	}
	return out[0], nil
}

// RevokeACLToken deletes the token, and returns ErrNotFound if there is none.
func (s *Store) RevokeACLToken(ctx context.Context, accessorID string, by Audit) error {
	if by.Actor == "" {
		return errors.New("revoke token: who is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("revoke token %s: %w", accessorID, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM acl_tokens WHERE accessor_id = ?`, accessorID)
	if err != nil {
		return fmt.Errorf("revoke token %s: %w", accessorID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("revoke token %s: %w", accessorID, ErrNotFound)
	}
	if err := s.insertChange(ctx, tx, by, "revoke", "token", accessorID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("revoke token %s: commit: %w", accessorID, err)
	}
	return nil
}

// ACLChanges returns the latest changes to ACL policies and tokens, newest first.
func (s *Store) ACLChanges(ctx context.Context, limit int) ([]ACLChange, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, ts, accessor_id, actor, action, kind, object
		FROM acl_changes ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list acl changes: %w", err)
	}
	defer rows.Close()
	var out []ACLChange
	for rows.Next() {
		var c ACLChange
		var ts string
		if err := rows.Scan(&c.ID, &ts, &c.AccessorID, &c.Actor, &c.Action, &c.Kind, &c.Object); err != nil {
			return nil, fmt.Errorf("scan acl change: %w", err)
		}
		if c.Time, err = parseTime(ts); err != nil {
			return nil, fmt.Errorf("parse acl change time %q: %w", ts, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list acl changes: %w", err)
	}
	return out, nil
}
