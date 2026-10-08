package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/store"
)

// The Administration pages (docs/dashboard.md): your token, and with a
// management token a page each for tokens, ACL policies, binding rules and the
// latest changes to them. Revoking and deleting go through a page that shows
// what goes, so a click on a list never changes anything.

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

// revokeView is the confirmation a revocation asks for: what it names, one
// token by its accessor ID or a revocation in one action, and the tokens it
// would revoke.
type revokeView struct {
	store.Revocation
	Accessor string
	Tokens   []tokenRow
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

// tokenForm is a new token as the form holds it.
type tokenForm struct {
	Name, Type, Expires string
}

// policyChoice is an ACL policy a new token can carry, ticked or not.
type policyChoice struct {
	policyRow
	Checked bool
}

// adminBase is what every Administration page carries: the menu at its side
// marks Section and, with a management token, lists the other pages.
type adminBase struct {
	baseData
	ACLOn   bool
	Manage  bool
	Section string // "self", "tokens", "policies", "rules" or "changes"
}

type adminSelfData struct {
	adminBase
	Self selfView
	// SessionSecret is the secret of the session token of this request, to copy
	// into the command line. Empty for any other token.
	SessionSecret string
}

type adminTokensData struct {
	adminBase
	Tokens []tokenRow
	// Sessions says the list holds the session tokens, not the created ones.
	Sessions            bool
	CreatedN, SessionsN int
	Revoke              *revokeView // the tokens a revocation would revoke, to confirm
	Created             *createdToken
}

type tokenFormData struct {
	adminBase
	Form     tokenForm
	Policies []policyChoice
	Expiry   []expiryChoice
	Error    string
}

type adminPoliciesData struct {
	adminBase
	Policies []policyRow
}

type policyPageData struct {
	adminBase
	Form   policyForm
	Error  string
	Delete bool // the deletion waits for a confirmation
}

type adminRulesData struct {
	adminBase
	Rules []ruleRow
}

type rulePageData struct {
	adminBase
	Form     ruleForm
	Error    string
	Delete   bool // the deletion waits for a confirmation
	Policies []policyRow
	Methods  []string
}

type adminChangesData struct {
	adminBase
	Changes []changeRow
}

func (s *server) adminBase(r *http.Request, section string) adminBase {
	return adminBase{baseData: s.base(r, "admin"), ACLOn: s.aclOn, Manage: subjectOf(r.Context()).acl.Management(), Section: section}
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

func (s *server) policyRows(ctx context.Context) ([]policyRow, error) {
	policies, err := s.access.ACLPolicies(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]policyRow, 0, len(policies))
	for _, p := range policies {
		rows = append(rows, policyRow{Name: p.Name, Description: p.Description, Modified: s.when(p.ModifiedAt)})
	}
	return rows, nil
}

// adminPage is GET /admin: the token of the request.
func (s *server) adminPage(w http.ResponseWriter, r *http.Request) {
	sub := subjectOf(r.Context())
	s.render(w, r, "admin", adminSelfData{adminBase: s.adminBase(r, "self"), Self: s.selfOf(sub), SessionSecret: sub.sessionSecret})
}

// tokensView builds the list of tokens. A query that names a token
// (revoke=<accessor>) or a revocation in one action adds what it would
// revoke, and revokes nothing.
func (s *server) tokensView(r *http.Request) (adminTokensData, error) {
	ctx, q := r.Context(), r.URL.Query()
	data := adminTokensData{adminBase: s.adminBase(r, "tokens"), Sessions: q.Get("sessions") == "1"}
	tokens, err := s.access.ACLTokens(ctx)
	if err != nil {
		return data, err
	}
	for _, t := range tokens {
		session := t.Origin == store.OriginLogin
		if session {
			data.SessionsN++
		} else {
			data.CreatedN++
		}
		if session == data.Sessions {
			data.Tokens = append(data.Tokens, s.tokenRow(t))
		}
	}
	rev, accessor := revocationOf(q), q.Get("revoke")
	switch {
	case accessor != "" && rev != (store.Revocation{}):
		return data, invalidf("name one token, or one person or creator")
	case accessor != "":
		i := slices.IndexFunc(tokens, func(t store.ACLToken) bool { return t.AccessorID == accessor })
		if i < 0 {
			return data, fmt.Errorf("token %s: %w", accessor, store.ErrNotFound)
		}
		data.Revoke = &revokeView{Accessor: accessor, Tokens: []tokenRow{s.tokenRow(tokens[i])}}
	case rev != (store.Revocation{}):
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
	return data, nil
}

// tokensPage is GET /admin/tokens.
func (s *server) tokensPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.tokensView(r)
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		s.badRequest(w, r, "Check what to revoke", bad.Error()+".")
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This token does not exist: someone revoked it already.")
	case err != nil:
		s.serverError(w, r, "list the tokens", err)
	default:
		s.render(w, r, "admin_tokens", data)
	}
}

