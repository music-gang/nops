package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
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
	// GroupsFile, optional, holds one "group: user1 user2" per line, in
	// Apache's AuthGroupFile format. Every member must be in UsersFile.
	GroupsFile string
	Log        *slog.Logger
}

// BasicAuth is the local-users auth method: no external identity provider, an
// operator-maintained file of usernames and bcrypt hashes instead. It
// implements Authenticator. See docs/dashboard.md#local-users--auth-modebasic.
type BasicAuth struct {
	users   map[string]string   // username -> bcrypt hash
	groups  map[string][]string // username -> sorted groups
	dummy   string              // see timingSafetyPassword
	xorigin *http.CrossOriginProtection
	host    LoginHost
	log     *slog.Logger
}

// NewBasicAuth creates the local-users login. The users file and the groups
// file are read once, like every other nops secret: a bad path, a malformed
// line, a hash that does not parse as bcrypt or a group member that is not a
// user is a startup error naming the line.
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
	var groups map[string][]string
	if o.GroupsFile != "" {
		if groups, err = parseGroupsFile(o.GroupsFile, users); err != nil {
			return nil, fmt.Errorf("web: %w", err)
		}
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte(timingSafetyPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &BasicAuth{users: users, groups: groups, dummy: string(dummy), xorigin: http.NewCrossOriginProtection(), log: log}, nil
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

// parseGroupsFile reads "group: user1 user2" lines and returns the groups of
// each user, sorted. A group may repeat, its members adding up, as in Apache.
// A member that is not in users is an error: a stale line would otherwise
// give a new user with an old name the group's access without a word.
func parseGroupsFile(path string, users map[string]string) (map[string][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	groups := make(map[string][]string)
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		group, rest, ok := strings.Cut(line, ":")
		group = strings.TrimSpace(group)
		members := strings.Fields(rest)
		if !ok || group == "" || len(members) == 0 {
			return nil, fmt.Errorf(`%s:%d: expected "group: user1 user2"`, path, n)
		}
		for _, m := range members {
			if _, known := users[m]; !known {
				return nil, fmt.Errorf("%s:%d: %s: member of %s is not in the users file", path, n, m, group)
			}
			groups[m] = append(groups[m], group)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for u, g := range groups {
		slices.Sort(g)
		groups[u] = slices.Compact(g)
	}
	return groups, nil
}

// Method implements Authenticator.
func (b *BasicAuth) Method() string { return "basic" }

// Register adds the local-users route to mux: POST /auth/basic, which the form
// of the login page posts to.
func (b *BasicAuth) Register(mux *http.ServeMux, host LoginHost) {
	b.host = host
	mux.Handle("POST "+basicPath, b.xorigin.Handler(http.HandlerFunc(b.login)))
	mux.Handle("POST "+cliBasicPath, b.xorigin.Handler(http.HandlerFunc(b.cliLogin)))
}

// check compares the credentials with the users file and, on success, returns
// the person with the groups of the groups file as the claims.
func (b *BasicAuth) check(ctx context.Context, username, password string) (Person, bool) {
	hash, known := b.users[username]
	if !known {
		hash = b.dummy // compare against something anyway: see timingSafetyPassword
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || !known {
		b.log.WarnContext(ctx, "login: invalid credentials", "user", username)
		return Person{}, false
	}
	return Person{
		Identity: "basic:" + username,
		Username: username,
		Claims:   acl.Claims{Username: username, Groups: b.groups[username]},
	}, true
}

// login handles the form of the login page.
func (b *BasicAuth) login(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := safeNext(r.PostFormValue("next"))
	p, ok := b.check(r.Context(), r.PostFormValue("username"), r.PostFormValue("password"))
	if !ok {
		b.host.Fail(w, r, http.StatusUnauthorized, "Invalid username or password.", next)
		return
	}
	b.host.Done(w, r, p, next)
}

// cliLogin is POST /auth/cli/basic: the same check for the command line, with a
// JSON body and a JSON answer (cli_login.go).
func (b *BasicAuth) cliLogin(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readBody(w, r, &body) {
		return
	}
	p, ok := b.check(r.Context(), body.Username, body.Password)
	if !ok {
		apiError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	b.host.CLIToken(w, r, p)
}
