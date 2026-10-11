package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/store"
)

// The login of the command line (cli_login.go): the basic users it asks for
// in the terminal, and the OIDC one that goes through the browser.

// cliClient is what the command line holds across an OIDC login: a verifier
// only it knows, and the state it checks on the way back.
type cliClient struct {
	port     string
	state    string
	verifier string
}

func newCLIClient() cliClient {
	return cliClient{port: "43210", state: randomToken(), verifier: oauth2.GenerateVerifier()}
}

func (c cliClient) startURL() string {
	return "/auth/oidc?" + url.Values{
		"cli_port":      {c.port},
		"cli_state":     {c.state},
		"cli_challenge": {oauth2.S256ChallengeFromVerifier(c.verifier)},
	}.Encode()
}

// cliCallback logs g in through the provider on behalf of the client, and
// returns where the browser is sent back.
func (a *app) cliCallback(c cliClient, g grant) *httptest.ResponseRecorder {
	a.t.Helper()
	rec := a.do("GET", c.startURL(), nil)
	if rec.Code != http.StatusFound {
		a.t.Fatalf("start: status %d, body %q", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		a.t.Fatal(err)
	}
	code := a.idp.authorize(loc, g)
	return a.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(loc.Query().Get("state")), []*http.Cookie{cookie(rec, loginCookie)})
}

// cliBack is where a redirect sends the browser: it must be the client's port on the loopback.
func cliBack(t *testing.T, rec *httptest.ResponseRecorder, port string) url.Values {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, body %q, want 303 back to the command line", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Scheme != "http" || loc.Host != "127.0.0.1:"+port || loc.Path != "/callback" {
		t.Fatalf("Location = %s, want http://127.0.0.1:%s/callback", loc, port)
	}
	return loc.Query()
}

func (a *app) exchange(code, verifier string) *httptest.ResponseRecorder {
	a.t.Helper()
	body, _ := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	req := httptest.NewRequest("POST", "/auth/cli/token", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	a.mux.ServeHTTP(rec, req)
	return rec
}

func sessionOf(t *testing.T, rec *httptest.ResponseRecorder) cliSession {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	var s cliSession
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Secret == "" {
		t.Fatal("no secret in the answer")
	}
	return s
}

func bearer(secret string) string { return "Bearer " + secret }

func TestCLILoginThroughTheBrowser(t *testing.T) {
	a := newApp(t)
	c := newCLIClient()

	rec := a.cliCallback(c, alice())
	back := cliBack(t, rec, c.port)
	if back.Get("state") != c.state || back.Get("error") != "" {
		t.Fatalf("back = %v, want the client's state and no error", back)
	}
	if cookie(rec, sessionCookie) != nil || strings.Contains(rec.Header().Get("Location"), "nops_") {
		t.Error("the browser got a session or a secret: only the client may")
	}

	sess := sessionOf(t, a.exchange(back.Get("code"), c.verifier))
	self := a.do("GET", "/api/acl/token/self", nil, "Authorization", bearer(sess.Secret))
	if self.Code != http.StatusOK || !strings.Contains(self.Body.String(), `"origin":"login"`) || !strings.Contains(self.Body.String(), `"name":"alice"`) {
		t.Errorf("self: status %d, body %s, want alice's login session", self.Code, self.Body)
	}
	if want := a.clock.now().Add(sessionTTL); !sess.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", sess.ExpiresAt, want)
	}

	// A code works once.
	if rec := a.exchange(back.Get("code"), c.verifier); rec.Code != http.StatusBadRequest {
		t.Errorf("second exchange: status %d, want 400", rec.Code)
	}
}

func TestCLILoginCodeNeedsItsVerifier(t *testing.T) {
	a := newApp(t)
	c := newCLIClient()
	code := cliBack(t, a.cliCallback(c, alice()), c.port).Get("code")

	if rec := a.exchange(code, oauth2.GenerateVerifier()); rec.Code != http.StatusBadRequest {
		t.Fatalf("another verifier: status %d, want 400", rec.Code)
	}
	// A wrong guess burns the code: the right verifier is too late.
	if rec := a.exchange(code, c.verifier); rec.Code != http.StatusBadRequest {
		t.Errorf("the right verifier after a wrong one: status %d, want 400", rec.Code)
	}
	if rec := a.exchange("unknown", c.verifier); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown code: status %d, want 400", rec.Code)
	}
}