// tokenFormPage renders the form of a new token, with what it holds ticked.
func (s *server) tokenFormPage(w http.ResponseWriter, r *http.Request, f tokenForm, policies []string, errMsg string) {
	rows, err := s.policyRows(r.Context())
	if err != nil {
		s.serverError(w, r, "list ACL policies", err)
		return
	}
	data := tokenFormData{adminBase: s.adminBase(r, "tokens"), Form: f, Expiry: expiryChoices, Error: errMsg}
	for _, p := range rows {
		data.Policies = append(data.Policies, policyChoice{policyRow: p, Checked: slices.Contains(policies, p.Name)})
	}
	s.render(w, r, "admin_token_new", data)
}

// newTokenPage is GET /admin/tokens/new.
func (s *server) newTokenPage(w http.ResponseWriter, r *http.Request) {
	s.tokenFormPage(w, r, tokenForm{Type: store.TokenClient, Expires: defaultExpiry}, nil, "")
}

// policiesPage is GET /admin/policies.
func (s *server) policiesPage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.policyRows(r.Context())
	if err != nil {
		s.serverError(w, r, "list ACL policies", err)
		return
	}
	s.render(w, r, "admin_policies", adminPoliciesData{adminBase: s.adminBase(r, "policies"), Policies: rows})
}

// rulesPage is GET /admin/binding-rules.
func (s *server) rulesPage(w http.ResponseWriter, r *http.Request) {
	rules, err := s.access.BindingRules(r.Context())
	if err != nil {
		s.serverError(w, r, "list binding rules", err)
		return
	}
	data := adminRulesData{adminBase: s.adminBase(r, "rules")}
	for _, b := range rules {
		binds := "management"
		if b.BindType == acl.BindPolicy {
			binds = "ACL policy " + b.BindName
		}
		data.Rules = append(data.Rules, ruleRow{ID: b.ID, Description: b.Description, AuthMethod: b.AuthMethod, Selector: b.Selector, Binds: binds, Modified: s.when(b.ModifiedAt)})
	}
	s.render(w, r, "admin_rules", data)
}

// changesPage is GET /admin/changes.
func (s *server) changesPage(w http.ResponseWriter, r *http.Request) {
	changes, err := s.access.ACLChanges(r.Context(), changesShown)
	if err != nil {
		s.serverError(w, r, "list the changes", err)
		return
	}
	data := adminChangesData{adminBase: s.adminBase(r, "changes")}
	for _, c := range changes {
		data.Changes = append(data.Changes, changeRow{When: s.when(c.Time), Actor: c.Actor, AccessorID: c.AccessorID, Identity: c.Identity, Action: c.Action, Kind: c.Kind, Object: c.Object})
	}
	s.render(w, r, "admin_changes", data)
}

// newPolicyPage is GET /admin/policies/new.
func (s *server) newPolicyPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "admin_policy", policyPageData{adminBase: s.adminBase(r, "policies")})
}

// policyPage is GET /admin/policies/{name}/edit: edit an ACL policy, and with
// delete=1 confirm its deletion.
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
		adminBase: s.adminBase(r, "policies"),
		Form:      policyForm{Name: p.Name, Description: p.Description, Rules: p.Rules, Existing: true},
		Delete:    r.URL.Query().Get("delete") == "1",
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
		s.render(w, r, "admin_policy", policyPageData{adminBase: s.adminBase(r, "policies"), Form: form, Error: bad.Error()})
	case err != nil:
		s.serverError(w, r, "save an ACL policy", err)
	default:
		http.Redirect(w, r, s.basePath+"/admin/policies", http.StatusSeeOther)
	}
}

