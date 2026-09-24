package web

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"log/slog"
)

const (
	testClientID     = "nops"
	testClientSecret = "client-secret"
	testRedirect     = "http://nops.test/auth/callback"
)

// -- fake clock, log buffer ----------------------------------------------------

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// -- fake identity provider ----------------------------------------------------

// idClaims are the claims of a user at the fake provider.
type idClaims struct {
	Sub               string
	PreferredUsername string
	Email             string
	EmailVerified     *bool
	Groups            []string
}

func (c idClaims) payload() map[string]any {
	m := map[string]any{}
	if c.PreferredUsername != "" {
		m["preferred_username"] = c.PreferredUsername
	}
	if c.Email != "" {
		m["email"] = c.Email
	}
	if c.EmailVerified != nil {
		m["email_verified"] = *c.EmailVerified
	}
	if c.Groups != nil {
		m["groups"] = c.Groups
	}
	return m
}

// grant is one authorization the fake provider will honour at its token endpoint.
type grant struct {
	claims idClaims
	// inUserinfo keeps every claim but the subject out of the ID token, as
	// Authelia does by default: they are only served by the userinfo endpoint.
	inUserinfo   bool
	mutate       func(map[string]any) // changes the ID token payload
	signKey      *rsa.PrivateKey      // signs with another key than the published one
	noIDToken    bool
	userinfoSub  string // userinfo answers for this subject instead
	userinfoFail bool

	nonce, challenge string // filled in when the login redirect is seen
	token            string // access token issued for it
}

type idp struct {
	t     *testing.T
	srv   *httptest.Server
	key   *rsa.PrivateKey
	clock *testClock

	down atomic.Bool
	hits atomic.Int32 // discovery requests

	mu     sync.Mutex
	grants map[string]*grant // code -> grant
	byTok  map[string]*grant // access token -> grant
}

