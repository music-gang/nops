package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/store"
)

// The Administration page (docs/dashboard.md): your token, and with a
// management token every token, ACL policy and binding rule.

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
	Kind       string // "management", "client", or "login" for a request without a token
	Session    bool   // a session token, which a login made
	AccessorID string
	Policies   []string
	Created    timeView
	Expires    timeView // zero: never
	Creator    string
}

// tokenRow is a token of the list, with whom its links to revoke in one action
// name: the person of a session, or the creator of a created token.
type tokenRow struct {
	selfView
	Identity, CreatorIdentity, CreatorAccessorID string
}

// revokeView is the confirmation a revocation in one action asks for: what it
// names, and the tokens it would revoke.
type revokeView struct {
	store.Revocation
	Tokens []tokenRow
}

// revocationOf reads a revocation in one action from the fields of a query or
// a form.
func revocationOf(v url.Values) store.Revocation {
	return store.Revocation{SessionsOf: v.Get("sessions_of"), CreatorIdentity: v.Get("creator_identity"), CreatorAccessorID: v.Get("creator")}
}

type policyRow struct {
	Name, Description string
	Modified          timeView
}

type ruleRow struct {
	ID, Description, AuthMethod, Selector, Binds string
	Modified                                     timeView
}

// ruleForm is a binding rule as the form holds it. Bind is "management" or
// "policy:<name>".
type ruleForm struct {
	ID, Description, AuthMethod, Selector, Bind string
	Existing                                    bool // editing: the ID is fixed
}

func ruleFormOf(b store.BindingRule) ruleForm {
	f := ruleForm{ID: b.ID, Description: b.Description, AuthMethod: b.AuthMethod, Selector: b.Selector, Bind: b.BindType, Existing: true}
	if b.BindType == acl.BindPolicy {
		f.Bind = "policy:" + b.BindName
	}
	return f
}

// binding turns the form back into a binding rule.
func (f ruleForm) binding() store.BindingRule {
	b := store.BindingRule{ID: f.ID, Description: f.Description, AuthMethod: f.AuthMethod, Selector: f.Selector, BindType: acl.BindManagement}
	if name, ok := strings.CutPrefix(f.Bind, "policy:"); ok {
		b.BindType, b.BindName = acl.BindPolicy, name
	} else if f.Bind != acl.BindManagement {
		b.BindType = f.Bind // not a type: putBindingRule refuses it
	}
	return b
}

type changeRow struct {
	When                 timeView
	Actor, AccessorID    string
	Identity             string
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
	Rules    []ruleRow
	Changes  []changeRow
	Expiry   []expiryChoice
	Default  string
	Created  *createdToken
	Revoke   *revokeView // the tokens a revocation in one action would revoke, to confirm

	// SessionSecret is the secret of the session token of this request, to copy
	// into the command line. Empty for any other token.
	SessionSecret string
	// Sessions says the token list holds the session tokens, hidden by default;
	// HiddenSessions is how many it leaves out.
	Sessions       bool
	HiddenSessions int
	Method         string // the auth method a new binding rule starts with
	NewRule        ruleForm

	// Error and Form come back from a form that was not filled in right.
	Error string
	Form  policyForm
}

type rulePageData struct {
	baseData
	Form     ruleForm
	Error    string
	Policies []policyRow
	Methods  []string
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
		Name: t.Name, Kind: t.Type, Session: t.Origin == store.OriginLogin, AccessorID: t.AccessorID, Policies: t.Policies,
		Created: s.when(t.CreatedAt), Expires: s.until(t.ExpiresAt), Creator: creator,
	}
}

func (s *server) tokenRow(t store.ACLToken) tokenRow {
	return tokenRow{selfView: s.tokenView(t), Identity: t.Identity, CreatorIdentity: t.CreatorIdentity, CreatorAccessorID: t.CreatorAccessorID}
}

