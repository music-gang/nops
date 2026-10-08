package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

var admin = Audit{AccessorID: "acc-admin", Actor: "alice"}

func TestACLPolicyCreateUpdateDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.PutACLPolicy(ctx, ACLPolicy{Name: "readers", Description: "see", Rules: `namespace "*" { policy = "read" }`}, admin); err != nil {
		t.Fatalf("PutACLPolicy: %v", err)
	}
	p, err := s.ACLPolicy(ctx, "readers")
	if err != nil || p.Description != "see" || p.CreatedAt.IsZero() || !p.CreatedAt.Equal(p.ModifiedAt) {
		t.Fatalf("ACLPolicy = %+v, %v, want the saved policy created and modified together", p, err)
	}

	if err := s.PutACLPolicy(ctx, ACLPolicy{Name: "readers", Description: "see more", Rules: `namespace "prod" { policy = "read" }`}, admin); err != nil {
		t.Fatalf("PutACLPolicy again: %v", err)
	}
	q, _ := s.ACLPolicy(ctx, "readers")
	if q.Rules != `namespace "prod" { policy = "read" }` || q.Description != "see more" || !q.CreatedAt.Equal(p.CreatedAt) || !q.ModifiedAt.After(p.ModifiedAt) {
		t.Errorf("after an update: %+v, want new rules, the same creation time and a later modification", q)
	}

	if err := s.PutACLPolicy(ctx, ACLPolicy{Name: "writers", Rules: `namespace "*" { policy = "write" }`}, admin); err != nil {
		t.Fatal(err)
	}
	list, err := s.ACLPolicies(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "readers" || list[1].Name != "writers" {
		t.Errorf("ACLPolicies = %+v, %v, want readers and writers by name", list, err)
	}

	if err := s.DeleteACLPolicy(ctx, "readers", admin); err != nil {
		t.Fatalf("DeleteACLPolicy: %v", err)
	}
	if _, err := s.ACLPolicy(ctx, "readers"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted ACL policy: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteACLPolicy(ctx, "readers", admin); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: err = %v, want ErrNotFound", err)
	}
}

func TestPutACLPolicyRequiresWhatItNeeds(t *testing.T) {
	s := newTestStore(t)
	for name, tc := range map[string]struct {
		p  ACLPolicy
		by Audit
	}{
		"a name": {ACLPolicy{Rules: "x"}, admin},
		"rules":  {ACLPolicy{Name: "x"}, admin},
		"who":    {ACLPolicy{Name: "x", Rules: "x"}, Audit{}},
	} {
		if err := s.PutACLPolicy(context.Background(), tc.p, tc.by); err == nil {
			t.Errorf("without %s: no error", name)
		}
	}
}

func TestACLTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	c, err := s.CreateACLToken(ctx, ACLToken{Name: "ci", Type: TokenClient, Policies: []string{"b", "a", "a"}, ACL: true,
		CreatorAccessorID: "bootstrap", CreatorName: "bootstrap"}, "hash-1", admin)
	if err != nil {
		t.Fatalf("CreateACLToken: %v", err)
	}
	if c.AccessorID == "" || c.CreatedAt.IsZero() {
		t.Fatalf("CreateACLToken = %+v, want an accessor ID and a creation time", c)
	}

	got, err := s.ACLTokenBySecret(ctx, "hash-1", true)
	if err != nil || got.AccessorID != c.AccessorID || got.Type != TokenClient || !slices.Equal(got.Policies, []string{"a", "b"}) ||
		got.CreatorAccessorID != "bootstrap" || got.CreatorName != "bootstrap" {
		t.Errorf("ACLTokenBySecret = %+v, %v, want the token with ACL policies a and b, once each, and its creator", got, err)
	}
	if _, err := s.ACLTokenBySecret(ctx, "other", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown hash: err = %v, want ErrNotFound", err)
	}
	byID, err := s.ACLToken(ctx, c.AccessorID)
	if err != nil || byID.Name != "ci" {
		t.Errorf("ACLToken = %+v, %v", byID, err)
	}

	if err := s.RevokeACLToken(ctx, c.AccessorID, admin); err != nil {
		t.Fatalf("RevokeACLToken: %v", err)
	}
	if _, err := s.ACLTokenBySecret(ctx, "hash-1", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked token: err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeACLToken(ctx, c.AccessorID, admin); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking twice: err = %v, want ErrNotFound", err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM acl_token_policies`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d ACL policy names left after the revoke, %v; want 0", left, err)
	}
}

func TestACLTokenWorksOnlyInTheModeItWasCreatedIn(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateACLToken(ctx, ACLToken{Name: "off", Type: TokenManagement}, "h-off", admin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateACLToken(ctx, ACLToken{Name: "on", Type: TokenManagement, ACL: true}, "h-on", admin); err != nil {
		t.Fatal(err)
	}
	for hash, tc := range map[string]struct{ acl, ok bool }{
		"h-off": {false, true}, "h-on": {true, true},
	} {
		if _, err := s.ACLTokenBySecret(ctx, hash, tc.acl); err != nil {
			t.Errorf("%s with the ACL %v: %v", hash, tc.acl, err)
		}
		if _, err := s.ACLTokenBySecret(ctx, hash, !tc.acl); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s with the ACL %v: err = %v, want ErrNotFound", hash, !tc.acl, err)
		}
	}
}

