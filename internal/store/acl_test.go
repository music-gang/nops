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
	tok, err := s.CreateACLToken(ctx, ACLToken{Name: "soon", Type: TokenManagement, ExpiresAt: exp}, "h", admin)
	if err != nil {
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
	if _, err := s.ACLToken(ctx, tok.AccessorID); !errors.Is(err, ErrNotFound) {
		t.Errorf("ACLToken after it expires: err = %v, want ErrNotFound", err)
	}
	list, err := s.ACLTokens(ctx)
	if err != nil || len(list) != 0 {
		t.Errorf("ACLTokens = %+v, %v, want the expired token left out", list, err)
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

func TestBindingRuleCreateUpdateDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	r, err := s.PutBindingRule(ctx, BindingRule{Description: "ops", AuthMethod: "oidc", Selector: `"ops" in list.groups`,
		BindType: "policy", BindName: "operator"}, admin)
	if err != nil || r.ID == "" || r.CreatedAt.IsZero() || !r.CreatedAt.Equal(r.ModifiedAt) {
		t.Fatalf("PutBindingRule = %+v, %v, want a rule with an ID, created and modified together", r, err)
	}

	r.Selector, r.BindType, r.BindName = "", "management", ""
	u, err := s.PutBindingRule(ctx, r, admin)
	if err != nil || u.ID != r.ID || u.Selector != "" || u.BindType != "management" || !u.CreatedAt.Equal(r.CreatedAt) || !u.ModifiedAt.After(r.ModifiedAt) {
		t.Errorf("after an update: %+v, %v, want the same rule, new binding, a later modification", u, err)
	}
	if _, err := s.PutBindingRule(ctx, BindingRule{ID: "nope", AuthMethod: "oidc", BindType: "management"}, admin); !errors.Is(err, ErrNotFound) {
		t.Errorf("updating an unknown rule: err = %v, want ErrNotFound", err)
	}

	if _, err := s.PutBindingRule(ctx, BindingRule{AuthMethod: "basic", BindType: "management"}, admin); err != nil {
		t.Fatal(err)
	}
	list, err := s.BindingRules(ctx)
	if err != nil || len(list) != 2 || list[0].ID != r.ID || list[1].AuthMethod != "basic" {
		t.Errorf("BindingRules = %+v, %v, want the two rules oldest first", list, err)
	}

	if err := s.DeleteBindingRule(ctx, r.ID, admin); err != nil {
		t.Fatalf("DeleteBindingRule: %v", err)
	}
	if _, err := s.BindingRule(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted rule: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteBindingRule(ctx, r.ID, admin); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: err = %v, want ErrNotFound", err)
	}

	got, _ := s.ACLChanges(ctx, 10)
	var kinds []string
	for _, c := range got {
		kinds = append(kinds, c.Action+" "+c.Kind)
	}
	want := []string{"delete binding-rule", "create binding-rule", "update binding-rule", "create binding-rule"}
	if !slices.Equal(kinds, want) {
		t.Errorf("changes = %v, want %v", kinds, want)
	}
}

func TestPutBindingRuleRequiresWhatItNeeds(t *testing.T) {
	s := newTestStore(t)
	for name, tc := range map[string]struct {
		r  BindingRule
		by Audit
	}{
		"who":                         {BindingRule{AuthMethod: "oidc", BindType: "management"}, Audit{}},
		"an auth method":              {BindingRule{BindType: "management"}, admin},
		"a known bind type":           {BindingRule{AuthMethod: "oidc", BindType: "root"}, admin},
		"a policy to bind":            {BindingRule{AuthMethod: "oidc", BindType: "policy"}, admin},
		"no policy on management":     {BindingRule{AuthMethod: "oidc", BindType: "management", BindName: "x"}, admin},
		"a known auth method (CHECK)": {BindingRule{AuthMethod: "ldap", BindType: "management"}, admin},
	} {
		if _, err := s.PutBindingRule(context.Background(), tc.r, tc.by); err == nil {
			t.Errorf("without %s: no error", name)
		}
	}
	if list, _ := s.BindingRules(context.Background()); len(list) != 0 {
		t.Errorf("%d rules saved by refused requests", len(list))
	}
}

func TestExpiredTokensAreDeleted(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t) // the clock advances one second per call
	person := Audit{Actor: "alice", Identity: "basic:alice"}

	if _, err := s.CreateACLToken(ctx, ACLToken{Name: "alice", Type: TokenClient, Policies: []string{"p"}, Origin: OriginLogin,
		Identity: "basic:alice", CreatorName: "alice", CreatorIdentity: "basic:alice", ExpiresAt: t0.Add(time.Minute)}, "h-sess", person); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateACLToken(ctx, ACLToken{Name: "ci", Type: TokenManagement, ExpiresAt: t0.Add(time.Minute)}, "h-ci", admin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateACLToken(ctx, ACLToken{Name: "forever", Type: TokenManagement}, "h-forever", admin); err != nil {
		t.Fatal(err)
	}
	got, err := s.ACLTokenBySecret(ctx, "h-sess", false)
	if err != nil || got.Origin != OriginLogin || got.Identity != "basic:alice" || got.CreatorIdentity != "basic:alice" {
		t.Fatalf("session = %+v, %v, want a login token for basic:alice", got, err)
	}
	if n, err := s.DeleteExpiredTokens(ctx); err != nil || n != 0 {
		t.Fatalf("DeleteExpiredTokens before the expiry = %d, %v, want 0", n, err)
	}

	for range 70 {
		s.now()
	}
	if n, err := s.DeleteExpiredTokens(ctx); err != nil || n != 2 {
		t.Fatalf("DeleteExpiredTokens = %d, %v, want 2: the session and the created token", n, err)
	}
	var kept string
	if err := s.db.QueryRow(`SELECT group_concat(name) FROM acl_tokens`).Scan(&kept); err != nil || kept != "forever" {
		t.Errorf("tokens left = %q, %v; want only the one that never expires", kept, err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM acl_token_policies`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d ACL policy names left after the delete, %v; want 0", left, err)
	}
}

func TestIdentityIsRecordedWithTheChangeAndTheEvent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	by := Audit{AccessorID: "acc-alice", Actor: "alice", Identity: "oidc:https://idp#sub-1"}

	if err := s.PutACLPolicy(ctx, ACLPolicy{Name: "p", Rules: "x"}, by); err != nil {
		t.Fatal(err)
	}
	changes, err := s.ACLChanges(ctx, 1)
	if err != nil || len(changes) != 1 || changes[0].Identity != by.Identity {
		t.Errorf("ACLChanges = %+v, %v, want the identity of the person", changes, err)
	}

	d := mustCreate(t, s, newDep("web"))
	if err := s.Transition(ctx, d.ID, StatePendingApproval, Transition{From: StateDetected, Actor: "nops"}); err != nil {
		t.Fatal(err)
	}
	ctx = WithIdentity(WithAccessor(ctx, by.AccessorID), by.Identity)
	if err := s.Transition(ctx, d.ID, StateRejected, Transition{From: StatePendingApproval, Actor: "alice", DecidedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	events, _ := s.Events(ctx, d.ID)
	if last := events[len(events)-1]; last.Identity != by.Identity || last.AccessorID != "acc-alice" {
		t.Errorf("last event = %+v, want the accessor and the identity", last)
	}
	if first := events[0]; first.Identity != "" {
		t.Errorf("first event identity = %q, want none for Nops itself", first.Identity)
	}
}

// makeToken stores a token for the revocation tests and returns its accessor ID.
func makeToken(t *testing.T, s *Store, tok ACLToken) string {
	t.Helper()
	if tok.Type == "" {
		tok.Type = TokenManagement
	}
	made, err := s.CreateACLToken(context.Background(), tok, "h-"+tok.Name, admin)
	if err != nil {
		t.Fatal(err)
	}
	return made.AccessorID
}

func namesOf(tokens []ACLToken) []string {
	var out []string
	for _, t := range tokens {
		out = append(out, t.Name)
	}
	slices.Sort(out)
	return out
}

func TestRevokeEverySessionOfAPerson(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	session := func(name, identity string) string {
		return makeToken(t, s, ACLToken{Name: name, Type: TokenClient, Origin: OriginLogin, Identity: identity,
			CreatorName: name, CreatorIdentity: identity, ExpiresAt: t0.Add(time.Hour)})
	}
	session("alice-laptop", "basic:alice")
	alicePhone := session("alice-phone", "basic:alice")
	session("bob", "basic:bob")
	makeToken(t, s, ACLToken{Name: "alice-ci", CreatorAccessorID: alicePhone, CreatorName: "alice", CreatorIdentity: "basic:alice"})

	rev := Revocation{SessionsOf: "basic:alice"}
	preview, err := s.TokensToRevoke(ctx, rev)
	if err != nil || !slices.Equal(namesOf(preview), []string{"alice-laptop", "alice-phone"}) {
		t.Fatalf("TokensToRevoke = %v, %v, want the two sessions of alice", namesOf(preview), err)
	}
	ids, err := s.RevokeACLTokens(ctx, rev, admin)
	if err != nil || len(ids) != 2 {
		t.Fatalf("RevokeACLTokens = %v, %v, want 2", ids, err)
	}
	left, _ := s.ACLTokens(ctx)
	if got := namesOf(left); !slices.Equal(got, []string{"alice-ci", "bob"}) {
		t.Errorf("left = %v, want the session of bob and the token alice created", got)
	}
	changes, _ := s.ACLChanges(ctx, 10)
	if changes[0].Action != "revoke" || changes[1].Action != "revoke" || changes[0].Actor != "alice" {
		t.Errorf("latest changes = %+v, want a revoke for each session", changes[:2])
	}
}

func TestRevokeWhatACreatorCreatedAndWhatThoseCreated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// The bootstrap token creates m, m creates t; alice, by a session, creates
	// ci, and ci creates deploy.
	m := makeToken(t, s, ACLToken{Name: "m", CreatorAccessorID: "bootstrap", CreatorName: "bootstrap"})
	makeToken(t, s, ACLToken{Name: "t", CreatorAccessorID: m, CreatorName: "m"})
	sess := makeToken(t, s, ACLToken{Name: "alice", Type: TokenClient, Origin: OriginLogin, Identity: "basic:alice",
		CreatorName: "alice", CreatorIdentity: "basic:alice", ExpiresAt: t0.Add(time.Hour)})
	ci := makeToken(t, s, ACLToken{Name: "ci", CreatorAccessorID: sess, CreatorName: "alice", CreatorIdentity: "basic:alice"})
	makeToken(t, s, ACLToken{Name: "deploy", CreatorAccessorID: ci, CreatorName: "ci"})
	makeToken(t, s, ACLToken{Name: "other", CreatorAccessorID: "acc-other", CreatorName: "other"})

	cases := []struct {
		rev  Revocation
		want []string
	}{
		{Revocation{CreatorAccessorID: "bootstrap"}, []string{"m", "t"}},
		{Revocation{CreatorAccessorID: m}, []string{"t"}},
		{Revocation{CreatorIdentity: "basic:alice"}, []string{"ci", "deploy"}}, // not her session
		{Revocation{CreatorAccessorID: "nobody"}, nil},
	}
	for _, tc := range cases {
		got, err := s.TokensToRevoke(ctx, tc.rev)
		if err != nil || !slices.Equal(namesOf(got), tc.want) {
			t.Errorf("TokensToRevoke(%+v) = %v, %v, want %v", tc.rev, namesOf(got), err, tc.want)
		}
	}

	ids, err := s.RevokeACLTokens(ctx, Revocation{CreatorAccessorID: "bootstrap"}, admin)
	if err != nil || len(ids) != 2 {
		t.Fatalf("RevokeACLTokens = %v, %v, want m and t", ids, err)
	}
	left, _ := s.ACLTokens(ctx)
	if got := namesOf(left); !slices.Equal(got, []string{"alice", "ci", "deploy", "other"}) {
		t.Errorf("left = %v", got)
	}
	if ids, err := s.RevokeACLTokens(ctx, Revocation{CreatorAccessorID: "nobody"}, admin); err != nil || ids == nil || len(ids) != 0 {
		t.Errorf("revoking nothing = %#v, %v, want an empty list and no error", ids, err)
	}
}

func TestARevocationNamesExactlyOneSubject(t *testing.T) {
	s := newTestStore(t)
	for _, rev := range []Revocation{{}, {SessionsOf: "basic:a", CreatorIdentity: "basic:a"}, {CreatorIdentity: "basic:a", CreatorAccessorID: "x"}} {
		if _, err := s.TokensToRevoke(context.Background(), rev); err == nil {
			t.Errorf("TokensToRevoke(%+v): no error", rev)
		}
		if _, err := s.RevokeACLTokens(context.Background(), rev, admin); err == nil {
			t.Errorf("RevokeACLTokens(%+v): no error", rev)
		}
	}
	if _, err := s.RevokeACLTokens(context.Background(), Revocation{SessionsOf: "basic:a"}, Audit{}); err == nil {
		t.Error("RevokeACLTokens without who: no error")
	}
}
