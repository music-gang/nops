package web

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/secret"
	"github.com/music-gang/nops/internal/store"
)

// maxTokenName is the longest name a token takes, in bytes.
const maxTokenName = 100

// maxRules is the most an ACL policy's rules may hold, in bytes.
const maxRules = 16 << 10

// changesShown is how many of the latest changes to ACL policies and tokens
// the Administration page and the API show.
const changesShown = 100

// policyName is what an ACL policy may be called.
var policyName = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// invalidInput is a mistake of the caller: the dashboard answers it with a 400
// page and the API with a 400.
type invalidInput string

func (e invalidInput) Error() string { return string(e) }

func invalidf(format string, args ...any) error { return invalidInput(fmt.Sprintf(format, args...)) }

// auditOf is who a change is recorded as.
func auditOf(sub *subject) store.Audit {
	return store.Audit{AccessorID: sub.accessorID, Actor: sub.actor, Identity: sub.identity}
}

// putPolicy checks an ACL policy and saves it. Invalid rules save nothing.
func (s *server) putPolicy(ctx context.Context, sub *subject, p store.ACLPolicy) error {
	if !policyName.MatchString(p.Name) {
		return invalidf("an ACL policy name has 1 to 128 letters, digits and -")
	}
	if strings.TrimSpace(p.Rules) == "" {
		return invalidf("the rules are empty")
	}
	if len(p.Rules) > maxRules {
		return invalidf("the rules hold %d bytes at most", maxRules)
	}
	if _, err := acl.Parse(p.Rules); err != nil {
		return invalidf("%v", err)
	}
	if err := s.access.PutACLPolicy(ctx, p, auditOf(sub)); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "ACL policy saved", "acl_policy", p.Name, "actor", sub.actor, "accessor_id", sub.accessorID)
	return nil
}

// maxSelector is the longest selector a binding rule takes, in bytes.
const maxSelector = 1 << 10

// maxDescription is the longest description of a binding rule, in bytes.
const maxDescription = 200

// authMethods are the names a binding rule gives an auth method.
var authMethods = []string{"oidc", "basic"}

// putBindingRule checks a binding rule and saves it: a new one when r.ID is
// empty, else the rule with that ID. A rule that does not check saves nothing.
func (s *server) putBindingRule(ctx context.Context, sub *subject, r store.BindingRule) (store.BindingRule, error) {
	r.Description, r.Selector = strings.TrimSpace(r.Description), strings.TrimSpace(r.Selector)
	if len(r.Description) > maxDescription {
		return store.BindingRule{}, invalidf("the description holds %d bytes at most", maxDescription)
	}
	if !slices.Contains(authMethods, r.AuthMethod) {
		return store.BindingRule{}, invalidf("the auth method is %s", strings.Join(authMethods, " or "))
	}
	if len(r.Selector) > maxSelector {
		return store.BindingRule{}, invalidf("the selector holds %d bytes at most", maxSelector)
	}
	if err := acl.ValidSelector(r.Selector); err != nil {
		return store.BindingRule{}, invalidf("%v", err)
	}
	switch r.BindType {
	case acl.BindManagement:
		if r.BindName != "" {
			return store.BindingRule{}, invalidf("a rule that binds management binds no ACL policy")
		}
	case acl.BindPolicy:
		if _, err := s.access.ACLPolicy(ctx, r.BindName); errors.Is(err, store.ErrNotFound) {
			return store.BindingRule{}, invalidf("the ACL policy %q does not exist", r.BindName)
		} else if err != nil {
			return store.BindingRule{}, err
		}
	default:
		return store.BindingRule{}, invalidf("the bind type is %s or %s", acl.BindPolicy, acl.BindManagement)
	}
	saved, err := s.access.PutBindingRule(ctx, r, auditOf(sub))
	if err != nil {
		return store.BindingRule{}, err
	}
	s.log.InfoContext(ctx, "binding rule saved", "binding_rule", saved.ID, "actor", sub.actor, "accessor_id", sub.accessorID)
	return saved, nil
}

// newToken is what a caller asks for.
type newToken struct {
	Name      string
	Type      string
	Policies  []string
	ExpiresIn time.Duration // zero: never
}

// createToken makes a token for the subject and returns it with its secret, the
// only time the secret exists.
func (s *server) createToken(ctx context.Context, sub *subject, in newToken) (store.ACLToken, string, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > maxTokenName {
		return store.ACLToken{}, "", invalidf("a token needs a name of 1 to %d bytes, so you can tell it from the others", maxTokenName)
	}
	if in.Type == "" {
		in.Type = store.TokenClient
	}
	switch in.Type {
	case store.TokenManagement:
		if len(in.Policies) > 0 {
			return store.ACLToken{}, "", invalidf("a management token can do everything: it carries no ACL policies")
		}
	case store.TokenClient:
		for _, name := range in.Policies {
			if _, err := s.access.ACLPolicy(ctx, name); errors.Is(err, store.ErrNotFound) {
				return store.ACLToken{}, "", invalidf("the ACL policy %q does not exist", name)
			} else if err != nil {
				return store.ACLToken{}, "", err
			}
		}
	default:
		return store.ACLToken{}, "", invalidf("the type of a token is %s or %s", store.TokenManagement, store.TokenClient)
	}
	if in.ExpiresIn < 0 {
		return store.ACLToken{}, "", invalidf("a token cannot expire in the past")
	}
	t := store.ACLToken{
		Name: in.Name, Type: in.Type, Policies: slices.Compact(slices.Sorted(slices.Values(in.Policies))), ACL: s.aclOn,
		CreatorAccessorID: sub.accessorID, CreatorName: sub.actor, CreatorIdentity: sub.identity,
	}
	if in.ExpiresIn > 0 {
		t.ExpiresAt = s.now().Add(in.ExpiresIn)
	}
	sec := secret.New()
	t, err := s.access.CreateACLToken(ctx, t, hashToken(sec), auditOf(sub))
	if err != nil {
		return store.ACLToken{}, "", err
	}
	s.log.InfoContext(ctx, "token created", "token", t.AccessorID, "name", t.Name, "type", t.Type, "actor", sub.actor, "accessor_id", sub.accessorID)
	return t, sec, nil
}

// checkRevocation says whether rev names one person or one token.
func checkRevocation(rev store.Revocation) error {
	if !rev.Valid() {
		return invalidf("name one person by identity, or one creator by identity or by accessor ID")
	}
	return nil
}

// revokeTokens revokes in one action the tokens rev names, and returns their
// accessor IDs.
func (s *server) revokeTokens(ctx context.Context, sub *subject, rev store.Revocation) ([]string, error) {
	if err := checkRevocation(rev); err != nil {
		return nil, err
	}
	ids, err := s.access.RevokeACLTokens(ctx, rev, auditOf(sub))
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "tokens revoked", "count", len(ids), "sessions_of", rev.SessionsOf,
		"creator_identity", rev.CreatorIdentity, "creator_accessor_id", rev.CreatorAccessorID,
		"actor", sub.actor, "accessor_id", sub.accessorID)
	return ids, nil
}
