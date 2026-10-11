package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// nops login and nops logout, and the session they save, against a stub of
// the /auth/cli routes of Nops.

const sessionSecret = "nops_session-secret"

// configDir points the session file at a directory of the test, and returns
// the path of the file.
func configDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := userConfigDir
	userConfigDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { userConfigDir = old })
	return filepath.Join(dir, "nops", "session")
}

func future() string { return time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339) }

func basicStub(t *testing.T) (*stub, string) {
	t.Helper()
	return newStub(t, map[string]answer{
		"GET /auth/cli":           {200, `{"method":"basic"}`},
		"POST /auth/cli/basic":    {200, `{"secret":"` + sessionSecret + `","expires_at":"` + future() + `"}`},
		"GET /api/acl/token/self": {200, `{"accessor_id":"01A","name":"alice","type":"client","origin":"login"}`},
		"GET /api/jobs":           {200, `{"jobs":[]}`},
	})
}

func saveSession(t *testing.T, addr string, expires time.Time) {
	t.Helper()
	if err := (savedSession{Addr: addr, Secret: sessionSecret, ExpiresAt: expires}).save(); err != nil {
		t.Fatal(err)
	}
}

func noSecret(t *testing.T, what string, outs ...string) {
	t.Helper()
	for _, o := range outs {
		if strings.Contains(o, sessionSecret) || strings.Contains(o, "s3cret") {
			t.Errorf("%s shows a secret: %q", what, o)
		}
	}
}

func TestLoginWithBasicSavesTheSession(t *testing.T) {
	file := configDir(t)
	s, addr := basicStub(t)

	code, out, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "alice\ns3cret\n", "login")
	if code != 0 || !strings.Contains(out, "Logged in to "+addr+" as alice") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "Username:") || !strings.Contains(errOut, "Password:") {
		t.Errorf("stderr = %q, want the prompts on the error output", errOut)
	}
	noSecret(t, "login", out, errOut)
	if !strings.Contains(s.seen[1].body, `"username":"alice"`) || !strings.Contains(s.seen[1].body, `"password":"s3cret"`) {
		t.Errorf("credentials sent = %q", s.seen[1].body)
	}
	if s.seen[0].auth != "" {
		t.Errorf("the login asked with a token: %q", s.seen[0].auth)
	}

	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("session file: %v, %v, want mode 0600", fi, err)
		}
		if fi, err := os.Stat(filepath.Dir(file)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("session directory: %v, %v, want mode 0700", fi, err)
		}
	}

	// The next command uses it, with no token anywhere.
	if code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "jobs"); code != 0 {
		t.Fatalf("jobs: exit %d: %s", code, errOut)
	}
	if got := s.seen[len(s.seen)-1].auth; got != "Bearer "+sessionSecret {
		t.Errorf("auth = %q, want the saved session", got)
	}
}

func TestLoginWithBasicRefused(t *testing.T) {
	file := configDir(t)
	s, addr := basicStub(t)
	s.answers["POST /auth/cli/basic"] = answer{401, `{"error":"invalid username or password"}`}

	code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "alice\nwrong\n", "login")
	if code != 1 || !strings.Contains(errOut, "invalid username or password") {
		t.Errorf("exit %d, stderr %q, want 1 with Nops's message", code, errOut)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("a refused login saved a session")
	}
}

func TestLoginInputAndSettings(t *testing.T) {
	configDir(t)
	s, addr := basicStub(t)
	for name, tt := range map[string]struct {
		env   map[string]string
		stdin string
		args  []string
		code  int
		want  string
	}{
		"no URL":         {nil, "alice\npw\n", []string{"login"}, 2, "NOPS_ADDR"},
		"-addr":          {nil, "alice\npw\n", []string{"login", "-addr", addr}, 0, "Logged in"},
		"no input":       {map[string]string{"NOPS_ADDR": addr}, "", []string{"login"}, 1, "no username"},
		"no password":    {map[string]string{"NOPS_ADDR": addr}, "alice\n", []string{"login"}, 1, "no password"},
		"empty password": {map[string]string{"NOPS_ADDR": addr}, "alice\n\n", []string{"login"}, 1, "cannot be empty"},
		"an argument":    {map[string]string{"NOPS_ADDR": addr}, "", []string{"login", "x"}, 2, "takes no argument"},
	} {
		if code, out, errOut := run(t, tt.env, tt.stdin, tt.args...); code != tt.code || !strings.Contains(out+errOut, tt.want) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want exit %d saying %q", name, code, out, errOut, tt.code, tt.want)
		}
	}

	// A mode this client does not know, and a server that is not Nops.
	s.answers["GET /auth/cli"] = answer{200, `{"method":"saml"}`}
	if code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "login"); code != 1 || !strings.Contains(errOut, `"saml"`) {
		t.Errorf("unknown mode: exit %d, stderr %q", code, errOut)
	}
	delete(s.answers, "GET /auth/cli")
	if code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "login"); code != 1 || !strings.Contains(errOut, "ask how Nops logs in") {
		t.Errorf("an old server: exit %d, stderr %q", code, errOut)
	}
}

