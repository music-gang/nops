package web

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// The endpoints under /api/acl/ (docs/api.md): ACL policies, binding rules,
// tokens and the log of changes to them. A management token uses all of them; a client token
// reads itself and the ACL policies it carries.

type apiACLPolicy struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Rules       string    `json:"rules"`
	CreatedAt   time.Time `json:"created_at"`
	ModifiedAt  time.Time `json:"modified_at"`
}

func policyOf(p store.ACLPolicy) apiACLPolicy {
	return apiACLPolicy{Name: p.Name, Description: p.Description, Rules: p.Rules, CreatedAt: p.CreatedAt, ModifiedAt: p.ModifiedAt}
}

type apiACLToken struct {
	AccessorID string `json:"accessor_id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	// Origin is "login" for a session token and "created" for the others.
	Origin string `json:"origin"`
	// Identity is the person a session token stands for.
	Identity  string    `json:"identity,omitempty"`
	Policies  []string  `json:"policies"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is absent for a token that never expires.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// CreatorAccessorID and CreatorName say who made the token: the token used,
	// and who it stands for. They stay after that token is gone.
	CreatorAccessorID string `json:"creator_accessor_id,omitempty"`
	CreatorName       string `json:"creator_name,omitempty"`
	CreatorIdentity   string `json:"creator_identity,omitempty"`
}

func tokenOf(t store.ACLToken) apiACLToken {
	return apiACLToken{
		AccessorID: t.AccessorID, Name: t.Name, Type: t.Type, Origin: t.Origin, Identity: t.Identity,
		Policies:  append([]string{}, t.Policies...),
		CreatedAt: t.CreatedAt, ExpiresAt: timePtr(t.ExpiresAt),
		CreatorAccessorID: t.CreatorAccessorID, CreatorName: t.CreatorName, CreatorIdentity: t.CreatorIdentity,
	}
}

// apiNewACLToken is a token just made: the one time its secret is shown.
type apiNewACLToken struct {
	apiACLToken
	Secret string `json:"secret"`
}

type apiChange struct {
	Time       time.Time `json:"time"`
	AccessorID string    `json:"accessor_id"`
	Actor      string    `json:"actor"`
	Identity   string    `json:"identity,omitempty"`
	Action     string    `json:"action"`
	Kind       string    `json:"kind"`
	Object     string    `json:"object"`
}

// carries reports whether the subject may read the ACL policy: a management
// token reads every one, a client token the ones it carries.
func carries(sub *subject, name string) bool {
	return sub.acl.Management() || (sub.token != nil && slices.Contains(sub.token.Policies, name))
}

// apiListPolicies is GET /api/acl/policies.
func (s *server) apiListPolicies(w http.ResponseWriter, r *http.Request) {
	sub := subjectOf(r.Context())
	all, err := s.access.ACLPolicies(r.Context())
	if err != nil {
		s.apiServerError(w, r, "list ACL policies", err)
		return
	}
	out := []apiACLPolicy{}
	for _, p := range all {
		if carries(sub, p.Name) {
			out = append(out, policyOf(p))
		}
	}
	replyJSON(w, http.StatusOK, map[string]any{"acl_policies": out})
}

// apiGetPolicy is GET /api/acl/policies/{name}.
func (s *server) apiGetPolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !carries(subjectOf(r.Context()), name) {
		apiForbidden(w, r)
		return
	}
	p, err := s.access.ACLPolicy(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "this ACL policy does not exist")
		return
	}
	if err != nil {
		s.apiServerError(w, r, "read an ACL policy", err)
		return
	}
	replyJSON(w, http.StatusOK, policyOf(p))
}

type policyBody struct {
	Description string `json:"description,omitempty"`
	Rules       string `json:"rules"`
}

// apiPutPolicy is PUT /api/acl/policies/{name}: create the ACL policy or
// replace it. Rules that do not parse save nothing and answer a 400 that says
// where.
func (s *server) apiPutPolicy(w http.ResponseWriter, r *http.Request) {
	var b policyBody
	if !readBody(w, r, &b) {
		return
	}
	err := s.putPolicy(r.Context(), subjectOf(r.Context()), store.ACLPolicy{Name: r.PathValue("name"), Description: b.Description, Rules: b.Rules})
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		apiError(w, http.StatusBadRequest, bad.Error())
	case err != nil:
		s.apiServerError(w, r, "save an ACL policy", err)
	default:
		p, err := s.access.ACLPolicy(r.Context(), r.PathValue("name"))
		if err != nil {
			s.apiServerError(w, r, "read an ACL policy", err)
			return
		}
		replyJSON(w, http.StatusOK, policyOf(p))
	}
}

