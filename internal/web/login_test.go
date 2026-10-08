package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/secret"
	"github.com/music-gang/nops/internal/store"
)

// What a login gets, with the ACL on and off (docs/acl.md#binding-rules). These
// tests log in through the fake OpenID Connect provider of auth_test.go, and
// through the local users of auth_basic_test.go, against a whole server.

// policy saves an ACL policy.
func (a *app) policy(name, rules string) {
	a.t.Helper()
	if err := a.store.PutACLPolicy(a.t.Context(), store.ACLPolicy{Name: name, Rules: rules}, store.Audit{Actor: "test"}); err != nil {
		a.t.Fatal(err)
	}
}

// rule saves a binding rule of the OIDC auth method.
func (a *app) rule(selector, bindType, bindName string) store.BindingRule {
	a.t.Helper()
	r, err := a.store.PutBindingRule(a.t.Context(), store.BindingRule{
		AuthMethod: "oidc", Selector: selector, BindType: bindType, BindName: bindName,
	}, store.Audit{Actor: "test"})
	if err != nil {
		a.t.Fatal(err)
	}
	return r
}

// tokenBehind is the session token a cookie carries.
func (a *app) tokenBehind(c *http.Cookie) store.ACLToken {
	a.t.Helper()
	tok, err := a.store.ACLTokenBySecret(a.t.Context(), hashToken(c.Value), a.aclOn)
	if err != nil {
		a.t.Fatalf("the token behind the cookie: %v", err)
	}
	return tok
}

func (a *app) post(target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	a.mux.ServeHTTP(rec, req)
	return rec
}

func olga() grant {
	return grant{claims: idClaims{Sub: "sub-olga", PreferredUsername: "olga", Groups: []string{"ops", "dev"}}}
}

func TestWithTheACLOffEveryLoginIsManagement(t *testing.T) {
	a := newApp(t)
	a.rule(`"ops" in list.groups`, acl.BindPolicy, "nothing-of-the-kind") // rules are not read

	sess := a.loggedIn(grant{claims: idClaims{Sub: "sub-x", PreferredUsername: "xavier"}})
	tok := a.tokenBehind(sess)
	if tok.Type != store.TokenManagement || tok.ACL || tok.Origin != store.OriginLogin || tok.Name != "xavier" {
		t.Errorf("token = %+v, want a management session made with the ACL off", tok)
	}
}

func TestALoginNoRuleMatchesFails(t *testing.T) {
	a := newACLApp(t)
	a.policy("operator", `namespace "*" { policy = "read" }`)

	// No rule at all.
	rec := a.callback("", olga())
	if rec.Code != http.StatusForbidden || cookie(rec, sessionCookie) != nil {
		t.Fatalf("no rule: status %d, session %v, want 403 and no session", rec.Code, cookie(rec, sessionCookie))
	}
	if !strings.Contains(a.logs.String(), "no binding rule matches") {
		t.Errorf("refusal not logged: %s", a.logs.String())
	}

	// A rule that is not about this person.
	a.rule(`"admins" in list.groups`, acl.BindPolicy, "operator")
	rec = a.callback("", olga())
	if rec.Code != http.StatusForbidden || cookie(rec, sessionCookie) != nil {
		t.Errorf("a rule that does not match: status %d, want 403 and no session", rec.Code)
	}

	tokens, _ := a.store.ACLTokens(t.Context())
	if len(tokens) != 0 {
		t.Errorf("%d tokens made by refused logins", len(tokens))
	}
}

