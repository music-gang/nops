package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func bcryptHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func writeUsersFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// basicApp wires a BasicAuth to a real mux, with the same GET /page and
// POST /act protected routes auth_test.go's app uses for OIDC, so the two
// backends can be exercised the same way.
type basicApp struct {
	t    *testing.T
	auth *BasicAuth
	mux  *http.ServeMux
	logs *syncBuffer
}

func newBasicApp(t *testing.T, usersContent string, tweak ...func(*BasicAuthOptions)) *basicApp {
	t.Helper()
	logs := &syncBuffer{}
	o := BasicAuthOptions{
		UsersFile: writeUsersFile(t, usersContent),
		PublicURL: "http://nops.test",
		Log:       slog.New(slog.NewTextHandler(logs, nil)),
	}
	for _, f := range tweak {
		f(&o)
	}
	auth, err := NewBasicAuth(o)
	if err != nil {
		t.Fatal(err)
	}
	ba := &basicApp{t: t, auth: auth, logs: logs}
	ba.mux = http.NewServeMux()
	auth.Register(ba.mux)
	ba.mux.Handle("GET /page", auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		w.Write([]byte("user=" + u))
	})))
	ba.mux.Handle("POST /act", auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("done"))
	})))
	return ba
}

func (ba *basicApp) do(method, target string, body io.Reader, cookies []*http.Cookie, headers ...string) *httptest.ResponseRecorder {
	ba.t.Helper()
	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	ba.mux.ServeHTTP(rec, req)
	return rec
}

func (ba *basicApp) login(username, password string) *httptest.ResponseRecorder {
	return ba.do("POST", "/auth/login", strings.NewReader(url.Values{"username": {username}, "password": {password}}.Encode()), nil)
}

// -- NewBasicAuth validation --------------------------------------------------

func TestNewBasicAuthValidates(t *testing.T) {
	valid := writeUsersFile(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name string
		opts BasicAuthOptions
	}{
		{"no users file", BasicAuthOptions{PublicURL: "http://nops.test", Log: log}},
		{"missing users file", BasicAuthOptions{UsersFile: "/does/not/exist", PublicURL: "http://nops.test", Log: log}},
		{"empty users file", BasicAuthOptions{UsersFile: writeUsersFile(t, "# just a comment\n"), PublicURL: "http://nops.test", Log: log}},
		{"malformed line", BasicAuthOptions{UsersFile: writeUsersFile(t, "not-a-valid-line\n"), PublicURL: "http://nops.test", Log: log}},
		{"not a bcrypt hash", BasicAuthOptions{UsersFile: writeUsersFile(t, "alice:not-bcrypt\n"), PublicURL: "http://nops.test", Log: log}},
		{"duplicate user", BasicAuthOptions{UsersFile: writeUsersFile(t, "alice:"+bcryptHash(t, "a")+"\nalice:"+bcryptHash(t, "b")+"\n"), PublicURL: "http://nops.test", Log: log}},
		{"no public URL", BasicAuthOptions{UsersFile: valid, Log: log}},
		{"invalid public URL", BasicAuthOptions{UsersFile: valid, PublicURL: "not a url", Log: log}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewBasicAuth(tt.opts); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestParseUsersFileIgnoresBlankLinesAndComments(t *testing.T) {
	hash := bcryptHash(t, "s3cret")
	path := writeUsersFile(t, "\n# a comment\n  \nalice:"+hash+"\n# trailing\n")
	users, err := parseUsersFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users["alice"] != hash {
		t.Errorf("users = %+v", users)
	}
}

// -- login flow ---------------------------------------------------------------

func TestBasicAuthLoginFlow(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")

	rec := ba.login("alice", "s3cret")
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Location") != "/" {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	sess := cookie(rec, sessionCookie)
	if sess == nil {
		t.Fatal("no session cookie set")
	}

	page := ba.do("GET", "/page", nil, []*http.Cookie{sess})
	if page.Code != http.StatusOK || page.Body.String() != "user=alice" {
		t.Errorf("status %d, body %q", page.Code, page.Body.String())
	}
}

func TestBasicAuthLoginKeepsNext(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	rec := ba.do("POST", "/auth/login", strings.NewReader(url.Values{
		"username": {"alice"}, "password": {"s3cret"}, "next": {"/deployments/d1"},
	}.Encode()), nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/deployments/d1" {
		t.Errorf("status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestBasicAuthWrongPassword(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	rec := ba.login("alice", "wrong")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (the login form again)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Invalid username or password") {
		t.Error("missing the generic error")
	}
	if cookie(rec, sessionCookie) != nil {
		t.Error("a failed login must not set a session cookie")
	}
}

func TestBasicAuthUnknownUserGetsTheSameGenericError(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	wrongPassword := ba.login("alice", "wrong")
	unknownUser := ba.login("bob", "whatever")

	if unknownUser.Code != http.StatusOK || cookie(unknownUser, sessionCookie) != nil {
		t.Fatalf("status %d, cookie %v", unknownUser.Code, cookie(unknownUser, sessionCookie))
	}
	// An unknown username must read exactly like a wrong password: nothing
	// in the response should tell them apart.
	if wrongPassword.Body.String() != unknownUser.Body.String() {
		t.Errorf("wrong password body %q\nunknown user body   %q", wrongPassword.Body.String(), unknownUser.Body.String())
	}
}

func TestBasicAuthRequireRedirectsToLogin(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	rec := ba.do("GET", "/page", nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/auth/login") {
		t.Errorf("Location = %q", loc)
	}

	rec = ba.do("POST", "/act", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestBasicAuthLoginFormRenders(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	rec := ba.do("GET", "/auth/login?next="+url.QueryEscape("/deployments/d1"), nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`name="username"`, `name="password"`, `/deployments/d1`} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
}

// A next that would leave the site (browsers read "/<TAB>/evil" as "//evil")
// is neither rendered into the login form nor followed after the login.
func TestBasicAuthIgnoresAnOffSiteNext(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	const next = "/\t/evil.example.com"

	rec := ba.do("GET", "/auth/login?next="+url.QueryEscape(next), nil, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "evil.example.com") {
		t.Errorf("login form: status %d, it carries the off-site next: %v", rec.Code, strings.Contains(rec.Body.String(), "evil.example.com"))
	}

	form := url.Values{"username": {"alice"}, "password": {"s3cret"}, "next": {next}}
	rec = ba.do("POST", "/auth/login", strings.NewReader(form.Encode()), nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Errorf("login: status %d, Location %q, want a redirect to /", rec.Code, rec.Header().Get("Location"))
	}
}

func TestBasicAuthLogout(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	sess := cookie(ba.login("alice", "s3cret"), sessionCookie)

	rec := ba.do("POST", "/auth/logout", nil, []*http.Cookie{sess})
	if rec.Code != http.StatusSeeOther || !deleted(rec, sessionCookie) {
		t.Errorf("status %d, cookie deleted %v", rec.Code, deleted(rec, sessionCookie))
	}
}

func TestBasicAuthLoginRefusesCrossOrigin(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	rec := ba.do("POST", "/auth/login",
		strings.NewReader(url.Values{"username": {"alice"}, "password": {"s3cret"}}.Encode()),
		nil, "Sec-Fetch-Site", "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if cookie(rec, sessionCookie) != nil {
		t.Error("a refused cross-origin login must not set a session cookie")
	}
}