func newIDP(t *testing.T, clock *testClock) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{t: t, key: key, clock: clock, grants: map[string]*grant{}, byTok: map[string]*grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/keys", p.keys)
	mux.HandleFunc("/token", p.token)
	mux.HandleFunc("/userinfo", p.userinfo)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) discovery(w http.ResponseWriter, r *http.Request) {
	if p.down.Load() {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	p.hits.Add(1)
	writeJSON(w, map[string]any{
		"issuer":                                p.srv.URL,
		"authorization_endpoint":                p.srv.URL + "/authorize",
		"token_endpoint":                        p.srv.URL + "/token",
		"jwks_uri":                              p.srv.URL + "/keys",
		"userinfo_endpoint":                     p.srv.URL + "/userinfo",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (p *idp) keys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
	}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// authorize is the user logging in at the provider: it records the grant
// against the nonce and PKCE challenge of the authorization request nops
// redirected to, and returns the code the provider would send back.
func (p *idp) authorize(authURL *url.URL, g grant) string {
	p.t.Helper()
	q := authURL.Query()
	if q.Get("client_id") != testClientID || q.Get("redirect_uri") != testRedirect || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		p.t.Fatalf("bad authorization request: %s", authURL)
	}
	g.nonce, g.challenge = q.Get("nonce"), q.Get("code_challenge")
	code := "code-" + randomToken()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grants[code] = &g
	return code
}

func (p *idp) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != testClientID || secret != testClientSecret || r.PostForm.Get("redirect_uri") != testRedirect {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	g := p.grants[r.PostForm.Get("code")]
	delete(p.grants, r.PostForm.Get("code")) // a code is single use
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if g == nil || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	g.token = "at-" + randomToken()
	p.mu.Lock()
	p.byTok[g.token] = g
	p.mu.Unlock()

	resp := map[string]any{"access_token": g.token, "token_type": "Bearer", "expires_in": 300}
	if !g.noIDToken {
		resp["id_token"] = p.idToken(g)
	}
	writeJSON(w, resp)
}

func (p *idp) idToken(g *grant) string {
	now := p.clock.now()
	m := map[string]any{
		"iss": p.srv.URL, "sub": g.claims.Sub, "aud": testClientID, "nonce": g.nonce,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
	if !g.inUserinfo {
		for k, v := range g.claims.payload() {
			m[k] = v
		}
	}
	if g.mutate != nil {
		g.mutate(m)
	}
	key := p.key
	if g.signKey != nil {
		key = g.signKey
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		p.t.Fatal(err)
	}
	payload, err := json.Marshal(m)
	if err != nil {
		p.t.Fatal(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		p.t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		p.t.Fatal(err)
	}
	return s
}

func (p *idp) userinfo(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	g := p.byTok[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	p.mu.Unlock()
	if g == nil || g.userinfoFail {
		http.Error(w, "nope", http.StatusUnauthorized)
		return
	}
	m := g.claims.payload()
	m["sub"] = g.claims.Sub
	if g.userinfoSub != "" {
		m["sub"] = g.userinfoSub
	}
	writeJSON(w, m)
}

// -- the app under test --------------------------------------------------------

type app struct {
	t     *testing.T
	idp   *idp
	auth  *Auth
	mux   *http.ServeMux
	clock *testClock
	logs  *syncBuffer
}

// newApp wires an Auth to a fake provider, with two protected routes: GET /page
// answers with the user, POST /act with "done".
func newApp(t *testing.T, tweak ...func(*AuthOptions)) *app {
	t.Helper()
	clock := &testClock{t: time.Now()}
	a := &app{t: t, clock: clock, idp: newIDP(t, clock), logs: &syncBuffer{}}
	o := AuthOptions{
		Issuer: a.idp.srv.URL, ClientID: testClientID, ClientSecret: testClientSecret, RedirectURL: testRedirect,
		AllowedUsers: []string{"alice", "bob@example.com"}, AllowedGroups: []string{"approvers"},
		Log: slog.New(slog.NewTextHandler(a.logs, nil)),
	}
	for _, f := range tweak {
		f(&o)
	}
	auth, err := NewAuth(o)
	if err != nil {
		t.Fatal(err)
	}
	auth.now = clock.now
	a.auth = auth
	a.mux = http.NewServeMux()
	auth.Register(a.mux)
	a.mux.Handle("GET /page", auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		w.Write([]byte("user=" + u))
	})))
	a.mux.Handle("POST /act", auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("done"))
	})))
	return a
}

func (a *app) do(method, target string, cookies []*http.Cookie, headers ...string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	a.mux.ServeHTTP(rec, req)
	return rec
}

// cookie returns the cookie name a response sets, or nil. A cookie the
// response deletes (MaxAge < 0) counts as not set.
func cookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 && c.Value != "" {
			return c
		}
	}
	return nil
}

func deleted(rec *httptest.ResponseRecorder, name string) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

// startLogin opens /auth/login and returns the provider redirect and the
// login cookie.
func (a *app) startLogin(next string) (*url.URL, *http.Cookie) {
	a.t.Helper()
	target := "/auth/login"
	if next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	rec := a.do("GET", target, nil)
	if rec.Code != http.StatusFound {
		a.t.Fatalf("login: status %d, body %q", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		a.t.Fatal(err)
	}
	lc := cookie(rec, loginCookie)
	if lc == nil {
		a.t.Fatal("login set no login cookie")
	}
	return loc, lc
}

// callback finishes a login for g, and returns the callback response.
func (a *app) callback(next string, g grant) *httptest.ResponseRecorder {
	a.t.Helper()
	loc, lc := a.startLogin(next)
	code := a.idp.authorize(loc, g)
	return a.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(loc.Query().Get("state")), []*http.Cookie{lc})
}

// loggedIn logs g in and returns the session cookie.
func (a *app) loggedIn(g grant) *http.Cookie {
	a.t.Helper()
	rec := a.callback("", g)
	if rec.Code != http.StatusFound {
		a.t.Fatalf("callback: status %d, body %q", rec.Code, rec.Body.String())
	}
	s := cookie(rec, sessionCookie)
	if s == nil {
		a.t.Fatal("no session cookie")
	}
	return s
}