func TestSessionIsTheLastChoiceOfToken(t *testing.T) {
	configDir(t)
	s, addr := basicStub(t)
	file := writeFile(t, t.TempDir(), "token", "from-file\n")

	saveSession(t, addr, time.Now().Add(time.Hour))

	run(t, map[string]string{"NOPS_ADDR": addr, "NOPS_TOKEN": "from-env"}, "", "jobs")
	if got := s.seen[len(s.seen)-1].auth; got != "Bearer from-env" {
		t.Errorf("NOPS_TOKEN: auth = %q, want it before the session", got)
	}
	run(t, map[string]string{"NOPS_ADDR": addr, "NOPS_TOKEN": "from-env"}, "", "jobs", "-token-file", file)
	if got := s.seen[len(s.seen)-1].auth; got != "Bearer from-file" {
		t.Errorf("-token-file: auth = %q, want it before the session", got)
	}
	run(t, map[string]string{"NOPS_ADDR": addr}, "", "jobs")
	if got := s.seen[len(s.seen)-1].auth; got != "Bearer "+sessionSecret {
		t.Errorf("auth = %q, want the session", got)
	}
}

func TestSessionOfAnotherServerIsNotSent(t *testing.T) {
	configDir(t)
	s, addr := basicStub(t)
	saveSession(t, "https://other.example.com", time.Now().Add(time.Hour))

	code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "jobs")
	if code != 2 || !strings.Contains(errOut, "nops login") || !strings.Contains(errOut, "https://other.example.com") {
		t.Errorf("exit %d, stderr %q, want a usage error naming nops login and the session's URL", code, errOut)
	}
	noSecret(t, "the error", errOut)
	if len(s.seen) != 0 {
		t.Errorf("%d requests sent without a token", len(s.seen))
	}
}