func TestBindingRulesDecideWhatALoginGets(t *testing.T) {
	a := newACLApp(t)
	a.policy("operator", `namespace "*" { policy = "read" }`)
	a.policy("personal", `namespace "default" { capabilities = ["read", "pause"] }`)
	a.rule(`"ops" in list.groups`, acl.BindPolicy, "operator")
	a.rule(`"dev" in list.groups`, acl.BindPolicy, "operator") // the same ACL policy twice is carried once
	a.rule(`value.sub == "sub-olga"`, acl.BindPolicy, "personal")
	a.rule(`value.username == "somebody-else"`, acl.BindManagement, "")

	sess := a.loggedIn(olga())
	tok := a.tokenBehind(sess)
	if tok.Type != store.TokenClient || !slices.Equal(tok.Policies, []string{"operator", "personal"}) {
		t.Errorf("token = %+v, want a client token with operator and personal, once each", tok)
	}
	if tok.Origin != store.OriginLogin || tok.Name != "olga" || tok.Identity != "oidc:"+a.idp.srv.URL+"#sub-olga" {
		t.Errorf("token = %+v, want a session of olga with the issuer and sub as identity", tok)
	}
	if !tok.ExpiresAt.Equal(a.clock.now().Add(sessionTTL)) || tok.CreatorName != "olga" || tok.CreatorIdentity != tok.Identity {
		t.Errorf("token = %+v, want it to last %v and name the person as its creator", tok, sessionTTL)
	}
	if !tok.ACL {
		t.Error("the token was not made with the ACL on")
	}

	// Management wins, and adds nothing to a client token it replaces.
	a.rule(`value.sub == "sub-olga"`, acl.BindManagement, "")
	tok = a.tokenBehind(a.loggedIn(olga()))
	if tok.Type != store.TokenManagement || len(tok.Policies) != 0 {
		t.Errorf("token = %+v, want a management token", tok)
	}
}

func TestGroupsFromUserinfoReachTheRules(t *testing.T) {
	a := newACLApp(t)
	a.policy("operator", `namespace "*" { policy = "read" }`)
	a.rule(`"ops" in list.groups`, acl.BindPolicy, "operator")

	g := olga()
	g.inUserinfo = true // Authelia keeps the groups out of the ID token
	tok := a.tokenBehind(a.loggedIn(g))
	if !slices.Equal(tok.Policies, []string{"operator"}) {
		t.Errorf("policies = %v, want operator", tok.Policies)
	}
}