func alice() grant { return grant{claims: idClaims{Sub: "sub-alice", PreferredUsername: "alice"}} }

func boolp(b bool) *bool { return &b }

// -- tests ---------------------------------------------------------------------

func TestLoginFlow(t *testing.T) {
	a := newApp(t)

	// Not logged in: the page sends the browser to the login, remembering where it was.
	rec := a.do("GET", "/page?x=1", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login?next=%2Fpage%3Fx%3D1" {
		t.Fatalf("no session: status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = a.callback("/page?x=1", alice())
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/page?x=1" {
		t.Fatalf("callback: status %d, Location %q, body %q", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	sess := cookie(rec, sessionCookie)
	if sess == nil {
		t.Fatal("no session cookie")
	}
	if !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" || sess.MaxAge != int(sessionTTL.Seconds()) {
		t.Errorf("session cookie attributes: %+v", sess)
	}
	if !deleted(rec, loginCookie) {
		t.Error("the login cookie is not cleared after use")
	}

	rec = a.do("GET", "/page", []*http.Cookie{sess})
	if rec.Code != http.StatusOK || rec.Body.String() != "user=alice" {
		t.Errorf("with a session: status %d, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(a.logs.String(), "login") || !strings.Contains(a.logs.String(), "user=alice") {
		t.Errorf("no login in the log: %s", a.logs.String())
	}
}

func TestLoginAsksForTheGroupsScopeOnlyWithGroups(t *testing.T) {
	a := newApp(t)
	loc, _ := a.startLogin("")
	if got := loc.Query().Get("scope"); got != "openid profile email groups" {
		t.Errorf("scope with a group allowlist = %q", got)
	}

	a = newApp(t, func(o *AuthOptions) { o.AllowedGroups = nil })
	loc, _ = a.startLogin("")
	if got := loc.Query().Get("scope"); got != "openid profile email" {
		t.Errorf("scope without groups = %q", got)
	}
}

func TestActorAndAllowlist(t *testing.T) {
	tests := []struct {
		name      string
		grant     grant
		wantActor string // "" = refused with 403
	}{
		{"username", grant{claims: idClaims{Sub: "s1", PreferredUsername: "alice"}}, "alice"},
		{"username is case-insensitive", grant{claims: idClaims{Sub: "s1", PreferredUsername: "ALICE"}}, "ALICE"},
		{"email when there is no username", grant{claims: idClaims{Sub: "s2", Email: "bob@example.com", EmailVerified: boolp(true)}}, "bob@example.com"},
		{"email without email_verified", grant{claims: idClaims{Sub: "s2", Email: "bob@example.com"}}, "bob@example.com"},
		{"unverified email is no identity", grant{claims: idClaims{Sub: "s2", Email: "bob@example.com", EmailVerified: boolp(false)}}, ""},
		{"a group alone, actor is the subject", grant{claims: idClaims{Sub: "s3", Groups: []string{"devs", "approvers"}}}, "s3"},
		{"a group, actor is the username", grant{claims: idClaims{Sub: "s3", PreferredUsername: "carol", Groups: []string{"approvers"}}}, "carol"},
		{"outsider", grant{claims: idClaims{Sub: "s4", PreferredUsername: "mallory", Email: "mallory@example.com", Groups: []string{"devs"}}}, ""},
		{"groups only match exactly", grant{claims: idClaims{Sub: "s5", PreferredUsername: "eve", Groups: []string{"approvers-old"}}}, ""},
		{"claims served by userinfo only", grant{claims: idClaims{Sub: "s6", PreferredUsername: "dave", Groups: []string{"approvers"}}, inUserinfo: true}, "dave"},
		{"outsider through userinfo", grant{claims: idClaims{Sub: "s7", PreferredUsername: "eve", Groups: []string{"devs"}}, inUserinfo: true}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newApp(t)
			rec := a.callback("", tt.grant)
			if tt.wantActor == "" {
				if rec.Code != http.StatusForbidden || cookie(rec, sessionCookie) != nil {
					t.Fatalf("status %d, session %v: want 403 and no session", rec.Code, cookie(rec, sessionCookie))
				}
				if !strings.Contains(a.logs.String(), "not in the allowlist") {
					t.Errorf("refusal not logged: %s", a.logs.String())
				}
				return
			}
			if rec.Code != http.StatusFound {
				t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
			}
			got := a.do("GET", "/page", []*http.Cookie{cookie(rec, sessionCookie)})
			if got.Body.String() != "user="+tt.wantActor {
				t.Errorf("actor = %q, want %q", got.Body.String(), tt.wantActor)
			}
		})
	}
}

// Nothing in a login response is trusted before it is checked.
func TestCallbackRejects(t *testing.T) {
	past := func(m map[string]any) { m["exp"] = time.Now().Add(-time.Hour).Unix() }
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		grant grant
		want  int
	}{
		{"expired ID token", func() grant { g := alice(); g.mutate = past; return g }(), http.StatusUnauthorized},
		{"ID token signed with another key", func() grant { g := alice(); g.signKey = other; return g }(), http.StatusUnauthorized},
		{"wrong audience", func() grant { g := alice(); g.mutate = func(m map[string]any) { m["aud"] = "someone-else" }; return g }(), http.StatusUnauthorized},
		{"wrong issuer", func() grant {
			g := alice()
			g.mutate = func(m map[string]any) { m["iss"] = "https://evil.example.com" }
			return g
		}(), http.StatusUnauthorized},
		{"nonce of another login", func() grant { g := alice(); g.mutate = func(m map[string]any) { m["nonce"] = "replayed" }; return g }(), http.StatusUnauthorized},
		{"no ID token", func() grant { g := alice(); g.noIDToken = true; return g }(), http.StatusUnauthorized},
		{"userinfo for another subject", func() grant {
			g := grant{claims: idClaims{Sub: "s1", PreferredUsername: "alice", Groups: []string{"approvers"}}, inUserinfo: true, userinfoSub: "someone-else"}
			return g
		}(), http.StatusUnauthorized},
		{"userinfo fails", func() grant {
			return grant{claims: idClaims{Sub: "s1", Groups: []string{"approvers"}}, inUserinfo: true, userinfoFail: true}
		}(), http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newApp(t)
			rec := a.callback("", tt.grant)
			if rec.Code != tt.want || cookie(rec, sessionCookie) != nil {
				t.Errorf("status %d, session %v, want %d and no session", rec.Code, cookie(rec, sessionCookie), tt.want)
			}
		})
	}
}