func TestExpiredAndBrokenSessions(t *testing.T) {
	file := configDir(t)
	s, addr := basicStub(t)

	saveSession(t, addr, time.Now().Add(-time.Minute))
	if code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "jobs"); code != 2 || !strings.Contains(errOut, "ended") || !strings.Contains(errOut, "nops login") {
		t.Errorf("expired: exit %d, stderr %q", code, errOut)
	}

	if err := os.WriteFile(file, []byte(`{"addr":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "jobs"); code != 1 || !strings.Contains(errOut, file) || !strings.Contains(errOut, "not a session") {
		t.Errorf("broken: exit %d, stderr %q, want it to name the file", code, errOut)
	}
	if len(s.seen) != 0 {
		t.Errorf("%d requests sent with no valid session", len(s.seen))
	}
}

func TestAnEndedSessionSaysSoOn401(t *testing.T) {
	configDir(t)
	s, addr := basicStub(t)
	s.answers["GET /api/jobs"] = answer{401, `{"error":"the token is not valid"}`}

	saveSession(t, addr, time.Now().Add(time.Hour))
	if code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "jobs"); code != 1 || !strings.Contains(errOut, "run nops login") {
		t.Errorf("session: exit %d, stderr %q, want a hint to log in again", code, errOut)
	}
	if code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr, "NOPS_TOKEN": "t"}, "", "jobs"); code != 1 || strings.Contains(errOut, "nops login") {
		t.Errorf("a token of the environment: exit %d, stderr %q, want no hint", code, errOut)
	}
}

// -- OIDC -------------------------------------------------------------------------

// browser plays the browser of an OIDC login: it is what openBrowser calls, and it
// goes back to the client the way Nops sends it.
type browser struct {
	t        *testing.T
	callback func(start *url.URL) []string // the queries to send to /callback, in order
	started  *url.URL
	landed   chan []string // the answers: status and Location of each
}

func (b *browser) install() {
	b.landed = make(chan []string, 1)
	old := openBrowser
	openBrowser = func(u string) error {
		start, err := url.Parse(u)
		if err != nil {
			return err
		}
		b.started = start
		go func() {
			var got []string
			noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 10 * time.Second}
			for _, q := range b.callback(start) {
				resp, err := noFollow.Get("http://127.0.0.1:" + start.Query().Get("cli_port") + "/callback?" + q)
				if err != nil {
					got = append(got, err.Error())
					continue
				}
				resp.Body.Close()
				got = append(got, resp.Status+" "+resp.Header.Get("Location"))
			}
			b.landed <- got
		}()
		return nil
	}
	b.t.Cleanup(func() { openBrowser = old })
}

func oidcStub(t *testing.T) (*stub, string) {
	t.Helper()
	return newStub(t, map[string]answer{
		"GET /auth/cli":           {200, `{"method":"oidc"}`},
		"POST /auth/cli/token":    {200, `{"secret":"` + sessionSecret + `","expires_at":"` + future() + `"}`},
		"GET /api/acl/token/self": {200, `{"name":"olga"}`},
	})
}

func TestLoginWithOIDCThroughTheBrowser(t *testing.T) {
	file := configDir(t)
	s, addr := oidcStub(t)
	b := &browser{t: t, callback: func(start *url.URL) []string {
		state := start.Query().Get("cli_state")
		return []string{"code=x&state=wrong", "code=the-code&state=" + url.QueryEscape(state)}
	}}
	b.install()

	code, out, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "login")
	if code != 0 || !strings.Contains(out, "Logged in to "+addr+" as olga") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	noSecret(t, "login", out, errOut)
	if !strings.Contains(errOut, addr+"/auth/oidc?") {
		t.Errorf("stderr = %q, want the address to open, in case the browser does not", errOut)
	}

	// What Nops was asked: the client's challenge is the one of the verifier it holds.
	q := b.started.Query()
	var exchanged struct{ Code, Verifier string }
	for _, r := range s.seen {
		if r.path == "/auth/cli/token" {
			json.Unmarshal([]byte(r.body), &exchanged)
		}
	}
	if exchanged.Code != "the-code" || oauth2.S256ChallengeFromVerifier(exchanged.Verifier) != q.Get("cli_challenge") {
		t.Errorf("exchange = %+v, want the code and the verifier of the challenge %s", exchanged, q.Get("cli_challenge"))
	}

	// A callback with another state is refused, and the right one goes on to Nops's page.
	got := <-b.landed
	if !strings.HasPrefix(got[0], "400 ") || got[1] != "303 See Other "+addr+"/auth/cli/done" {
		t.Errorf("browser saw %q, want 400 for the wrong state and a 303 to the done page", got)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("no session saved: %v", err)
	}
}

func TestLoginWithOIDCRefused(t *testing.T) {
	file := configDir(t)
	s, addr := oidcStub(t)
	b := &browser{t: t, callback: func(start *url.URL) []string {
		return []string{"error=no_binding&state=" + url.QueryEscape(start.Query().Get("cli_state"))}
	}}
	b.install()

	code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "login")
	if code != 1 || !strings.Contains(errOut, "no binding rule matches your login") {
		t.Errorf("exit %d, stderr %q, want the refusal said in the terminal", code, errOut)
	}
	if got := <-b.landed; got[0] != "303 See Other "+addr+"/auth/cli/done?failed=1" {
		t.Errorf("browser saw %q, want a 303 to the page for a failure", got)
	}
	for _, r := range s.seen {
		if r.path == "/auth/cli/token" {
			t.Error("the client asked for a secret with no code")
		}
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("a refused login saved a session")
	}
}

func TestLoginWithOIDCWhenNopsRefusesTheCode(t *testing.T) {
	file := configDir(t)
	s, addr := oidcStub(t)
	s.answers["POST /auth/cli/token"] = answer{400, `{"error":"the login code is unknown, expired, or used: log in again"}`}
	b := &browser{t: t, callback: func(start *url.URL) []string {
		return []string{"code=c&state=" + url.QueryEscape(start.Query().Get("cli_state"))}
	}}
	b.install()

	code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "login")
	if code != 1 || !strings.Contains(errOut, "log in again") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if got := <-b.landed; !strings.HasSuffix(got[0], "/auth/cli/done?failed=1") {
		t.Errorf("browser saw %q, want the page for a failure", got)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("a failed login saved a session")
	}
}

func TestLoginWithOIDCTimesOut(t *testing.T) {
	configDir(t)
	_, addr := oidcStub(t)
	old := openBrowser
	openBrowser = func(string) error { return nil } // nobody comes back
	t.Cleanup(func() { openBrowser = old })
	oldTimeout := loginTimeout
	loginTimeout = 50 * time.Millisecond
	t.Cleanup(func() { loginTimeout = oldTimeout })

	code, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "login")
	if code != 1 || !strings.Contains(errOut, "did not finish the login") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestLoginWithOIDCWhenTheBrowserDoesNotOpen(t *testing.T) {
	configDir(t)
	_, addr := oidcStub(t)
	old := openBrowser
	openBrowser = func(string) error { return os.ErrNotExist }
	t.Cleanup(func() { openBrowser = old })
	oldTimeout := loginTimeout
	loginTimeout = 50 * time.Millisecond
	t.Cleanup(func() { loginTimeout = oldTimeout })

	_, _, errOut := run(t, map[string]string{"NOPS_ADDR": addr}, "", "login")
	if !strings.Contains(errOut, "Could not open the browser") || !strings.Contains(errOut, addr+"/auth/oidc?") {
		t.Errorf("stderr = %q, want to say so and still give the address", errOut)
	}
}

// -- logout -----------------------------------------------------------------------

func TestLogout(t *testing.T) {
	file := configDir(t)
	s, addr := basicStub(t)
	for name, status := range map[string]int{"revoked": 204, "already gone": 401} {
		saveSession(t, addr, time.Now().Add(time.Hour))
		s.answers["DELETE /api/acl/token/self"] = answer{status, `{"error":"the token is not valid"}`}
		code, out, errOut := run(t, map[string]string{"NOPS_ADDR": "http://elsewhere.example.com"}, "", "logout")
		if code != 0 || !strings.Contains(out, "Logged out of "+addr) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, out, errOut)
		}
		if last := s.seen[len(s.seen)-1]; last.method != "DELETE" || last.auth != "Bearer "+sessionSecret {
			t.Errorf("%s: last request = %+v, want the saved session revoking itself", name, last)
		}
		if _, err := os.Stat(file); err == nil {
			t.Errorf("%s: the session is still saved", name)
		}
	}
}

func TestLogoutKeepsTheSessionWhenItCannotRevokeIt(t *testing.T) {
	file := configDir(t)
	s, addr := basicStub(t)
	s.answers["DELETE /api/acl/token/self"] = answer{500, `{"error":"nops hit an error"}`}
	saveSession(t, addr, time.Now().Add(time.Hour))

	code, _, errOut := run(t, nil, "", "logout")
	if code != 1 || !strings.Contains(errOut, "still saved") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if _, err := os.Stat(file); err != nil {
		t.Error("the session was forgotten though Nops did not revoke it")
	}
	noSecret(t, "logout", errOut)
}

func TestLogoutWithNothingToLogOutOf(t *testing.T) {
	configDir(t)
	s, addr := basicStub(t)

	if code, out, _ := run(t, nil, "", "logout"); code != 0 || !strings.Contains(out, "Not logged in") {
		t.Errorf("no session: exit %d, stdout %q", code, out)
	}
	saveSession(t, addr, time.Now().Add(-time.Hour))
	if code, out, _ := run(t, nil, "", "logout"); code != 0 || !strings.Contains(out, "had ended") {
		t.Errorf("an ended session: exit %d, stdout %q", code, out)
	}
	if len(s.seen) != 0 {
		t.Errorf("%d requests for a session that was not there or had ended", len(s.seen))
	}
}
