package web

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// The endpoints under /api/acl/ (docs/api.md): ACL policies, tokens and the
// log of changes to them. A management token uses all of them; a client token
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
	AccessorID string    `json:"accessor_id"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Policies   []string  `json:"policies"`
	CreatedAt  time.Time `json:"created_at"`
	// ExpiresAt is absent for a token that never expires.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// CreatorAccessorID and CreatorName say who made the token: the token used,
	// and who it stands for. They stay after that token is gone.
	CreatorAccessorID string `json:"creator_accessor_id,omitempty"`
	CreatorName       string `json:"creator_name,omitempty"`
}

func tokenOf(t store.ACLToken) apiACLToken {
	return apiACLToken{
		AccessorID: t.AccessorID, Name: t.Name, Type: t.Type, Policies: append([]string{}, t.Policies...),
		CreatedAt: t.CreatedAt, ExpiresAt: timePtr(t.ExpiresAt),
		CreatorAccessorID: t.CreatorAccessorID, CreatorName: t.CreatorName,
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
	replyJSON(w, http.StatusOK, apiACLToken{AccessorID: sub.accessorID, Name: sub.actor, Type: store.TokenManagement, Policies: []string{}})
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
		out = append(out, apiChange{Time: c.Time, AccessorID: c.AccessorID, Actor: c.Actor, Action: c.Action, Kind: c.Kind, Object: c.Object})
	}
	replyJSON(w, http.StatusOK, map[string]any{"changes": out})
}