func TestCallbackRejectsBrokenLoginState(t *testing.T) {
	a := newApp(t)
	loc, lc := a.startLogin("")
	code := a.idp.authorize(loc, alice())
	state := loc.Query().Get("state")

	tests := []struct {
		name    string
		target  string
		cookies []*http.Cookie
		advance time.Duration
	}{
		{"state of another login", "/auth/callback?code=" + code + "&state=other", []*http.Cookie{lc}, 0},
		{"no state", "/auth/callback?code=" + code, []*http.Cookie{lc}, 0},
		{"no code", "/auth/callback?state=" + state, []*http.Cookie{lc}, 0},
		{"no login cookie", "/auth/callback?code=" + code + "&state=" + state, nil, 0},
		{"tampered login cookie", "/auth/callback?code=" + code + "&state=" + state, []*http.Cookie{{Name: loginCookie, Value: lc.Value[:len(lc.Value)-4] + "AAAA"}}, 0},
		{"login took too long", "/auth/callback?code=" + code + "&state=" + state, []*http.Cookie{lc}, loginTTL + time.Minute},
		{"the provider refused", "/auth/callback?error=access_denied&state=" + state, []*http.Cookie{lc}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a.clock.advance(tt.advance)
			defer a.clock.advance(-tt.advance)
			rec := a.do("GET", tt.target, tt.cookies)
			if rec.Code < 400 || cookie(rec, sessionCookie) != nil {
				t.Errorf("status %d, session %v: want an error and no session", rec.Code, cookie(rec, sessionCookie))
			}
		})
	}

	// The very same, valid response still works: none of the above consumed the code.
	if rec := a.do("GET", "/auth/callback?code="+code+"&state="+state, []*http.Cookie{lc}); rec.Code != http.StatusFound {
		t.Fatalf("valid callback: status %d, %q", rec.Code, rec.Body.String())
	}
	// A code is single use: replaying the whole response fails.
	if rec := a.do("GET", "/auth/callback?code="+code+"&state="+state, []*http.Cookie{lc}); rec.Code != http.StatusUnauthorized {
		t.Errorf("replayed callback: status %d, want 401", rec.Code)
	}
}