func TestABrokenBindingRuleBindsNothingAndOthersApply(t *testing.T) {
	a := newACLApp(t)
	a.policy("operator", `namespace "*" { policy = "read" }`)
	// A selector that cannot run is refused when it is saved over the API; this one
	// was written some other way.
	if _, err := a.store.PutBindingRule(t.Context(), store.BindingRule{
		AuthMethod: "oidc", Selector: `value.nope == "x"`, BindType: acl.BindManagement,
	}, store.Audit{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	a.rule("", acl.BindPolicy, "operator")

	tok := a.tokenBehind(a.loggedIn(olga()))
	if tok.Type != store.TokenClient || !slices.Equal(tok.Policies, []string{"operator"}) {
		t.Errorf("token = %+v, want only what the sound rule binds", tok)
	}
	if !strings.Contains(a.logs.String(), "cannot be evaluated") {
		t.Errorf("the broken rule is not in the log: %s", a.logs.String())
	}
}

// The invariant of docs/philosophy.md: no login without "approve" on the job's
// namespace moves a deployment past pending_approval, however the binding rules
// are written.
func TestALoginWithoutApproveCannotApprove(t *testing.T) {
	a := newACLApp(t)
	a.policy("readers", `namespace "*" { policy = "read" }`)
	a.policy("approvers", `namespace "default" { capabilities = ["read", "approve"] }`)
	a.rule(`"ops" in list.groups`, acl.BindPolicy, "readers")
	a.rule(`"approvers" in list.groups`, acl.BindPolicy, "approvers")

	reader := a.loggedIn(olga())
	form := url.Values{"spec_hash": {"spec-hash-1"}}
	for _, target := range []string{"/deployments/d1/approve", "/deployments/d1/reject"} {
		if rec := a.post(target, form, reader); rec.Code != http.StatusForbidden {
			t.Errorf("%s as a reader: status %d, want 403", target, rec.Code)
		}
	}
	if len(a.engine.approveCalls)+len(a.engine.rejectCalls) != 0 {
		t.Fatalf("the engine was called: %v %v", a.engine.approveCalls, a.engine.rejectCalls)
	}

	g := idClaims{Sub: "sub-ann", PreferredUsername: "ann", Groups: []string{"approvers"}}
	approver := a.loggedIn(grant{claims: g})
	if rec := a.post("/deployments/d1/approve", form, approver); rec.Code != http.StatusSeeOther {
		t.Fatalf("approve as an approver: status %d, body %s", rec.Code, rec.Body)
	}
	tok := a.tokenBehind(approver)
	if len(a.engine.approveCalls) != 1 || a.engine.approveCalls[0].actor != "ann" ||
		a.engine.accessor != tok.AccessorID || a.engine.identity != tok.Identity {
		t.Errorf("approve calls %+v, accessor %q, identity %q: want ann, with the session's accessor ID and identity",
			a.engine.approveCalls, a.engine.accessor, a.engine.identity)
	}
}

func TestAChangedBindingRuleAppliesFromTheNextLogin(t *testing.T) {
	a := newACLApp(t)
	a.policy("readers", `namespace "*" { policy = "read" }`)
	a.policy("writers", `namespace "*" { policy = "write" }`)
	r := a.rule(`"ops" in list.groups`, acl.BindPolicy, "readers")

	first := a.loggedIn(olga())
	r.BindName = "writers"
	if _, err := a.store.PutBindingRule(t.Context(), r, store.Audit{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := a.tokenBehind(first).Policies; !slices.Equal(got, []string{"readers"}) {
		t.Errorf("the session already open: policies %v, want it to keep readers", got)
	}
	if got := a.tokenBehind(a.loggedIn(olga())).Policies; !slices.Equal(got, []string{"writers"}) {
		t.Errorf("the next login: policies %v, want writers", got)
	}
}

func TestSessionsExpireAndTheNextLoginDeletesThem(t *testing.T) {
	a := newApp(t)
	old := a.loggedIn(alice())
	tok := a.tokenBehind(old)

	a.clock.advance(sessionTTL + time.Minute)
	if rec := a.do("GET", "/page", []*http.Cookie{old}); rec.Code != http.StatusFound {
		t.Errorf("an expired session: status %d, want the login redirect", rec.Code)
	}
	if _, err := a.store.ACLToken(t.Context(), tok.AccessorID); err != nil {
		t.Fatalf("the expired session is gone before a login: %v", err)
	}
	a.loggedIn(alice())
	if _, err := a.store.ACLToken(t.Context(), tok.AccessorID); err == nil {
		t.Error("the expired session is still there after a login")
	}
}

func TestLoginAndLogoutAreInTheChangesOfTheACL(t *testing.T) {
	a := newApp(t)
	sess := a.loggedIn(alice())
	tok := a.tokenBehind(sess)
	a.do("POST", "/auth/logout", []*http.Cookie{sess})

	changes, err := a.store.ACLChanges(t.Context(), 10)
	if err != nil || len(changes) != 2 {
		t.Fatalf("changes = %+v, %v, want two", changes, err)
	}
	logout, login := changes[0], changes[1]
	if login.Action != "create" || login.Kind != "token" || login.Object != tok.AccessorID || login.Actor != "alice" || login.Identity != tok.Identity {
		t.Errorf("login change = %+v", login)
	}
	if logout.Action != "revoke" || logout.Object != tok.AccessorID || logout.AccessorID != tok.AccessorID || logout.Identity != tok.Identity {
		t.Errorf("logout change = %+v", logout)
	}
}

func TestLoginPage(t *testing.T) {
	a := newApp(t)
	rec := a.do("GET", "/auth/login?next="+url.QueryEscape("/jobs?state=drift"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), `href="/auth/oidc?next=%2Fjobs%3Fstate%3Ddrift"`, `action="/auth/token"`, `name="token"`)
	mustNotContain(t, rec.Body.String(), `name="username"`)

	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	rec = ba.do("GET", "/auth/login", nil, nil)
	mustContain(t, rec.Body.String(), `action="/auth/basic"`, `name="username"`, `action="/auth/token"`)
	mustNotContain(t, rec.Body.String(), "/auth/oidc")
}

func TestSessionCookieIsSecureOnAnHTTPSPublicURL(t *testing.T) {
	a := newApp(t, func(o *AuthOptions) { o.RedirectURL = "https://nops.example.com/auth/callback" })
	// The fake provider only accepts the redirect URL of the fake app, so the
	// server is asked for the cookie of a pasted token, which needs no round trip.
	boot := secret.New()
	if _, err := a.store.CreateACLToken(t.Context(), store.ACLToken{Name: "t", Type: store.TokenManagement}, hashToken(boot), store.Audit{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	rec := a.post("/auth/token", url.Values{"token": {boot}})
	if c := cookie(rec, tokenCookie); c == nil || !c.Secure || !c.HttpOnly {
		t.Errorf("token cookie = %+v, want Secure and HttpOnly", c)
	}
}

// -- a pasted token -----------------------------------------------------------

func TestAPastedTokenLogsIn(t *testing.T) {
	a := newACLApp(t)

	rec := a.post("/auth/token", url.Values{"token": {a.boot}, "next": {"/page"}})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/page" {
		t.Fatalf("status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	pasted := cookie(rec, tokenCookie)
	if pasted == nil || pasted.MaxAge != 0 || !pasted.HttpOnly || pasted.SameSite != http.SameSiteLaxMode {
		t.Fatalf("token cookie = %+v, want one the browser forgets when it closes", pasted)
	}
	if rec := a.do("GET", "/page", []*http.Cookie{pasted}); rec.Body.String() != "user=bootstrap" {
		t.Errorf("page = %q, want the bootstrap token", rec.Body.String())
	}
	// The whole Administration page is open to it: it is a management token.
	admin := a.do("GET", "/admin", []*http.Cookie{pasted})
	mustContain(t, admin.Body.String(), "New binding rule", "New ACL policy")
	mustNotContain(t, admin.Body.String(), "session-secret")

	// Logging out forgets it, and revokes nothing: it may be the bootstrap token.
	out := a.do("POST", "/auth/logout", []*http.Cookie{pasted})
	if out.Code != http.StatusSeeOther || !deleted(out, tokenCookie) {
		t.Errorf("logout: status %d, cookie deleted %v", out.Code, deleted(out, tokenCookie))
	}
	if rec := a.post("/auth/token", url.Values{"token": {a.boot}}); rec.Code != http.StatusFound {
		t.Errorf("the token after the logout: status %d, want it still valid", rec.Code)
	}
}

func TestAPastedCreatedTokenSurvivesTheLogout(t *testing.T) {
	a := newACLApp(t)
	sec := secret.New()
	tok, err := a.store.CreateACLToken(t.Context(), store.ACLToken{Name: "ci", Type: store.TokenManagement, ACL: true}, hashToken(sec), store.Audit{Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	pasted := cookie(a.post("/auth/token", url.Values{"token": {" " + sec + "\n"}}), tokenCookie)
	if pasted == nil {
		t.Fatal("a token with blanks around it was refused")
	}
	a.do("POST", "/auth/logout", []*http.Cookie{pasted})
	if _, err := a.store.ACLToken(t.Context(), tok.AccessorID); err != nil {
		t.Errorf("the token after the logout: %v, want it kept", err)
	}
}

func TestAPastedTokenThatIsNotValidIsRefused(t *testing.T) {
	a := newACLApp(t)
	for name, token := range map[string]string{
		"unknown": secret.New(),
		"empty":   "",
	} {
		rec := a.post("/auth/token", url.Values{"token": {token}})
		if rec.Code != http.StatusUnauthorized || cookie(rec, tokenCookie) != nil {
			t.Errorf("%s: status %d, cookie %v, want 401 and no cookie", name, rec.Code, cookie(rec, tokenCookie))
		}
		mustContain(t, rec.Body.String(), "That token is not valid", `name="token"`)
	}

	// A token made with the ACL off does not work once it is on.
	off := secret.New()
	if _, err := a.store.CreateACLToken(t.Context(), store.ACLToken{Name: "old", Type: store.TokenManagement, ACL: false}, hashToken(off), store.Audit{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if rec := a.post("/auth/token", url.Values{"token": {off}}); rec.Code != http.StatusUnauthorized {
		t.Errorf("a token of the other mode: status %d, want 401", rec.Code)
	}
}

func TestAPastedTokenIsRefusedCrossOrigin(t *testing.T) {
	a := newACLApp(t)
	req := httptest.NewRequest("POST", "/auth/token", strings.NewReader(url.Values{"token": {a.boot}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	a.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || cookie(rec, tokenCookie) != nil {
		t.Errorf("status %d, cookie %v, want 403 and no cookie", rec.Code, cookie(rec, tokenCookie))
	}
}

// -- the session token on the Administration page -----------------------------

func TestTheAdministrationPageShowsTheSecretOfASessionToken(t *testing.T) {
	a := newApp(t)
	sess := a.loggedIn(alice())

	page := a.do("GET", "/admin", []*http.Cookie{sess}).Body.String()
	mustContain(t, page, `value="`+sess.Value+`"`, `data-copy="session-secret"`, "Session started")

	// The same secret pasted into another browser is a pasted token: it works,
	// and the page never shows it.
	pasted := &http.Cookie{Name: tokenCookie, Value: sess.Value}
	if rec := a.do("GET", "/page", []*http.Cookie{pasted}); rec.Body.String() != "user=alice" {
		t.Fatalf("a pasted session token: %q", rec.Body.String())
	}
	page = a.do("GET", "/admin", []*http.Cookie{pasted}).Body.String()
	mustNotContain(t, page, sess.Value, "session-secret")
}

func TestSessionsAreHiddenInTheTokenListByDefault(t *testing.T) {
	a := newACLApp(t)
	a.rule("", acl.BindManagement, "")
	sess := a.loggedIn(alice())

	page := a.do("GET", "/admin", []*http.Cookie{sess}).Body.String()
	mustContain(t, page, "1 session token hidden", `href="/admin?sessions=1"`)
	page = a.do("GET", "/admin?sessions=1", []*http.Cookie{sess}).Body.String()
	mustContain(t, page, "Showing the session tokens", "login")
	mustNotContain(t, page, "session token hidden")
}

// -- local users --------------------------------------------------------------

func TestBasicLoginsGetWhatTheRulesBind(t *testing.T) {
	ba := newBasicAppWith(t, "alice:"+bcryptHash(t, "s3cret")+"\nbob:"+bcryptHash(t, "s3cret")+"\n", "", true)
	if err := ba.store.PutACLPolicy(t.Context(), store.ACLPolicy{Name: "readers", Rules: `namespace "*" { policy = "read" }`}, store.Audit{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ba.store.PutBindingRule(t.Context(), store.BindingRule{
		AuthMethod: "basic", Selector: `value.username == "alice"`, BindType: acl.BindPolicy, BindName: "readers",
	}, store.Audit{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	// A rule for the other auth method does not apply to a local user.
	if _, err := ba.store.PutBindingRule(t.Context(), store.BindingRule{AuthMethod: "oidc", BindType: acl.BindManagement}, store.Audit{Actor: "test"}); err != nil {
		t.Fatal(err)
	}

	rec := ba.login("alice", "s3cret")
	sess := cookie(rec, sessionCookie)
	if rec.Code != http.StatusFound || sess == nil {
		t.Fatalf("alice: status %d, session %v", rec.Code, sess)
	}
	tok, err := ba.store.ACLTokenBySecret(t.Context(), hashToken(sess.Value), true)
	if err != nil || tok.Type != store.TokenClient || !slices.Equal(tok.Policies, []string{"readers"}) || tok.Identity != "basic:alice" {
		t.Errorf("token = %+v, %v, want a client token with readers, for basic:alice", tok, err)
	}

	rec = ba.login("bob", "s3cret")
	if rec.Code != http.StatusForbidden || cookie(rec, sessionCookie) != nil {
		t.Errorf("bob: status %d, session %v, want 403 and no session", rec.Code, cookie(rec, sessionCookie))
	}
}

func TestBasicGroupsReachTheRules(t *testing.T) {
	hash := bcryptHash(t, "s3cret")
	ba := newBasicAppFrom(t, BasicAuthOptions{
		UsersFile:  writeUsersFile(t, "alice:"+hash+"\nbob:"+hash+"\n"),
		GroupsFile: writeGroupsFile(t, "ops: alice\n"),
	}, "", true)
	if err := ba.store.PutACLPolicy(t.Context(), store.ACLPolicy{Name: "readers", Rules: `namespace "*" { policy = "read" }`}, store.Audit{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ba.store.PutBindingRule(t.Context(), store.BindingRule{
		AuthMethod: "basic", Selector: `"ops" in list.groups`, BindType: acl.BindPolicy, BindName: "readers",
	}, store.Audit{Actor: "test"}); err != nil {
		t.Fatal(err)
	}

	rec := ba.login("alice", "s3cret")
	sess := cookie(rec, sessionCookie)
	if rec.Code != http.StatusFound || sess == nil {
		t.Fatalf("alice: status %d, session %v", rec.Code, sess)
	}
	tok, err := ba.store.ACLTokenBySecret(t.Context(), hashToken(sess.Value), true)
	if err != nil || !slices.Equal(tok.Policies, []string{"readers"}) {
		t.Errorf("token = %+v, %v, want one with readers", tok, err)
	}

	// bob is a user, but in no group.
	rec = ba.login("bob", "s3cret")
	if rec.Code != http.StatusForbidden || cookie(rec, sessionCookie) != nil {
		t.Errorf("bob: status %d, session %v, want 403 and no session", rec.Code, cookie(rec, sessionCookie))
	}
}