func TestCLILoginCodeExpires(t *testing.T) {
	a := newApp(t)
	c := newCLIClient()
	code := cliBack(t, a.cliCallback(c, alice()), c.port).Get("code")

	a.clock.advance(cliCodeTTL + time.Second)
	if rec := a.exchange(code, c.verifier); rec.Code != http.StatusBadRequest {
		t.Errorf("an expired code: status %d, want 400", rec.Code)
	}
}

func TestCLILoginRefusesBadParameters(t *testing.T) {
	a := newApp(t)
	good := newCLIClient()
	challenge := oauth2.S256ChallengeFromVerifier(good.verifier)
	for name, q := range map[string]url.Values{
		"port 0":          {"cli_port": {"0"}, "cli_state": {good.state}, "cli_challenge": {challenge}},
		"port too big":    {"cli_port": {"65536"}, "cli_state": {good.state}, "cli_challenge": {challenge}},
		"port not a port": {"cli_port": {"80@evil.example"}, "cli_state": {good.state}, "cli_challenge": {challenge}},
		"no state":        {"cli_port": {"43210"}, "cli_challenge": {challenge}},
		"short state":     {"cli_port": {"43210"}, "cli_state": {"abc"}, "cli_challenge": {challenge}},
		"state with a /":  {"cli_port": {"43210"}, "cli_state": {good.state[:20] + "/"}, "cli_challenge": {challenge}},
		"no challenge":    {"cli_port": {"43210"}, "cli_state": {good.state}},
		"short challenge": {"cli_port": {"43210"}, "cli_state": {good.state}, "cli_challenge": {"abc"}},
		"only a port":     {"cli_port": {"43210"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := a.do("GET", "/auth/oidc?"+q.Encode(), nil)
			if rec.Code != http.StatusBadRequest || cookie(rec, loginCookie) != nil {
				t.Errorf("status %d, login cookie %v, want 400 and none", rec.Code, cookie(rec, loginCookie))
			}
		})
	}
}

func TestCLILoginWithNoBindingRuleComesBackWithAnError(t *testing.T) {
	a := newACLApp(t)
	c := newCLIClient()

	back := cliBack(t, a.cliCallback(c, olga()), c.port)
	if back.Get("error") != "no_binding" || back.Get("code") != "" || back.Get("state") != c.state {
		t.Errorf("back = %v, want error=no_binding, the state and no code", back)
	}
	if tokens, _ := a.store.ACLTokens(t.Context()); len(tokens) != 0 {
		t.Errorf("%d tokens made by a refused login", len(tokens))
	}
}

func TestCLILoginGetsWhatTheBindingRulesGive(t *testing.T) {
	a := newACLApp(t)
	a.policy("operator", `namespace "*" { policy = "read" }`)
	a.rule(`"ops" in list.groups`, acl.BindPolicy, "operator")
	c := newCLIClient()

	sess := sessionOf(t, a.exchange(cliBack(t, a.cliCallback(c, olga()), c.port).Get("code"), c.verifier))
	tok, err := a.store.ACLTokenBySecret(t.Context(), hashToken(sess.Secret), true)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Type != store.TokenClient || len(tok.Policies) != 1 || tok.Policies[0] != "operator" || tok.Origin != store.OriginLogin {
		t.Errorf("token = %+v, want olga's client session with operator", tok)
	}
}

func TestCLILoginInfoSaysTheMethod(t *testing.T) {
	a := newApp(t)
	rec := a.do("GET", "/auth/cli", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"method":"oidc"`) {
		t.Errorf("status %d, body %s", rec.Code, rec.Body)
	}
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "pw")+"\n")
	rec = ba.do("GET", "/auth/cli", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"method":"basic"`) {
		t.Errorf("basic: status %d, body %s", rec.Code, rec.Body)
	}
}

// -- basic ---------------------------------------------------------------------

