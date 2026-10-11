package cli

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/term"
)

// nops login and nops logout (docs/cli.md). The server side is internal/web's
// cli_login.go, which has the reasoning for the two flows.

// loginTimeout is how long the browser has to finish an OIDC login: the time
// Nops gives a login in progress. A test shortens it.
var loginTimeout = 10 * time.Minute

// openBrowser opens a URL in the browser of the user. A test replaces it.
var openBrowser = func(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

// loginAnswer is what Nops gives for a login: the secret, once, and when it ends.
type loginAnswer struct {
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
}

func loginFlags(fs *flag.FlagSet, k *call) {
	fs.StringVar(&k.addr, "addr", "", "URL of Nops (NOPS_ADDR)")
}

func runLogin(k *call) error {
	addr, err := resolveAddr(k.addr, k.getenv)
	if err != nil {
		return err
	}
	c := &client{base: addr, http: &http.Client{Timeout: requestTimeout}}

	var info struct {
		Method string `json:"method"`
	}
	if _, err := c.get(k.ctx, "/auth/cli", &info); err != nil {
		return fmt.Errorf("ask how Nops logs in: %w", err)
	}
	var answer loginAnswer
	switch info.Method {
	case "basic":
		answer, err = loginBasic(k, c)
	case "oidc":
		answer, err = loginOIDC(k, c)
	default:
		err = fmt.Errorf("nops logs in with %q, which this client does not know", info.Method)
	}
	if err != nil {
		return err
	}

	// Who it is, as Nops sees it: the name is what tells a person it worked.
	c.token = answer.Secret
	var self aclToken
	who := ""
	if _, err := c.get(k.ctx, "/api/acl/token/self", &self); err == nil && self.Name != "" {
		who = " as " + self.Name
	}
	fmt.Fprintf(k.out, "Logged in to %s%s. The session ends %s.\n", addr, who, stamp(answer.ExpiresAt))
	return nil
}

// loginBasic asks for the username and the password in the terminal.
func loginBasic(k *call, c *client) (loginAnswer, error) {
	username, password, err := promptCredentials(k)
	if err != nil {
		return loginAnswer{}, err
	}
	var a loginAnswer
	raw, err := c.do(k.ctx, http.MethodPost, "/auth/cli/basic", map[string]string{"username": username, "password": password})
	if err != nil {
		return a, err
	}
	if a, err = decodeLogin(raw); err != nil {
		return a, err
	}
	return a, savedSession{Addr: c.base, Secret: a.Secret, ExpiresAt: a.ExpiresAt}.save()
}

// promptCredentials asks on the error output, so a pipe gets only the answer.
// The password is not echoed. With no terminal on the input, the username and
// the password are its first two lines.
func promptCredentials(k *call) (username, password string, err error) {
	fmt.Fprint(k.errOut, "Username: ")
	line, err := k.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(k.errOut)
		return "", "", errors.New("no username: type it, or send the username and the password as two lines")
	}
	username = strings.TrimSpace(line)

	fmt.Fprint(k.errOut, "Password: ")
	if f, ok := k.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		password, err = readPassword(k.ctx, int(f.Fd()))
		fmt.Fprintln(k.errOut)
		if err != nil {
			return "", "", err
		}
	} else {
		line, err := k.in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(k.errOut)
			return "", "", errors.New("no password: send the username and the password as two lines")
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if username == "" || password == "" {
		return "", "", errors.New("the username and the password cannot be empty")
	}
	return username, password, nil
}

// readPassword reads a line without echo. Ctrl-C reaches the client as a
// cancelled context, not as a signal that stops the read, so the terminal is
// put back by hand.
func readPassword(ctx context.Context, fd int) (string, error) {
	type result struct {
		b   []byte
		err error
	}
	old, _ := term.GetState(fd)
	ch := make(chan result, 1)
	go func() {
		b, err := term.ReadPassword(fd)
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("read the password: %w", r.err)
		}
		return string(r.b), nil
	case <-ctx.Done():
		if old != nil {
			term.Restore(fd, old)
		}
		return "", ctx.Err()
	}
}

func decodeLogin(raw []byte) (loginAnswer, error) {
	var a loginAnswer
	if err := json.Unmarshal(raw, &a); err != nil || a.Secret == "" {
		return a, errors.New("the answer of the login is not what Nops sends: is the URL the one of Nops?")
	}
	return a, nil
}

// loginResult is what the browser brings back to the client.
type loginResult struct {
	code  string
	err   error
	reply chan<- string // where to send the browser, once the client has finished
}

