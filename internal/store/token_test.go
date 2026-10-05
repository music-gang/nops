package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTokenOwner(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	tok, err := s.CreateToken(ctx, "alice", "ci", "hash-1", time.Time{})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if tok.ID == "" || tok.Owner != "alice" || tok.Name != "ci" || tok.CreatedAt.IsZero() || !tok.ExpiresAt.IsZero() {
		t.Errorf("CreateToken = %+v, want an ID, owner alice, name ci, created, never expiring", tok)
	}
	if owner, err := s.TokenOwner(ctx, "hash-1"); err != nil || owner != "alice" {
		t.Errorf("TokenOwner = %q, %v, want alice", owner, err)
	}
	if _, err := s.TokenOwner(ctx, "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TokenOwner of an unknown hash: err = %v, want ErrNotFound", err)
	}
}

func TestTokenExpires(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// The test clock moves one second per call: a token that expires in 1 hour
	// is still valid right away and gone once the clock is past its expiry.
	soon := s.now().Add(time.Hour)
	if _, err := s.CreateToken(ctx, "alice", "ci", "hash-1", soon); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TokenOwner(ctx, "hash-1"); err != nil {
		t.Fatalf("TokenOwner before the expiry: %v", err)
	}
	past := s.now
	s.now = func() time.Time { return past().Add(2 * time.Hour) }
	if _, err := s.TokenOwner(ctx, "hash-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TokenOwner after the expiry: err = %v, want ErrNotFound", err)
	}
	// An expired token is still listed, so someone can revoke it.
	if got, err := s.Tokens(ctx); err != nil || len(got) != 1 {
		t.Errorf("Tokens = %+v, %v, want the expired token listed", got, err)
	}
}

func TestTokensListEveryOwnerNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for i, c := range []struct{ owner, name string }{{"alice", "first"}, {"bob", "second"}} {
		if _, err := s.CreateToken(ctx, c.owner, c.name, "hash-"+c.name, time.Time{}); err != nil {
			t.Fatalf("CreateToken %d: %v", i, err)
		}
	}
	got, err := s.Tokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "second" || got[0].Owner != "bob" || got[1].Name != "first" {
		t.Errorf("Tokens = %+v, want bob's second token, then alice's first", got)
	}
}

func TestRevokeToken(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tok, err := s.CreateToken(ctx, "alice", "ci", "hash-1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := s.TokenOwner(ctx, "hash-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TokenOwner after the revoke: err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeToken(ctx, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second RevokeToken: err = %v, want ErrNotFound", err)
	}
}

func TestCreateTokenRequiresWhatItNeeds(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []struct{ owner, name, hash string }{{"", "ci", "h"}, {"alice", "", "h"}, {"alice", "ci", ""}} {
		if _, err := s.CreateToken(context.Background(), c.owner, c.name, c.hash, time.Time{}); err == nil {
			t.Errorf("CreateToken(%q, %q, %q) succeeded, want an error", c.owner, c.name, c.hash)
		}
	}
}

func TestTokenHashIsUnique(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateToken(ctx, "alice", "a", "same", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken(ctx, "bob", "b", "same", time.Time{}); err == nil {
		t.Error("a second token with the same hash was stored, want an error")
	}
}
