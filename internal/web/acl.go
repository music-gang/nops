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
	return store.Audit{AccessorID: sub.accessorID, Actor: sub.actor}
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
		CreatorAccessorID: sub.accessorID, CreatorName: sub.actor,
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
