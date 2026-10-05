package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Token is an API token as listed: who owns it and when it stops working. The
// secret itself is not kept, only its hash (docs/api.md#tokens).
type Token struct {
	ID        string
	Owner     string
	Name      string
	CreatedAt time.Time
	ExpiresAt time.Time // zero for a token that never expires
}

// CreateToken stores a token for owner, identified by the hash of its secret,
// and returns it. A zero expiresAt means it never expires.
func (s *Store) CreateToken(ctx context.Context, owner, name, hash string, expiresAt time.Time) (Token, error) {
	if owner == "" || name == "" || hash == "" {
		return Token{}, errors.New("create token: owner, name and hash are required")
	}
	t := Token{ID: s.newID(), Owner: owner, Name: name}
	now := s.now()
	t.CreatedAt = now
	var expires any
	if !expiresAt.IsZero() {
		t.ExpiresAt = expiresAt
		expires = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens (id, owner, name, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, t.ID, owner, name, hash, now.UTC().Format(time.RFC3339Nano), expires)
	if err != nil {
		return Token{}, fmt.Errorf("create token for %s: %w", owner, err)
	}
	return t, nil
}

// Tokens returns every token, newest first, expired ones included: they stay
// listed until someone revokes them.
func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, owner, name, created_at, expires_at
		FROM api_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created string
		var expires sql.NullString
		if err := rows.Scan(&t.ID, &t.Owner, &t.Name, &created, &expires); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		if t.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("parse token time %q: %w", created, err)
		}
		if t.ExpiresAt, err = parseTime(expires.String); err != nil {
			return nil, fmt.Errorf("parse token expiry %q: %w", expires.String, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return out, nil
}

// RevokeToken deletes the token, and returns ErrNotFound if there is none.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke token %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke token %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("revoke token %s: %w", id, ErrNotFound)
	}
	return nil
}

// TokenOwner returns who owns the token with this hash, or ErrNotFound if there
// is none or it has expired.
func (s *Store) TokenOwner(ctx context.Context, hash string) (string, error) {
	var owner string
	var expires sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT owner, expires_at FROM api_tokens WHERE token_hash = ?`, hash).Scan(&owner, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	// Compared as times, not as text: RFC3339Nano drops trailing zeros, so two
	// stamps of one moment do not always sort alike.
	at, err := parseTime(expires.String)
	if err != nil {
		return "", fmt.Errorf("parse token expiry %q: %w", expires.String, err)
	}
	if !at.IsZero() && !s.now().Before(at) {
		return "", ErrNotFound
	}
	return owner, nil
}