func TestSessionValidity(t *testing.T) {
	a := newApp(t)
	sess := a.loggedIn(alice())

	rec := a.do("POST", "/act", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST without a session: status %d, want 401", rec.Code)
	}
	if rec := a.do("POST", "/act", []*http.Cookie{sess}); rec.Code != http.StatusOK || rec.Body.String() != "done" {
		t.Errorf("POST with a session: status %d, %q", rec.Code, rec.Body.String())
	}

	tampered := &http.Cookie{Name: sessionCookie, Value: sess.Value[:len(sess.Value)-4] + "AAAA"}
	if rec := a.do("GET", "/page", []*http.Cookie{tampered}); rec.Code != http.StatusFound {
		t.Errorf("tampered cookie: status %d, want the login redirect", rec.Code)
	}

	// The key lives in the process: a session of another one (a restart) is no session.
	restarted := newApp(t)
	if rec := restarted.do("GET", "/page", []*http.Cookie{sess}); rec.Code != http.StatusFound {
		t.Errorf("session signed by another key: status %d, want the login redirect", rec.Code)
	}

	a.clock.advance(sessionTTL - time.Minute)
	if rec := a.do("GET", "/page", []*http.Cookie{sess}); rec.Code != http.StatusOK {
		t.Errorf("just before the expiry: status %d, want 200", rec.Code)
	}
	a.clock.advance(2 * time.Minute)
	if rec := a.do("GET", "/page", []*http.Cookie{sess}); rec.Code != http.StatusFound {
		t.Errorf("after the expiry: status %d, want the login redirect", rec.Code)
	}
}

func TestLogout(t *testing.T) {
	a := newApp(t)
	sess := a.loggedIn(alice())

	if rec := a.do("GET", "/auth/logout", []*http.Cookie{sess}); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET logout: status %d, want 405", rec.Code)
	}
	rec := a.do("POST", "/auth/logout", []*http.Cookie{sess})
	if rec.Code != http.StatusSeeOther || !deleted(rec, sessionCookie) {
		t.Errorf("logout: status %d, cookie deleted %v", rec.Code, deleted(rec, sessionCookie))
	}
	if rec := a.do("POST", "/auth/logout", []*http.Cookie{sess}, "Sec-Fetch-Site", "cross-site"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site logout: status %d, want 403", rec.Code)
	}
}

func TestCrossOriginWritesAreRefused(t *testing.T) {
	a := newApp(t)
	sess := a.loggedIn(alice())
	cs := []*http.Cookie{sess}

	for _, tt := range []struct {
		name    string
		headers []string
		want    int
	}{
		{"same origin", []string{"Sec-Fetch-Site", "same-origin"}, http.StatusOK},
		{"not a browser", nil, http.StatusOK},
		{"cross-site", []string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
		{"same-site, another subdomain", []string{"Sec-Fetch-Site", "same-site"}, http.StatusForbidden},
		{"foreign Origin, no Fetch Metadata", []string{"Origin", "https://evil.example.com"}, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if rec := a.do("POST", "/act", cs, tt.headers...); rec.Code != tt.want {
				t.Errorf("status %d, want %d", rec.Code, tt.want)
			}
		})
	}
	// A cross-site GET is a link somebody clicked: allowed, and it changes nothing.
	if rec := a.do("GET", "/page", cs, "Sec-Fetch-Site", "cross-site"); rec.Code != http.StatusOK {
		t.Errorf("cross-site GET: status %d, want 200", rec.Code)
	}
}