func (ba *basicApp) cliLogin(body string, headers ...string) *httptest.ResponseRecorder {
	ba.t.Helper()
	req := httptest.NewRequest("POST", "/auth/cli/basic", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	ba.mux.ServeHTTP(rec, req)
	return rec
}

func TestBasicCLILogin(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")

	rec := ba.cliLogin(`{"username":"alice","password":"s3cret"}`)
	sess := sessionOf(t, rec)
	if len(rec.Result().Cookies()) != 0 {
		t.Error("the answer sets a cookie: the secret goes to the client only")
	}
	self := ba.do("GET", "/api/acl/token/self", nil, nil, "Authorization", bearer(sess.Secret))
	if self.Code != http.StatusOK || !strings.Contains(self.Body.String(), `"origin":"login"`) || !strings.Contains(self.Body.String(), `"identity":"basic:alice"`) {
		t.Errorf("self: status %d, body %s, want alice's login session", self.Code, self.Body)
	}
}

func TestBasicCLILoginRefuses(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	wrong := ba.cliLogin(`{"username":"alice","password":"nope"}`)
	unknown := ba.cliLogin(`{"username":"mallory","password":"s3cret"}`)
	for name, rec := range map[string]*httptest.ResponseRecorder{"a wrong password": wrong, "an unknown user": unknown} {
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s: status %d, body %s, want 401 with an error", name, rec.Code, rec.Body)
		}
	}
	if wrong.Body.String() != unknown.Body.String() {
		t.Errorf("the answers differ (%q, %q): they tell a wrong user from a wrong password", wrong.Body, unknown.Body)
	}
	if rec := ba.cliLogin(`not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("a body that is not JSON: status %d, want 400", rec.Code)
	}
	if rec := ba.cliLogin(`{"username":"alice","password":"s3cret"}`, "Sec-Fetch-Site", "cross-site"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: status %d, want 403", rec.Code)
	}
}

func TestBasicCLILoginWithNoBindingRule(t *testing.T) {
	ba := newBasicAppWith(t, "alice:"+bcryptHash(t, "s3cret")+"\n", "", true)
	rec := ba.cliLogin(`{"username":"alice","password":"s3cret"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no binding rule matches") {
		t.Errorf("status %d, body %s, want 403 naming the rules", rec.Code, rec.Body)
	}
}

// -- DELETE /api/acl/token/self ----------------------------------------------------

func TestTheTokenOfTheRequestRevokesItself(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	sess := sessionOf(t, ba.cliLogin(`{"username":"alice","password":"s3cret"}`))

	if rec := ba.do("DELETE", "/api/acl/token/self", nil, nil, "Authorization", bearer(sess.Secret)); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, body %s, want 204", rec.Code, rec.Body)
	}
	if rec := ba.do("GET", "/api/acl/token/self", nil, nil, "Authorization", bearer(sess.Secret)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the token after revoking itself: status %d, want 401", rec.Code)
	}
	if rec := ba.do("DELETE", "/api/acl/token/self", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", rec.Code)
	}
}

func TestTheBootstrapTokenCannotRevokeItself(t *testing.T) {
	a := newACLApp(t)
	rec := a.do("DELETE", "/api/acl/token/self", nil, "Authorization", bearer(a.boot))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bootstrap") {
		t.Errorf("status %d, body %s, want 400 naming the bootstrap token", rec.Code, rec.Body)
	}
	if rec := a.do("GET", "/api/acl/token/self", nil, "Authorization", bearer(a.boot)); rec.Code != http.StatusOK {
		t.Errorf("the bootstrap token afterwards: status %d, want 200", rec.Code)
	}
}

// -- the page at the end -----------------------------------------------------------

func TestCLIDonePage(t *testing.T) {
	ba := newBasicAppAt(t, "alice:"+bcryptHash(t, "pw")+"\n", "/nops")

	ok := ba.do("GET", "/auth/cli/done", nil, nil)
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), "You’re logged in") || !strings.Contains(ok.Body.String(), `href="/nops/"`) {
		t.Errorf("status %d, body %s, want the success page linking to the dashboard under its base path", ok.Code, ok.Body)
	}
	if strings.Contains(ok.Body.String(), "login__error") {
		t.Error("the success page shows an error")
	}

	failed := ba.do("GET", "/auth/cli/done?failed=1", nil, nil)
	if failed.Code != http.StatusOK || !strings.Contains(failed.Body.String(), "login__error") {
		t.Errorf("status %d, body %s, want the page with an error", failed.Code, failed.Body)
	}

	// Nothing of the query comes back in the page.
	evil := ba.do("GET", "/auth/cli/done?failed=%3Cscript%3Ealert(1)%3C/script%3E&message=pwned", nil, nil)
	if strings.Contains(evil.Body.String(), "script>alert") || strings.Contains(evil.Body.String(), "pwned") || strings.Contains(evil.Body.String(), "login__error") {
		t.Errorf("the page reflects the query: %s", evil.Body)
	}
}