// adminView builds the page for the subject of the request. A query that names
// a revocation in one action adds what it would revoke, and revokes nothing.
func (s *server) adminView(r *http.Request) (adminData, error) {
	sub := subjectOf(r.Context())
	data := adminData{
		baseData: s.base(r, "admin"), ACLOn: s.aclOn, Self: s.selfOf(sub),
		Manage: sub.acl.Management(), SessionSecret: sub.sessionSecret,
		Expiry: expiryChoices, Default: defaultExpiry,
		Sessions: r.URL.Query().Get("sessions") == "1", Method: s.auth.Method(),
	}
	data.NewRule = ruleForm{AuthMethod: data.Method, Bind: acl.BindManagement}
	if !data.Manage {
		return data, nil
	}
	ctx := r.Context()
	tokens, err := s.access.ACLTokens(ctx)
	if err != nil {
		return data, err
	}
	for _, t := range tokens {
		if t.Origin == store.OriginLogin && !data.Sessions {
			data.HiddenSessions++
			continue
		}
		data.Tokens = append(data.Tokens, s.tokenRow(t))
	}
	if rev := revocationOf(r.URL.Query()); rev != (store.Revocation{}) {
		if err := checkRevocation(rev); err != nil {
			return data, err
		}
		taken, err := s.access.TokensToRevoke(ctx, rev)
		if err != nil {
			return data, err
		}
		data.Revoke = &revokeView{Revocation: rev}
		for _, t := range taken {
			data.Revoke.Tokens = append(data.Revoke.Tokens, s.tokenRow(t))
		}
	}
	policies, err := s.access.ACLPolicies(ctx)
	if err != nil {
		return data, err
	}
	for _, p := range policies {
		data.Policies = append(data.Policies, policyRow{Name: p.Name, Description: p.Description, Modified: s.when(p.ModifiedAt)})
	}
	rules, err := s.access.BindingRules(ctx)
	if err != nil {
		return data, err
	}
	for _, b := range rules {
		binds := "management"
		if b.BindType == acl.BindPolicy {
			binds = "ACL policy " + b.BindName
		}
		data.Rules = append(data.Rules, ruleRow{ID: b.ID, Description: b.Description, AuthMethod: b.AuthMethod, Selector: b.Selector, Binds: binds, Modified: s.when(b.ModifiedAt)})
	}
	changes, err := s.access.ACLChanges(ctx, changesShown)
	if err != nil {
		return data, err
	}
	for _, c := range changes {
		data.Changes = append(data.Changes, changeRow{When: s.when(c.Time), Actor: c.Actor, AccessorID: c.AccessorID, Identity: c.Identity, Action: c.Action, Kind: c.Kind, Object: c.Object})
	}
	return data, nil
}

// adminPage is GET /admin.
func (s *server) adminPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.adminView(r)
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		s.badRequest(w, r, "Check what to revoke", bad.Error()+".")
	case err != nil:
		s.serverError(w, r, "build the administration page", err)
	default:
		s.render(w, r, "admin", data)
	}
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

// ruleFormPage renders the page of a binding rule.
func (s *server) ruleFormPage(w http.ResponseWriter, r *http.Request, f ruleForm, errMsg string) {
	policies, err := s.access.ACLPolicies(r.Context())
	if err != nil {
		s.serverError(w, r, "list ACL policies", err)
		return
	}
	data := rulePageData{baseData: s.base(r, "admin"), Form: f, Error: errMsg, Methods: authMethods}
	for _, p := range policies {
		data.Policies = append(data.Policies, policyRow{Name: p.Name, Description: p.Description})
	}
	s.render(w, r, "admin_rule", data)
}

// bindingRulePage is GET /admin/binding-rules/{id}: edit a binding rule.
func (s *server) bindingRulePage(w http.ResponseWriter, r *http.Request) {
	b, err := s.access.BindingRule(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFoundMessage(w, r, "This binding rule does not exist.")
		return
	}
	if err != nil {
		s.serverError(w, r, "read a binding rule", err)
		return
	}
	s.ruleFormPage(w, r, ruleFormOf(b), "")
}

// saveBindingRule is POST /admin/binding-rules: create a binding rule or
// replace one. A rule that does not check saves nothing and comes back with the
// error.
func (s *server) saveBindingRule(w http.ResponseWriter, r *http.Request) {
	form := ruleForm{
		ID:          r.FormValue("id"),
		Description: r.FormValue("description"),
		AuthMethod:  r.FormValue("auth_method"),
		Selector:    r.FormValue("selector"),
		Bind:        r.FormValue("bind"),
		Existing:    r.FormValue("id") != "",
	}
	_, err := s.putBindingRule(r.Context(), subjectOf(r.Context()), form.binding())
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		w.WriteHeader(http.StatusBadRequest)
		s.ruleFormPage(w, r, form, bad.Error())
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This binding rule does not exist: someone deleted it.")
	case err != nil:
		s.serverError(w, r, "save a binding rule", err)
	default:
		http.Redirect(w, r, s.basePath+"/admin", http.StatusSeeOther)
	}
}

// deleteBindingRule is POST /admin/binding-rules/{id}/delete.
func (s *server) deleteBindingRule(w http.ResponseWriter, r *http.Request) {
	sub := subjectOf(r.Context())
	err := s.access.DeleteBindingRule(r.Context(), r.PathValue("id"), auditOf(sub))
	switch {
	case err == nil:
		s.log.InfoContext(r.Context(), "binding rule deleted", "binding_rule", r.PathValue("id"), "actor", sub.actor, "accessor_id", sub.accessorID)
		http.Redirect(w, r, s.basePath+"/admin", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This binding rule does not exist: someone deleted it already.")
	default:
		s.serverError(w, r, "delete a binding rule", err)
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

// revokeTokensForm is POST /admin/tokens/revoke: the revocation in one action
// that the administration page showed and someone confirmed.
func (s *server) revokeTokensForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, r, "Check the form", "The form could not be read.")
		return
	}
	_, err := s.revokeTokens(r.Context(), subjectOf(r.Context()), revocationOf(r.PostForm))
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		s.badRequest(w, r, "Check what to revoke", bad.Error()+".")
	case err != nil:
		s.serverError(w, r, "revoke tokens", err)
	default:
		http.Redirect(w, r, s.basePath+"/admin", http.StatusSeeOther)
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
