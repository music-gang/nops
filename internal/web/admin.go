package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// The Administration page (docs/dashboard.md): your token, and with a
// management token every token and ACL policy.

// expiryChoice is one answer to "when does it expire" on the form of a new
// token: a fixed list, so there is nothing to validate beyond membership. A
// zero Days is a token that never expires.
type expiryChoice struct {
	Key, Label string
	Days       int
}

var expiryChoices = []expiryChoice{
	{"7", "7 days", 7},
	{"30", "30 days", 30},
	{"90", "90 days", 90},
	{"365", "365 days", 365},
	{"never", "Never", 0},
}

// defaultExpiry is the choice the form starts on.
const defaultExpiry = "90"

// selfView is the token of the request, or the login when it has none.
type selfView struct {
	Name       string
	Kind       string // "management", "client", or "login" for a session without a token
	AccessorID string
	Policies   []string
	Created    timeView
	Expires    timeView // zero: never
	Creator    string
}

type tokenRow struct {
	selfView
	Expired bool
}

type policyRow struct {
	Name, Description string
	Modified          timeView
}

type changeRow struct {
	When                 timeView
	Actor, AccessorID    string
	Action, Kind, Object string
}

// createdToken is the token just made: the one time its secret is shown.
type createdToken struct {
	Name, Secret string
}

// policyForm is an ACL policy as the form holds it.
type policyForm struct {
	Name, Description, Rules string
	Existing                 bool // editing: the name is fixed
}

type adminData struct {
	baseData
	ACLOn  bool
	Self   selfView
	Manage bool // the token is a management token: the rest of the page

	Tokens   []tokenRow
	Policies []policyRow
	Changes  []changeRow
	Expiry   []expiryChoice
	Default  string
	Created  *createdToken

	// Error and Form come back from a form that was not filled in right.
	Error string
	Form  policyForm
}

type policyPageData struct {
	baseData
	Form  policyForm
	Error string
}

func (s *server) selfOf(sub *subject) selfView {
	v := selfView{Name: sub.actor, Kind: "login"}
	switch {
	case sub.token != nil:
		v = s.tokenView(*sub.token)
	case sub.accessorID != "": // the bootstrap token has no row
		v.Kind, v.AccessorID, v.Creator = store.TokenManagement, sub.accessorID, "the configuration"
	}
	return v
}

func (s *server) tokenView(t store.ACLToken) selfView {
	creator := t.CreatorName
	if creator == "" {
		creator = "—"
	}
	return selfView{
		Name: t.Name, Kind: t.Type, AccessorID: t.AccessorID, Policies: t.Policies,
		Created: s.when(t.CreatedAt), Expires: s.until(t.ExpiresAt), Creator: creator,
	}
}

// adminView builds the page for the subject of the request.
func (s *server) adminView(r *http.Request) (adminData, error) {
	sub := subjectOf(r.Context())
	data := adminData{
		baseData: s.base(r, "admin"), ACLOn: s.aclOn, Self: s.selfOf(sub),
		Manage: sub.acl.Management(),
		Expiry: expiryChoices, Default: defaultExpiry,
	}
	if !data.Manage {
		return data, nil
	}
	ctx := r.Context()
	tokens, err := s.access.ACLTokens(ctx)
	if err != nil {
		return data, err
	}
	now := s.now()
	for _, t := range tokens {
		data.Tokens = append(data.Tokens, tokenRow{selfView: s.tokenView(t), Expired: !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)})
	}
	policies, err := s.access.ACLPolicies(ctx)
	if err != nil {
		return data, err
	}
	for _, p := range policies {
		data.Policies = append(data.Policies, policyRow{Name: p.Name, Description: p.Description, Modified: s.when(p.ModifiedAt)})
	}
	changes, err := s.access.ACLChanges(ctx, changesShown)
	if err != nil {
		return data, err
	}
	for _, c := range changes {
		data.Changes = append(data.Changes, changeRow{When: s.when(c.Time), Actor: c.Actor, AccessorID: c.AccessorID, Action: c.Action, Kind: c.Kind, Object: c.Object})
	}
	return data, nil
}

// adminPage is GET /admin.
func (s *server) adminPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.adminView(r)
	if err != nil {
		s.serverError(w, r, "build the administration page", err)
		return
	}
	s.render(w, r, "admin", data)
}

