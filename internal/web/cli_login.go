package web

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// The login of the command line (docs/cli.md, nops login). It makes the same
// session as a dashboard login, but hands the secret to the client instead of
// a cookie:
//
//   - basic: the client posts the username and password to cliBasicPath
//     (auth_basic.go) and gets the secret back.
//   - OIDC: the client opens the browser at /auth/oidc with the port of a
//     listener on its loopback, a state and the PKCE challenge of a verifier
//     only it knows. After the usual round trip to the provider, the browser
//     goes back to that listener with a one-time code, which the client
//     exchanges for the secret at cliTokenPath with the verifier. The secret
//     is never in a URL, and a code seen by anyone else is worthless without
//     the verifier.
//
// None of these routes takes a token: they are how a client gets one.

const (
	cliInfoPath  = "/auth/cli"
	cliBasicPath = "/auth/cli/basic"
	cliTokenPath = "/auth/cli/token"
	cliDonePath  = "/auth/cli/done"

	// cliCodeTTL is how long a client has to exchange its code: it comes back
	// from the browser at once.
	cliCodeTTL = time.Minute

	// cliLoopback is the only host the browser is sent back to.
	cliLoopback = "127.0.0.1"
)

// cliLogin is what the client asks for when it starts a login in the browser.
// It rides in the login cookie across the provider's round trip.
type cliLogin struct {
	Port      int    `json:"p"`
	State     string `json:"s"`
	Challenge string `json:"c"`
}

// parseCLILogin reads the cli_* parameters of /auth/oidc. It returns nil when
// the request has none, and an error when it has some but not all, or a bad one.
func parseCLILogin(q url.Values) (*cliLogin, error) {
	if !q.Has("cli_port") && !q.Has("cli_state") && !q.Has("cli_challenge") {
		return nil, nil
	}
	port, err := strconv.Atoi(q.Get("cli_port"))
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("cli_port is not a port")
	}
	c := &cliLogin{Port: port, State: q.Get("cli_state"), Challenge: q.Get("cli_challenge")}
	if !urlSafe(c.State, 16, 128) {
		return nil, errors.New("cli_state is not 16 to 128 URL-safe characters")
	}
	// An S256 challenge is the 32 bytes of a SHA-256 in unpadded base64url.
	if !urlSafe(c.Challenge, 43, 43) {
		return nil, errors.New("cli_challenge is not an S256 challenge")
	}
	return c, nil
}

// urlSafe reports whether s is min to max characters of the base64url alphabet.
func urlSafe(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// cliSession is what a client gets for a login: the secret, shown once, and when it ends.
type cliSession struct {
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
}

// cliCode is a session waiting for its client to ask for it.
type cliCode struct {
	session   cliSession
	challenge string
	expires   time.Time
}

// cliCodes holds the codes in memory: one that a restart loses only makes the
// client log in again. The zero value is ready.
type cliCodes struct {
	mu    sync.Mutex
	codes map[string]cliCode
}

// put stores a code, and forgets the ones that have expired.
func (c *cliCodes) put(code string, v cliCode, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.codes == nil {
		c.codes = make(map[string]cliCode)
	}
	for k, old := range c.codes {
		if now.After(old.expires) {
			delete(c.codes, k)
		}
	}
	c.codes[code] = v
}

// take removes a code and returns it. A code works once, whether or not what
// the client sends with it is right.
func (c *cliCodes) take(code string, now time.Time) (cliCode, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.codes[code]
	delete(c.codes, code)
	return v, ok && !now.After(v.expires)
}

// CLIToken implements LoginHost.
func (s *server) CLIToken(w http.ResponseWriter, r *http.Request, p Person) {
	noStore(w)
	sec, t, err := s.newLoginSession(r.Context(), p)
	switch {
	case errors.Is(err, errNoBinding):
		apiError(w, http.StatusForbidden, err.Error())
		return
	case err != nil:
		s.apiServerError(w, r, "log in", err)
		return
	}
	s.log.InfoContext(r.Context(), "login from the command line", "user", p.Username, "identity", p.Identity, "accessor_id", t.AccessorID, "type", t.Type)
	replyJSON(w, http.StatusOK, cliSession{Secret: sec, ExpiresAt: t.ExpiresAt})
}

// CLIRedirect implements LoginHost. A login that no rule allows is sent back
// too, with an error and no code, so the client says so at once.
func (s *server) CLIRedirect(w http.ResponseWriter, r *http.Request, p Person, c cliLogin) {
	noStore(w)
	back := url.Values{"state": {c.State}}
	sec, t, err := s.newLoginSession(r.Context(), p)
	switch {
	case errors.Is(err, errNoBinding):
		back.Set("error", "no_binding")
	case err != nil:
		s.log.ErrorContext(r.Context(), "dashboard error", "action", "log in from the command line", "error", err)
		back.Set("error", "server_error")
	default:
		code := randomToken()
		now := s.now()
		s.cliCodes.put(code, cliCode{
			session:   cliSession{Secret: sec, ExpiresAt: t.ExpiresAt},
			challenge: c.Challenge,
			expires:   now.Add(cliCodeTTL),
		}, now)
		back.Set("code", code)
		s.log.InfoContext(r.Context(), "login from the command line", "user", p.Username, "identity", p.Identity, "accessor_id", t.AccessorID, "type", t.Type)
	}
	loopback := url.URL{Scheme: "http", Host: net.JoinHostPort(cliLoopback, strconv.Itoa(c.Port)), Path: "/callback", RawQuery: back.Encode()}
	http.Redirect(w, r, loopback.String(), http.StatusSeeOther)
}

// cliInfo is GET /auth/cli: the auth method, which tells the client how to log in.
func (s *server) cliInfo(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	replyJSON(w, http.StatusOK, map[string]string{"method": s.auth.Method()})
}

// cliToken is POST /auth/cli/token: the secret of a session, for the code the
// browser brought to the client and the verifier of its challenge.
func (s *server) cliToken(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var body struct {
		Code     string `json:"code"`
		Verifier string `json:"verifier"`
	}
	if !readBody(w, r, &body) {
		return
	}
	v, ok := s.cliCodes.take(body.Code, s.now())
	if !ok || !equal(oauth2.S256ChallengeFromVerifier(body.Verifier), v.challenge) {
		s.log.WarnContext(r.Context(), "login from the command line: a code is not valid")
		apiError(w, http.StatusBadRequest, "the login code is unknown, expired, or used: log in again")
		return
	}
	replyJSON(w, http.StatusOK, v.session)
}

// cliDoneData is what the page at the end of a login in the browser needs.
type cliDoneData struct {
	Base   string
	Failed bool
}

// cliDone is GET /auth/cli/done: where the client sends the browser when it has
// finished, so the last page is Nops's and not the client's. It shows nothing
// from the request but whether it failed, and the client says why in the terminal.
func (s *server) cliDone(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := cliDoneData{Base: s.basePath, Failed: r.URL.Query().Get("failed") == "1"}
	if err := s.tmpl.ExecuteTemplate(w, "cli_done", data); err != nil {
		s.log.Error("render the page at the end of a command-line login", "error", err)
	}
}

// registerCLILogin adds the routes of the command-line login that do not
// depend on the auth method.
func (s *server) registerCLILogin(mux Router) {
	mux.HandleFunc("GET "+cliInfoPath, s.cliInfo)
	mux.Handle("POST "+cliTokenPath, s.xorigin.Handler(http.HandlerFunc(s.cliToken)))
	mux.HandleFunc("GET "+cliDonePath, s.cliDone)
}
