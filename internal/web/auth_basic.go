package web

import (
	"bufio"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// timingSafetyPassword is hashed once at startup (NewBasicAuth) to give
// login a hash to compare an unknown username against: bcrypt itself is the
// slow part, so always running one compare, real hash or this one, keeps a
// wrong username and a wrong password indistinguishable by timing.
const timingSafetyPassword = "nops-unknown-user-timing-safety"

// BasicAuthOptions configures NewBasicAuth.
type BasicAuthOptions struct {
	// UsersFile holds one "username:bcrypt-hash" per line (blank lines and
	// lines starting with "#" are ignored). Generate a line with, for
	// example, `htpasswd -nB <user>` (docs/dashboard.md#local-users).
	UsersFile string
	// PublicURL decides whether cookies are Secure, like OIDC's RedirectURL.
	PublicURL string
	Log       *slog.Logger
}

// BasicAuth is the local-users login: no external identity provider, an
// operator-maintained file of usernames and bcrypt hashes instead. It
// implements Authenticator; *session gives it Require and its share of
// Register (logout) for free. See docs/dashboard.md#local-users.
type BasicAuth struct {
	*session
	users map[string]string // username -> bcrypt hash
	dummy string            // see timingSafetyPassword
	tmpl  *template.Template
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
	u, err := url.Parse(o.PublicURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("web: invalid public URL %q", o.PublicURL)
	}
	sess, err := newSession(u.Scheme == "https", o.Log)
	if err != nil {
		return nil, err
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte(timingSafetyPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	return &BasicAuth{session: sess, users: users, dummy: string(dummy), tmpl: tmpl}, nil
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

// Register adds the local-users login routes to mux, on top of the shared
// ones (POST /auth/logout).
func (b *BasicAuth) Register(mux *http.ServeMux) {
	b.session.Register(mux)
	mux.HandleFunc("GET "+loginPath, b.loginForm)
	mux.Handle("POST "+loginPath, b.xorigin.Handler(http.HandlerFunc(b.login)))
}

// loginPageData is what the login_basic template needs.
type loginPageData struct {
	Error string
	Next  string
}

func (b *BasicAuth) loginForm(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	b.renderLogin(w, "", safeNext(r.URL.Query().Get("next")))
}

func (b *BasicAuth) renderLogin(w http.ResponseWriter, errMsg, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := b.tmpl.ExecuteTemplate(w, "login_basic", loginPageData{Error: errMsg, Next: next}); err != nil {
		b.log.Error("render login page", "error", err)
	}
}

// login checks the submitted credentials and, on success, starts a session
// exactly like the OIDC callback does: the actor is the username as is, no
// claims to map.
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
		b.renderLogin(w, "Invalid username or password.", next)
		return
	}
	if !b.start(w, username) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	b.log.InfoContext(r.Context(), "login", "user", username)
	http.Redirect(w, r, next, http.StatusFound)
}