// policyPage is GET /admin/policies/{name}: edit an ACL policy.
func (s *server) policyPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.access.ACLPolicy(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFoundMessage(w, r, "This ACL policy does not exist.")
		return
	}
	if err != nil {
		s.serverError(w, r, "read an ACL policy", err)
		return
	}
	s.render(w, r, "admin_policy", policyPageData{
		baseData: s.base(r, "admin"),
		Form:     policyForm{Name: p.Name, Description: p.Description, Rules: p.Rules, Existing: true},
	})
}

// savePolicy is POST /admin/policies: create an ACL policy or replace one.
// Rules that do not parse save nothing and come back with the error.
func (s *server) savePolicy(w http.ResponseWriter, r *http.Request) {
	form := policyForm{
		Name:        strings.TrimSpace(r.FormValue("name")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Rules:       r.FormValue("rules"),
		Existing:    r.FormValue("existing") == "true",
	}
	err := s.putPolicy(r.Context(), subjectOf(r.Context()), store.ACLPolicy{Name: form.Name, Description: form.Description, Rules: form.Rules})
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, "admin_policy", policyPageData{baseData: s.base(r, "admin"), Form: form, Error: bad.Error()})
	case err != nil:
		s.serverError(w, r, "save an ACL policy", err)
	default:
		http.Redirect(w, r, s.basePath+"/admin", http.StatusSeeOther)
	}
}

// deletePolicy is POST /admin/policies/{name}/delete.
func (s *server) deletePolicy(w http.ResponseWriter, r *http.Request) {
	sub := subjectOf(r.Context())
	err := s.access.DeleteACLPolicy(r.Context(), r.PathValue("name"), auditOf(sub))
	switch {
	case err == nil:
		s.log.InfoContext(r.Context(), "ACL policy deleted", "acl_policy", r.PathValue("name"), "actor", sub.actor, "accessor_id", sub.accessorID)
		http.Redirect(w, r, s.basePath+"/admin", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This ACL policy does not exist: someone deleted it already.")
	default:
		s.serverError(w, r, "delete an ACL policy", err)
	}
}

// createTokenForm is POST /admin/tokens: make a token and answer with the page
// that shows its secret, the only time it is shown: it is not kept, so it is
// not a redirect that could be repeated.
func (s *server) createTokenForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, r, "Check the form", "The form could not be read.")
		return
	}
	in := newToken{Name: r.PostForm.Get("name"), Type: r.PostForm.Get("type"), Policies: r.PostForm["policy"]}
	choice, found := expiryChoice{}, false
	for _, c := range expiryChoices {
		if c.Key == r.FormValue("expires") {
			choice, found = c, true
		}
	}
	if !found {
		s.badRequest(w, r, "Choose an expiry", "The expiry is one of 7, 30, 90 or 365 days, or never.")
		return
	}
	in.ExpiresIn = time.Duration(choice.Days) * 24 * time.Hour
	t, sec, err := s.createToken(r.Context(), subjectOf(r.Context()), in)
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		s.badRequest(w, r, "Check the token", bad.Error()+".")
		return
	case err != nil:
		s.serverError(w, r, "create a token", err)
		return
	}
	data, err := s.adminView(r)
	if err != nil {
		s.serverError(w, r, "build the administration page", err)
		return
	}
	data.Created = &createdToken{Name: t.Name, Secret: sec}
	s.render(w, r, "admin", data)
}

// revokeTokenForm is POST /admin/tokens/{accessor}/revoke.
func (s *server) revokeTokenForm(w http.ResponseWriter, r *http.Request) {
	sub := subjectOf(r.Context())
	err := s.access.RevokeACLToken(r.Context(), r.PathValue("accessor"), auditOf(sub))
	switch {
	case err == nil:
		s.log.InfoContext(r.Context(), "token revoked", "token", r.PathValue("accessor"), "actor", sub.actor, "accessor_id", sub.accessorID)
		http.Redirect(w, r, s.basePath+"/admin", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This token does not exist: someone revoked it already.")
	default:
		s.serverError(w, r, "revoke a token", err)
	}
}

// badRequest renders the 400 page of a form that was not filled in right.
func (s *server) badRequest(w http.ResponseWriter, r *http.Request, title, message string) {
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "error", errorData{
		baseData: s.base(r, ""),
		Status:   http.StatusBadRequest,
		Title:    title,
		Message:  message,
	})
}