// apiDeletePolicy is DELETE /api/acl/policies/{name}.
func (s *server) apiDeletePolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.access.DeleteACLPolicy(r.Context(), name, auditOf(subjectOf(r.Context())))
	switch {
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "this ACL policy does not exist")
	case err != nil:
		s.apiServerError(w, r, "delete an ACL policy", err)
	default:
		sub := subjectOf(r.Context())
		s.log.InfoContext(r.Context(), "ACL policy deleted", "acl_policy", name, "actor", sub.actor, "accessor_id", sub.accessorID)
		w.WriteHeader(http.StatusNoContent)
	}
}

type apiBindingRule struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	AuthMethod  string    `json:"auth_method"`
	Selector    string    `json:"selector"`
	BindType    string    `json:"bind_type"`
	BindName    string    `json:"bind_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ModifiedAt  time.Time `json:"modified_at"`
}

func bindingRuleOf(b store.BindingRule) apiBindingRule {
	return apiBindingRule{ID: b.ID, Description: b.Description, AuthMethod: b.AuthMethod, Selector: b.Selector,
		BindType: b.BindType, BindName: b.BindName, CreatedAt: b.CreatedAt, ModifiedAt: b.ModifiedAt}
}

type bindingRuleBody struct {
	Description string `json:"description,omitempty"`
	AuthMethod  string `json:"auth_method"`
	Selector    string `json:"selector,omitempty"`
	BindType    string `json:"bind_type"`
	BindName    string `json:"bind_name,omitempty"`
}

func (b bindingRuleBody) rule(id string) store.BindingRule {
	return store.BindingRule{ID: id, Description: b.Description, AuthMethod: b.AuthMethod, Selector: b.Selector, BindType: b.BindType, BindName: b.BindName}
}

// apiListBindingRules is GET /api/acl/binding-rules.
func (s *server) apiListBindingRules(w http.ResponseWriter, r *http.Request) {
	all, err := s.access.BindingRules(r.Context())
	if err != nil {
		s.apiServerError(w, r, "list binding rules", err)
		return
	}
	out := []apiBindingRule{}
	for _, b := range all {
		out = append(out, bindingRuleOf(b))
	}
	replyJSON(w, http.StatusOK, map[string]any{"binding_rules": out})
}

// apiGetBindingRule is GET /api/acl/binding-rules/{id}.
func (s *server) apiGetBindingRule(w http.ResponseWriter, r *http.Request) {
	b, err := s.access.BindingRule(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "this binding rule does not exist")
		return
	}
	if err != nil {
		s.apiServerError(w, r, "read a binding rule", err)
		return
	}
	replyJSON(w, http.StatusOK, bindingRuleOf(b))
}

// apiSaveBindingRule saves the body as the rule with this ID, or as a new one
// when it is empty, and answers with the status given.
func (s *server) apiSaveBindingRule(w http.ResponseWriter, r *http.Request, id string, status int) {
	var b bindingRuleBody
	if !readBody(w, r, &b) {
		return
	}
	saved, err := s.putBindingRule(r.Context(), subjectOf(r.Context()), b.rule(id))
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		apiError(w, http.StatusBadRequest, bad.Error())
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "this binding rule does not exist")
	case err != nil:
		s.apiServerError(w, r, "save a binding rule", err)
	default:
		replyJSON(w, status, bindingRuleOf(saved))
	}
}

// apiCreateBindingRule is POST /api/acl/binding-rules.
func (s *server) apiCreateBindingRule(w http.ResponseWriter, r *http.Request) {
	s.apiSaveBindingRule(w, r, "", http.StatusCreated)
}

// apiPutBindingRule is PUT /api/acl/binding-rules/{id}: replace the rule.
func (s *server) apiPutBindingRule(w http.ResponseWriter, r *http.Request) {
	s.apiSaveBindingRule(w, r, r.PathValue("id"), http.StatusOK)
}

// apiDeleteBindingRule is DELETE /api/acl/binding-rules/{id}.
func (s *server) apiDeleteBindingRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sub := subjectOf(r.Context())
	err := s.access.DeleteBindingRule(r.Context(), id, auditOf(sub))
	switch {
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "this binding rule does not exist")
	case err != nil:
		s.apiServerError(w, r, "delete a binding rule", err)
	default:
		s.log.InfoContext(r.Context(), "binding rule deleted", "binding_rule", id, "actor", sub.actor, "accessor_id", sub.accessorID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// apiListTokens is GET /api/acl/tokens.
func (s *server) apiListTokens(w http.ResponseWriter, r *http.Request) {
	all, err := s.access.ACLTokens(r.Context())
	if err != nil {
		s.apiServerError(w, r, "list tokens", err)
		return
	}
	out := []apiACLToken{}
	for _, t := range all {
		out = append(out, tokenOf(t))
	}
	replyJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

type tokenBody struct {
	Name     string   `json:"name"`
	Type     string   `json:"type,omitempty"`
	Policies []string `json:"policies,omitempty"`
	// ExpiresIn is a Go duration such as "720h"; empty means never.
	ExpiresIn string `json:"expires_in,omitempty"`
}

// apiCreateToken is POST /api/acl/tokens.
func (s *server) apiCreateToken(w http.ResponseWriter, r *http.Request) {
	var b tokenBody
	if !readBody(w, r, &b) {
		return
	}
	in := newToken{Name: b.Name, Type: b.Type, Policies: b.Policies}
	if b.ExpiresIn != "" {
		d, err := time.ParseDuration(b.ExpiresIn)
		if err != nil || d <= 0 {
			apiError(w, http.StatusBadRequest, `"expires_in" is a duration such as "720h", or empty for a token that never expires`)
			return
		}
		in.ExpiresIn = d
	}
	t, sec, err := s.createToken(r.Context(), subjectOf(r.Context()), in)
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		apiError(w, http.StatusBadRequest, bad.Error())
	case err != nil:
		s.apiServerError(w, r, "create a token", err)
	default:
		replyJSON(w, http.StatusCreated, apiNewACLToken{apiACLToken: tokenOf(t), Secret: sec})
	}
}

// apiGetToken is GET /api/acl/tokens/{accessor_id}.
func (s *server) apiGetToken(w http.ResponseWriter, r *http.Request) {
	t, err := s.access.ACLToken(r.Context(), r.PathValue("accessor_id"))
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "this token does not exist")
		return
	}
	if err != nil {
		s.apiServerError(w, r, "read a token", err)
		return
	}
	replyJSON(w, http.StatusOK, tokenOf(t))
}

// apiRevokeToken is DELETE /api/acl/tokens/{accessor_id}.
func (s *server) apiRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("accessor_id")
	sub := subjectOf(r.Context())
	err := s.access.RevokeACLToken(r.Context(), id, auditOf(sub))
	switch {
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "this token does not exist")
	case err != nil:
		s.apiServerError(w, r, "revoke a token", err)
	default:
		s.log.InfoContext(r.Context(), "token revoked", "token", id, "actor", sub.actor, "accessor_id", sub.accessorID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// apiSelfToken is GET /api/acl/token/self: the token of the request. The
// bootstrap token has no row, so it is described here.
func (s *server) apiSelfToken(w http.ResponseWriter, r *http.Request) {
	sub := subjectOf(r.Context())
	if sub.token != nil {
		replyJSON(w, http.StatusOK, tokenOf(*sub.token))
		return
	}
	replyJSON(w, http.StatusOK, apiACLToken{AccessorID: sub.accessorID, Name: sub.actor, Type: store.TokenManagement, Origin: store.OriginCreated, Policies: []string{}})
}

// apiChanges is GET /api/acl/changes: the latest changes to ACL policies and tokens.
func (s *server) apiChanges(w http.ResponseWriter, r *http.Request) {
	list, err := s.access.ACLChanges(r.Context(), changesShown)
	if err != nil {
		s.apiServerError(w, r, "list ACL changes", err)
		return
	}
	out := []apiChange{}
	for _, c := range list {
		out = append(out, apiChange{Time: c.Time, AccessorID: c.AccessorID, Actor: c.Actor, Identity: c.Identity, Action: c.Action, Kind: c.Kind, Object: c.Object})
	}
	replyJSON(w, http.StatusOK, map[string]any{"changes": out})
}