// loginOIDC logs in through the browser. Nops does the round trip to the
// provider, and sends the browser back to a listener on this machine with a
// code that only this client can exchange: it holds the verifier.
func loginOIDC(k *call, c *client) (loginAnswer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return loginAnswer{}, fmt.Errorf("listen for the browser: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	state, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier() // two random strings; the first is only checked on the way back

	results := make(chan loginResult, 1)
	handled := make(chan struct{}, 1) // the browser has its answer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "this is not the login the command line started", http.StatusBadRequest)
			return
		}
		reply := make(chan string, 1)
		res := loginResult{code: q.Get("code"), reply: reply}
		switch e := q.Get("error"); {
		case e != "":
			res.err = refusal(e)
		case res.code == "":
			res.err = errors.New("nops sent no login code")
		}
		select {
		case results <- res:
		default:
			http.Error(w, "the login is already finished", http.StatusConflict)
			return
		}
		select {
		case next := <-reply:
			http.Redirect(w, r, next, http.StatusSeeOther)
			http.NewResponseController(w).Flush()
			select {
			case handled <- struct{}{}:
			default:
			}
		case <-r.Context().Done():
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	// Not Shutdown: a browser keeps spare connections open, which it would wait for.
	defer srv.Close()

	start := c.base + "/auth/oidc?" + url.Values{
		"cli_port":      {strconv.Itoa(port)},
		"cli_state":     {state},
		"cli_challenge": {oauth2.S256ChallengeFromVerifier(verifier)},
	}.Encode()
	fmt.Fprintf(k.errOut, "Opening your browser to log in. If it does not open, go to:\n\n  %s\n\n", start)
	if err := openBrowser(start); err != nil {
		fmt.Fprintf(k.errOut, "Could not open the browser (%v): open the address above.\n", err)
	}

	ctx, cancel := context.WithTimeout(k.ctx, loginTimeout)
	defer cancel()
	var res loginResult
	select {
	case res = <-results:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return loginAnswer{}, fmt.Errorf("the browser did not finish the login in %v: run nops login again", loginTimeout)
		}
		return loginAnswer{}, ctx.Err()
	}

	// finish sends the browser to a page of Nops, and waits until it has gone.
	finish := func(page string) {
		res.reply <- page
		select {
		case <-handled:
		case <-time.After(2 * time.Second):
		}
	}
	done, failed := c.base+"/auth/cli/done", c.base+"/auth/cli/done?failed=1"
	if res.err != nil {
		finish(failed)
		return loginAnswer{}, res.err
	}
	raw, err := c.do(ctx, http.MethodPost, "/auth/cli/token", map[string]string{"code": res.code, "verifier": verifier})
	var a loginAnswer
	if err == nil {
		a, err = decodeLogin(raw)
	}
	if err == nil {
		err = savedSession{Addr: c.base, Secret: a.Secret, ExpiresAt: a.ExpiresAt}.save()
	}
	if err != nil {
		finish(failed)
		return loginAnswer{}, err
	}
	finish(done)
	return a, nil
}

// refusal says what a login Nops sent back with an error means. Only the
// codes Nops sends are known: anything else is not repeated.
func refusal(code string) error {
	switch code {
	case "no_binding":
		return errors.New("no binding rule matches your login: ask for one")
	case "server_error":
		return errors.New("nops could not make the session: see its log")
	}
	return errors.New("the login was refused")
}

func runLogout(k *call) error {
	saved, err := loadSession()
	if err != nil {
		return err
	}
	if saved == nil {
		fmt.Fprintln(k.out, "Not logged in: there is no saved session.")
		return nil
	}
	if time.Now().After(saved.ExpiresAt) { // Nops has already deleted it
		fmt.Fprintf(k.out, "The session at %s had ended: forgot it.\n", saved.Addr)
		return forgetSession()
	}
	c := &client{base: saved.Addr, token: saved.Secret, http: &http.Client{Timeout: requestTimeout}}
	_, err = c.do(k.ctx, http.MethodDelete, "/api/acl/token/self", nil)
	var se *statusError
	if err != nil && !(errors.As(err, &se) && se.status == http.StatusUnauthorized) {
		return fmt.Errorf("revoke the session (it is still saved): %w", err)
	}
	if err := forgetSession(); err != nil {
		return err
	}
	fmt.Fprintf(k.out, "Logged out of %s.\n", saved.Addr)
	return nil
}
