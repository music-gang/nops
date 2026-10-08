package web

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/music-gang/nops/internal/acl"
)

// basicPath is where the login page posts a username and a password.
const basicPath = "/auth/basic"

// timingSafetyPassword is hashed once at startup (NewBasicAuth) to give
// login a hash to compare an unknown username against: bcrypt itself is the
// slow part, so always running one compare, real hash or this one, keeps a
// wrong username and a wrong password indistinguishable by timing.
const timingSafetyPassword = "nops-unknown-user-timing-safety"

// BasicAuthOptions configures NewBasicAuth.
type BasicAuthOptions struct {
	// UsersFile holds one "username:bcrypt-hash" per line (blank lines and
	// lines starting with "#" are ignored). Generate a line with, for
	// example, `htpasswd -nB <user>` (docs/dashboard.md#local-users--auth-modebasic).
	UsersFile string
	Log       *slog.Logger
}

// BasicAuth is the local-users auth method: no external identity provider, an
// operator-maintained file of usernames and bcrypt hashes instead. It
// implements Authenticator. See docs/dashboard.md#local-users--auth-modebasic.
type BasicAuth struct {
	users   map[string]string // username -> bcrypt hash
	dummy   string            // see timingSafetyPassword
	xorigin *http.CrossOriginProtection
	host    LoginHost
	log     *slog.Logger
}

// NewBasicAuth creates the local-users login. The users file is read once,
// like every other nops secret: a bad path, a malformed line or a hash that
// does not parse as bcrypt is a startup error naming the line.
func NewBasicAuth(o BasicAuthOptions) (*BasicAuth, error) {
	if o.UsersFile == "" {
		return nil, errors.New("web: UsersFile is required")
	}
	users, err := parseUsersFile(o.UsersFile)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("web: %s holds no user", o.UsersFile)
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte(timingSafetyPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &BasicAuth{users: users, dummy: string(dummy), xorigin: http.NewCrossOriginProtection(), log: log}, nil
}

// parseUsersFile reads "username:bcrypt-hash" lines. A username may not
// repeat: two lines for the same user is very likely a mistake (a leftover
// old line after rotating a password), not intentional, so it is rejected
// rather than silently keeping the last one.
func parseUsersFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	users := make(map[string]string)
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, hash, ok := strings.Cut(line, ":")
		if !ok || name == "" || hash == "" {
			return nil, fmt.Errorf(`%s:%d: expected "username:bcrypt-hash"`, path, n)
		}
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			return nil, fmt.Errorf("%s:%d: %s: not a bcrypt hash: %w", path, n, name, err)
		}
		if _, dup := users[name]; dup {
			return nil, fmt.Errorf("%s:%d: %s: duplicate user", path, n, name)
		}
		users[name] = hash
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return users, nil
}

// Method implements Authenticator.
func (b *BasicAuth) Method() string { return "basic" }

// Register adds the local-users route to mux: POST /auth/basic, which the form
// of the login page posts to.
func (b *BasicAuth) Register(mux *http.ServeMux, host LoginHost) {
	b.host = host
	mux.Handle("POST "+basicPath, b.xorigin.Handler(http.HandlerFunc(b.login)))
}

// login checks the submitted credentials and, on success, hands the username to
// the server as the only claim there is.
func (b *BasicAuth) login(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))

	hash, known := b.users[username]
	if !known {
		hash = b.dummy // compare against something anyway: see timingSafetyPassword
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || !known {
		b.log.WarnContext(r.Context(), "login: invalid credentials", "user", username)
		b.host.Fail(w, r, http.StatusUnauthorized, "Invalid username or password.", next)
		return
	}
	b.host.Done(w, r, Person{
		Identity: "basic:" + username,
		Username: username,
		Claims:   acl.Claims{Username: username},
	}, next)
}