func TestACLTokenExpires(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t) // the clock advances one second per call
	exp := t0.Add(time.Minute)
	if _, err := s.CreateACLToken(ctx, ACLToken{Name: "soon", Type: TokenManagement, ExpiresAt: exp}, "h", admin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ACLTokenBySecret(ctx, "h", false); err != nil {
		t.Fatalf("before it expires: %v", err)
	}
	for range 70 {
		s.now()
	}
	if _, err := s.ACLTokenBySecret(ctx, "h", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("after it expires: err = %v, want ErrNotFound", err)
	}
	list, err := s.ACLTokens(ctx)
	if err != nil || len(list) != 1 || !list[0].ExpiresAt.Equal(exp) {
		t.Errorf("ACLTokens = %+v, %v, want the expired token still listed", list, err)
	}
}

func TestACLTokensListNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, n := range []string{"first", "second", "third"} {
		if _, err := s.CreateACLToken(ctx, ACLToken{Name: n, Type: TokenManagement}, "h-"+n, admin); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ACLTokens(ctx)
	if err != nil || len(list) != 3 || list[0].Name != "third" || list[2].Name != "first" {
		t.Errorf("ACLTokens = %+v, %v, want newest first", list, err)
	}
}

func TestCreateACLTokenRequiresWhatItNeeds(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for name, tc := range map[string]struct {
		t    ACLToken
		hash string
		by   Audit
	}{
		"a name":                       {ACLToken{Type: TokenClient}, "h", admin},
		"a hash":                       {ACLToken{Name: "x", Type: TokenClient}, "", admin},
		"who":                          {ACLToken{Name: "x", Type: TokenClient}, "h", Audit{}},
		"a known type":                 {ACLToken{Name: "x", Type: "root"}, "h", admin},
		"no ACL policies on a manager": {ACLToken{Name: "x", Type: TokenManagement, Policies: []string{"a"}}, "h", admin},
	} {
		if _, err := s.CreateACLToken(ctx, tc.t, tc.hash, tc.by); err == nil {
			t.Errorf("without %s: no error", name)
		}
	}
}

func TestACLTokenHashIsUnique(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateACLToken(ctx, ACLToken{Name: "a", Type: TokenManagement}, "same", admin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateACLToken(ctx, ACLToken{Name: "b", Type: TokenManagement}, "same", admin); err == nil {
		t.Error("two tokens with one hash")
	}
}

func TestACLChangesLogWhoDidWhat(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	other := Audit{AccessorID: "acc-bob", Actor: "bob"}

	if err := s.PutACLPolicy(ctx, ACLPolicy{Name: "p", Rules: "x"}, admin); err != nil {
		t.Fatal(err)
	}
	if err := s.PutACLPolicy(ctx, ACLPolicy{Name: "p", Rules: "y"}, admin); err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateACLToken(ctx, ACLToken{Name: "t", Type: TokenManagement}, "h", other)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeACLToken(ctx, tok.AccessorID, other); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteACLPolicy(ctx, "p", admin); err != nil {
		t.Fatal(err)
	}
	// A change that fails leaves no trace.
	if err := s.DeleteACLPolicy(ctx, "p", admin); err == nil {
		t.Fatal("deleting twice worked")
	}

	got, err := s.ACLChanges(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ who, action, kind, object string }
	var rows []row
	for _, c := range got {
		rows = append(rows, row{c.AccessorID + "/" + c.Actor, c.Action, c.Kind, c.Object})
	}
	want := []row{
		{"acc-admin/alice", "delete", "acl-policy", "p"},
		{"acc-bob/bob", "revoke", "token", tok.AccessorID},
		{"acc-bob/bob", "create", "token", tok.AccessorID},
		{"acc-admin/alice", "update", "acl-policy", "p"},
		{"acc-admin/alice", "create", "acl-policy", "p"},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("ACLChanges = %v, want %v", rows, want)
	}
	if got[0].Time.IsZero() || !got[0].Time.After(got[1].Time) {
		t.Errorf("times %v, %v: want newest first", got[0].Time, got[1].Time)
	}
	if two, _ := s.ACLChanges(ctx, 2); len(two) != 2 {
		t.Errorf("ACLChanges(2) returned %d rows", len(two))
	}
}

func TestEventsRecordTheAccessor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	d := mustCreate(t, s, newDep("web"))

	if err := s.Transition(ctx, d.ID, StatePendingApproval, Transition{From: StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(WithAccessor(ctx, "acc-bob"), d.ID, StateRejected,
		Transition{From: StatePendingApproval, Actor: "bob", DecidedBy: "bob"}); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, d.ID)
	if err != nil || len(events) < 2 {
		t.Fatalf("Events = %+v, %v", events, err)
	}
	if first, last := events[0], events[len(events)-1]; first.AccessorID != "" || last.AccessorID != "acc-bob" || last.Actor != "bob" {
		t.Errorf("accessors %q then %q, want none then acc-bob", first.AccessorID, last.AccessorID)
	}
}