// deletePolicy is POST /admin/policies/{name}/delete.
func (s *server) deletePolicy(w http.ResponseWriter, r *http.Request) {
	sub := subjectOf(r.Context())
	err := s.access.DeleteACLPolicy(r.Context(), r.PathValue("name"), auditOf(sub))
	switch {
	case err == nil:
		s.log.InfoContext(r.Context(), "ACL policy deleted", "acl_policy", r.PathValue("name"), "actor", sub.actor, "accessor_id", sub.accessorID)
		http.Redirect(w, r, s.basePath+"/admin/policies", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This ACL policy does not exist: someone deleted it already.")
	default:
		s.serverError(w, r, "delete an ACL policy", err)
	}
}

// ruleFormPage renders the page of a binding rule.
func (s *server) ruleFormPage(w http.ResponseWriter, r *http.Request, f ruleForm, errMsg string) {
	rows, err := s.policyRows(r.Context())
	if err != nil {
		s.serverError(w, r, "list ACL policies", err)
		return
	}
	s.render(w, r, "admin_rule", rulePageData{
		adminBase: s.adminBase(r, "rules"), Form: f, Error: errMsg, Policies: rows, Methods: authMethods,
		Delete: f.Existing && r.Method == http.MethodGet && r.URL.Query().Get("delete") == "1",
	})
}

// newRulePage is GET /admin/binding-rules/new.
func (s *server) newRulePage(w http.ResponseWriter, r *http.Request) {
	s.ruleFormPage(w, r, ruleForm{AuthMethod: s.auth.Method(), Bind: acl.BindManagement}, "")
}

// bindingRulePage is GET /admin/binding-rules/{id}/edit: edit a binding rule,
// and with delete=1 confirm its deletion.
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
		http.Redirect(w, r, s.basePath+"/admin/binding-rules", http.StatusSeeOther)
	}
}

// deleteBindingRule is POST /admin/binding-rules/{id}/delete.
func (s *server) deleteBindingRule(w http.ResponseWriter, r *http.Request) {
	sub := subjectOf(r.Context())
	err := s.access.DeleteBindingRule(r.Context(), r.PathValue("id"), auditOf(sub))
	switch {
	case err == nil:
		s.log.InfoContext(r.Context(), "binding rule deleted", "binding_rule", r.PathValue("id"), "actor", sub.actor, "accessor_id", sub.accessorID)
		http.Redirect(w, r, s.basePath+"/admin/binding-rules", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This binding rule does not exist: someone deleted it already.")
	default:
		s.serverError(w, r, "delete a binding rule", err)
	}
}

// createTokenForm is POST /admin/tokens: make a token and answer with the page
// that shows its secret, the only time it is shown: it is not kept, so it is
// not a redirect that could be repeated. A form not filled in right comes back
// with the error and what was typed.
func (s *server) createTokenForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.badRequest(w, r, "Check the form", "The form could not be read.")
		return
	}
	form := tokenForm{Name: r.PostForm.Get("name"), Type: r.PostForm.Get("type"), Expires: r.PostForm.Get("expires")}
	policies := r.PostForm["policy"]
	i := slices.IndexFunc(expiryChoices, func(c expiryChoice) bool { return c.Key == form.Expires })
	if i < 0 {
		w.WriteHeader(http.StatusBadRequest)
		s.tokenFormPage(w, r, form, policies, "The expiry is one of 7, 30, 90 or 365 days, or never.")
		return
	}
	in := newToken{Name: form.Name, Type: form.Type, Policies: policies, ExpiresIn: time.Duration(expiryChoices[i].Days) * 24 * time.Hour}
	t, sec, err := s.createToken(r.Context(), subjectOf(r.Context()), in)
	var bad invalidInput
	switch {
	case errors.As(err, &bad):
		w.WriteHeader(http.StatusBadRequest)
		s.tokenFormPage(w, r, form, policies, upperFirst(bad.Error())+".")
		return
	case err != nil:
		s.serverError(w, r, "create a token", err)
		return
	}
	data, err := s.tokensView(r)
	if err != nil {
		s.serverError(w, r, "list the tokens", err)
		return
	}
	data.Created = &createdToken{Name: t.Name, Secret: sec}
	s.render(w, r, "admin_tokens", data)
}

// revokeTokenForm is POST /admin/tokens/{accessor}/revoke: the revocation of one
// token that the list of tokens showed and someone confirmed.
func (s *server) revokeTokenForm(w http.ResponseWriter, r *http.Request) {
	sub := subjectOf(r.Context())
	err := s.access.RevokeACLToken(r.Context(), r.PathValue("accessor"), auditOf(sub))
	switch {
	case err == nil:
		s.log.InfoContext(r.Context(), "token revoked", "token", r.PathValue("accessor"), "actor", sub.actor, "accessor_id", sub.accessorID)
		http.Redirect(w, r, s.basePath+"/admin/tokens", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This token does not exist: someone revoked it already.")
	default:
		s.serverError(w, r, "revoke a token", err)
	}
}

// revokeTokensForm is POST /admin/tokens/revoke: the revocation in one action
// that the list of tokens showed and someone confirmed.
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
		http.Redirect(w, r, s.basePath+"/admin/tokens", http.StatusSeeOther)
	}
}

// upperFirst starts a message with a capital, for one that stands alone.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
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