// An unreachable provider never stops nops: the login says so, loudly, and
// works again as soon as the provider does.
func TestProviderDownIsRetriedAtEveryLogin(t *testing.T) {
	a := newApp(t)
	if a.idp.hits.Load() != 0 {
		t.Fatal("NewAuth contacted the provider")
	}
	a.idp.down.Store(true)

	rec := a.do("GET", "/auth/login", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("provider down: status %d, want 503", rec.Code)
	}
	if !strings.Contains(a.logs.String(), "level=ERROR") || !strings.Contains(a.logs.String(), "identity provider unavailable") {
		t.Errorf("no ERROR in the log: %s", a.logs.String())
	}
	if rec := a.do("GET", "/page", nil); rec.Code != http.StatusFound {
		t.Errorf("a protected page with the provider down: status %d, want the login redirect", rec.Code)
	}

	a.idp.down.Store(false)
	a.loggedIn(alice())
	if a.idp.hits.Load() != 1 {
		t.Fatalf("discovery requests = %d, want 1", a.idp.hits.Load())
	}
	a.loggedIn(alice())
	if a.idp.hits.Load() != 1 {
		t.Errorf("discovery requests = %d after a second login, want it cached", a.idp.hits.Load())
	}
}

func TestSecureCookiesFollowThePublicURL(t *testing.T) {
	a := newApp(t, func(o *AuthOptions) { o.RedirectURL = testRedirect })
	if _, lc := a.startLogin(""); lc.Secure {
		t.Error("cookie is Secure on an http public URL")
	}

	// The provider only accepts the redirect URL of the fake app, so the https
	// case checks the cookie the login sets, which needs no round trip.
	a = newApp(t, func(o *AuthOptions) { o.RedirectURL = "https://nops.example.com/auth/callback" })
	if _, lc := a.startLogin(""); !lc.Secure {
		t.Error("cookie is not Secure on an https public URL")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                         "/",
		"/":                        "/",
		"/deployments/01ABC":       "/deployments/01ABC",
		"/page?x=1&y=2":            "/page?x=1&y=2",
		"//evil.example.com":       "/",
		"/\\evil.example.com":      "/",
		"https://evil.example.com": "/",
		"evil":                     "/",
		"/a\r\nSet-Cookie: x=y":    "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewAuthValidates(t *testing.T) {
	ok := AuthOptions{Issuer: "https://idp.example.com", ClientID: "nops", ClientSecret: "s",
		RedirectURL: "https://nops.example.com/auth/callback", AllowedUsers: []string{"alice"}}
	if _, err := NewAuth(ok); err != nil {
		t.Fatalf("valid options: %v", err)
	}
	for name, tweak := range map[string]func(*AuthOptions){
		"no issuer":      func(o *AuthOptions) { o.Issuer = "" },
		"no client id":   func(o *AuthOptions) { o.ClientID = "" },
		"no secret":      func(o *AuthOptions) { o.ClientSecret = "" },
		"no allowlist":   func(o *AuthOptions) { o.AllowedUsers = nil },
		"bad redirect":   func(o *AuthOptions) { o.RedirectURL = "callback" },
		"empty redirect": func(o *AuthOptions) { o.RedirectURL = "" },
	} {
		o := ok
		tweak(&o)
		if _, err := NewAuth(o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestUserFromWithoutRequire(t *testing.T) {
	if u, ok := UserFrom(httptest.NewRequest("GET", "/", nil).Context()); ok || u != "" {
		t.Errorf("UserFrom on a plain context = %q, %v", u, ok)
	}
}
