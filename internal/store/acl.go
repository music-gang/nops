package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/music-gang/nops/internal/acl"
)

// The two types of token (docs/acl.md#tokens).
const (
	TokenManagement = "management"
	TokenClient     = "client"
)

// How a token came to be.
const (
	OriginLogin   = "login"   // a login made it: a session
	OriginCreated = "created" // a person or a token created it
)

// Audit says who made a change to the ACL: the accessor ID of the token used,
// and who it stands for, by name and, for a person, by identity (the issuer and
// sub of an OIDC login, or the method and username of a basic one). It stays
// readable after the token is gone.
type Audit struct {
	AccessorID string
	Actor      string
	Identity   string
}

type accessorKey struct{}
type identityKey struct{}

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

// WithIdentity returns a context that makes the events of an action carry the
// identity of the person who asked for it.
func WithIdentity(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// IdentityFrom returns the identity WithIdentity put in the context.
func IdentityFrom(ctx context.Context) string {
	id, _ := ctx.Value(identityKey{}).(string)
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
	Origin            string   // OriginLogin or OriginCreated; empty means OriginCreated
	Identity          string   // the person a session stands for; empty for a created token
	CreatedAt         time.Time
	ExpiresAt         time.Time // zero for a token that never expires
	CreatorAccessorID string
	CreatorName       string
	CreatorIdentity   string // the person who created the token, when it was one
}

// ACLChange is one row of the log of changes to ACL policies and tokens.
type ACLChange struct {
	ID         int64
	Time       time.Time
	AccessorID string
	Actor      string
	Identity   string
	Action     string // "create", "update", "delete" or "revoke"
	Kind       string // "acl-policy", "binding-rule" or "token"
	Object     string // the name of an ACL policy, the ID of a binding rule, or the accessor ID of a token
}

func (s *Store) insertChange(ctx context.Context, tx *sql.Tx, by Audit, action, kind, object string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO acl_changes (ts, accessor_id, actor, identity, action, kind, object)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, s.ts(), by.AccessorID, by.Actor, by.Identity, action, kind, object)
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
	if t.Origin == "" {
		t.Origin = OriginCreated
	}
	if t.Origin != OriginLogin && t.Origin != OriginCreated {
		return ACLToken{}, fmt.Errorf("create token: origin %q is not %s or %s", t.Origin, OriginLogin, OriginCreated)
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
		(accessor_id, secret_hash, name, type, acl, origin, identity, created_at, expires_at,
		 creator_accessor_id, creator_name, creator_identity)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.AccessorID, secretHash, t.Name, t.Type, t.ACL, t.Origin, t.Identity, now.UTC().Format(time.RFC3339Nano), expires,
		t.CreatorAccessorID, t.CreatorName, t.CreatorIdentity); err != nil {
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

const aclTokenColumns = `accessor_id, name, type, acl, origin, identity, created_at, expires_at, creator_accessor_id, creator_name, creator_identity`

func scanACLToken(r scanner) (ACLToken, error) {
	var t ACLToken
	var created string
	var expires sql.NullString
	if err := r.Scan(&t.AccessorID, &t.Name, &t.Type, &t.ACL, &t.Origin, &t.Identity, &created, &expires,
		&t.CreatorAccessorID, &t.CreatorName, &t.CreatorIdentity); err != nil {
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

// expired says whether the token has expired. Expiry is compared as times, not
// as text: RFC3339Nano drops trailing zeros, so two stamps of one moment do not
// always sort alike.
func (s *Store) expired(t ACLToken) bool {
	return !t.ExpiresAt.IsZero() && !s.now().Before(t.ExpiresAt)
}

// queryTokens returns the tokens the query selects, newest first, leaving out
// the expired ones: an expired token is as good as deleted, and
// DeleteExpiredTokens deletes it later.
func (s *Store) queryTokens(ctx context.Context, query string, args ...any) ([]ACLToken, error) {
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at DESC, accessor_id DESC`, args...)
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
		if !s.expired(t) {
			out = append(out, t)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return out, s.loadPolicies(ctx, out)
}

// ACLTokens returns every token that has not expired, newest first.
func (s *Store) ACLTokens(ctx context.Context) ([]ACLToken, error) {
	return s.queryTokens(ctx, `SELECT `+aclTokenColumns+` FROM acl_tokens`)
}

// ACLToken returns the token with this accessor ID, or ErrNotFound if there is
// none or it has expired.
func (s *Store) ACLToken(ctx context.Context, accessorID string) (ACLToken, error) {
	t, err := scanACLToken(s.db.QueryRowContext(ctx, `SELECT `+aclTokenColumns+` FROM acl_tokens WHERE accessor_id = ?`, accessorID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && s.expired(t)) {
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
	if t.ACL != acl || s.expired(t) {
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

// A Revocation names the tokens to revoke in one action. Exactly one field is
// set: the sessions of a person, or the tokens a person or a token created
// together with, in turn, the tokens those created.
type Revocation struct {
	SessionsOf        string // the identity of a person
	CreatorIdentity   string // the identity of a person
	CreatorAccessorID string // the accessor ID of a token, "bootstrap" included
}

// Valid says whether exactly one field is set.
func (r Revocation) Valid() bool {
	set := 0
	for _, f := range []string{r.SessionsOf, r.CreatorIdentity, r.CreatorAccessorID} {
		if f != "" {
			set++
		}
	}
	return set == 1
}

// query returns the SQL that selects the tokens of r, and its argument.
func (r Revocation) query() (string, string, error) {
	if !r.Valid() {
		return "", "", errors.New("revoke tokens: set exactly one of a person's sessions, a creator identity or a creator accessor ID")
	}
	if r.SessionsOf != "" {
		return `SELECT ` + aclTokenColumns + ` FROM acl_tokens WHERE origin = 'login' AND identity = ?`, r.SessionsOf, nil
	}
	// A session records its own person as its creator: only created tokens seed
	// the search. Each step adds the tokens the ones found so far created.
	seed, arg := `creator_identity = ?`, r.CreatorIdentity
	if r.CreatorAccessorID != "" {
		seed, arg = `creator_accessor_id = ?`, r.CreatorAccessorID
	}
	return `WITH RECURSIVE taken (accessor_id) AS (
			SELECT accessor_id FROM acl_tokens WHERE origin = 'created' AND ` + seed + `
			UNION
			SELECT t.accessor_id FROM acl_tokens t JOIN taken ON t.creator_accessor_id = taken.accessor_id
		)
		SELECT ` + aclTokenColumns + ` FROM acl_tokens WHERE accessor_id IN (SELECT accessor_id FROM taken)`, arg, nil
}

// TokensToRevoke returns the tokens RevokeACLTokens would revoke, newest first.
func (s *Store) TokensToRevoke(ctx context.Context, r Revocation) ([]ACLToken, error) {
	q, arg, err := r.query()
	if err != nil {
		return nil, err
	}
	return s.queryTokens(ctx, q, arg)
}

// RevokeACLTokens deletes the tokens of r in one transaction, logs a revoke
// for each, and returns their accessor IDs, newest first. None is not an error.
func (s *Store) RevokeACLTokens(ctx context.Context, r Revocation, by Audit) ([]string, error) {
	if by.Actor == "" {
		return nil, errors.New("revoke tokens: who is required")
	}
	tokens, err := s.TokensToRevoke(ctx, r)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("revoke tokens: %w", err)
	}
	defer tx.Rollback()
	ids := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, err := tx.ExecContext(ctx, `DELETE FROM acl_tokens WHERE accessor_id = ?`, t.AccessorID); err != nil {
			return nil, fmt.Errorf("revoke token %s: %w", t.AccessorID, err)
		}
		if err := s.insertChange(ctx, tx, by, "revoke", "token", t.AccessorID); err != nil {
			return nil, err
		}
		ids = append(ids, t.AccessorID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("revoke tokens: commit: %w", err)
	}
	return ids, nil
}

// DeleteExpiredTokens deletes the tokens that have expired, sessions and
// created ones alike, and returns how many. It is not a change to the ACL:
// nothing is logged.
func (s *Store) DeleteExpiredTokens(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT accessor_id, expires_at FROM acl_tokens WHERE expires_at IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("list tokens: %w", err)
	}
	var expired []string
	for rows.Next() {
		var t ACLToken
		var at string
		if err := rows.Scan(&t.AccessorID, &at); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan token: %w", err)
		}
		if t.ExpiresAt, err = parseTime(at); err != nil {
			rows.Close()
			return 0, fmt.Errorf("parse token expiry %q: %w", at, err)
		}
		if s.expired(t) {
			expired = append(expired, t.AccessorID)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("list tokens: %w", err)
	}
	for _, id := range expired {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM acl_tokens WHERE accessor_id = ?`, id); err != nil {
			return 0, fmt.Errorf("delete token %s: %w", id, err)
		}
	}
	return len(expired), nil
}

// BindingRule is a binding rule as saved (docs/acl.md#binding-rules).
type BindingRule struct {
	ID          string
	Description string
	AuthMethod  string // "oidc" or "basic"
	Selector    string // a go-bexpr expression on the login's claims; empty matches every login
	BindType    string // acl.BindPolicy or acl.BindManagement
	BindName    string // the ACL policy a BindPolicy rule binds; empty for BindManagement
	CreatedAt   time.Time
	ModifiedAt  time.Time
}

func checkBindingRule(r BindingRule, by Audit) error {
	if by.Actor == "" {
		return errors.New("put binding rule: who is required")
	}
	if r.AuthMethod == "" {
		return errors.New("put binding rule: the auth method is required")
	}
	switch r.BindType {
	case acl.BindPolicy:
		if r.BindName == "" {
			return errors.New("put binding rule: an ACL policy to bind is required")
		}
	case acl.BindManagement:
		if r.BindName != "" {
			return errors.New("put binding rule: management binds no ACL policy")
		}
	default:
		return fmt.Errorf("put binding rule: bind type %q is not %s or %s", r.BindType, acl.BindPolicy, acl.BindManagement)
	}
	return nil
}

// PutBindingRule creates a binding rule when r.ID is empty, and returns it with
// its ID. Otherwise it replaces the rule with that ID, or returns ErrNotFound.
// The change is logged in the same transaction. The caller has checked the
// selector.
func (s *Store) PutBindingRule(ctx context.Context, r BindingRule, by Audit) (BindingRule, error) {
	if err := checkBindingRule(r, by); err != nil {
		return BindingRule{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BindingRule{}, fmt.Errorf("put binding rule: %w", err)
	}
	defer tx.Rollback()

	now := s.now()
	stamp := now.UTC().Format(time.RFC3339Nano)
	action := "update"
	if r.ID == "" {
		action = "create"
		r.ID = s.newID()
		r.CreatedAt = now
		_, err = tx.ExecContext(ctx, `INSERT INTO acl_binding_rules
			(id, description, auth_method, selector, bind_type, bind_name, created_at, modified_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Description, r.AuthMethod, r.Selector, r.BindType, r.BindName, stamp, stamp)
	} else {
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE acl_binding_rules
			SET description = ?, auth_method = ?, selector = ?, bind_type = ?, bind_name = ?, modified_at = ? WHERE id = ?`,
			r.Description, r.AuthMethod, r.Selector, r.BindType, r.BindName, stamp, r.ID)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return BindingRule{}, fmt.Errorf("binding rule %s: %w", r.ID, ErrNotFound)
			}
		}
	}
	if err != nil {
		return BindingRule{}, fmt.Errorf("put binding rule %s: %w", r.ID, err)
	}
	if err := s.insertChange(ctx, tx, by, action, "binding-rule", r.ID); err != nil {
		return BindingRule{}, err
	}
	if err := tx.Commit(); err != nil {
		return BindingRule{}, fmt.Errorf("put binding rule %s: commit: %w", r.ID, err)
	}
	return s.BindingRule(ctx, r.ID)
}

const bindingRuleColumns = `id, description, auth_method, selector, bind_type, bind_name, created_at, modified_at`

func scanBindingRule(r scanner) (BindingRule, error) {
	var b BindingRule
	var created, modified string
	if err := r.Scan(&b.ID, &b.Description, &b.AuthMethod, &b.Selector, &b.BindType, &b.BindName, &created, &modified); err != nil {
		return BindingRule{}, err
	}
	var err error
	if b.CreatedAt, err = parseTime(created); err != nil {
		return BindingRule{}, fmt.Errorf("parse binding rule time %q: %w", created, err)
	}
	if b.ModifiedAt, err = parseTime(modified); err != nil {
		return BindingRule{}, fmt.Errorf("parse binding rule time %q: %w", modified, err)
	}
	return b, nil
}

// BindingRule returns the binding rule, or ErrNotFound.
func (s *Store) BindingRule(ctx context.Context, id string) (BindingRule, error) {
	b, err := scanBindingRule(s.db.QueryRowContext(ctx, `SELECT `+bindingRuleColumns+` FROM acl_binding_rules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return BindingRule{}, fmt.Errorf("binding rule %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return BindingRule{}, fmt.Errorf("read binding rule %s: %w", id, err)
	}
	return b, nil
}

// BindingRules returns every binding rule, oldest first.
func (s *Store) BindingRules(ctx context.Context) ([]BindingRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+bindingRuleColumns+` FROM acl_binding_rules ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list binding rules: %w", err)
	}
	defer rows.Close()
	var out []BindingRule
	for rows.Next() {
		b, err := scanBindingRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan binding rule: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list binding rules: %w", err)
	}
	return out, nil
}

// DeleteBindingRule deletes the binding rule, or returns ErrNotFound. The
// sessions it bound keep what they have until they expire.
func (s *Store) DeleteBindingRule(ctx context.Context, id string, by Audit) error {
	if by.Actor == "" {
		return errors.New("delete binding rule: who is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete binding rule %s: %w", id, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM acl_binding_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete binding rule %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete binding rule %s: %w", id, ErrNotFound)
	}
	if err := s.insertChange(ctx, tx, by, "delete", "binding-rule", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete binding rule %s: commit: %w", id, err)
	}
	return nil
}

// ACLChanges returns the latest changes to ACL policies and tokens, newest first.
func (s *Store) ACLChanges(ctx context.Context, limit int) ([]ACLChange, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, ts, accessor_id, actor, identity, action, kind, object
		FROM acl_changes ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list acl changes: %w", err)
	}
	defer rows.Close()
	var out []ACLChange
	for rows.Next() {
		var c ACLChange
		var ts string
		if err := rows.Scan(&c.ID, &ts, &c.AccessorID, &c.Actor, &c.Identity, &c.Action, &c.Kind, &c.Object); err != nil {
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
