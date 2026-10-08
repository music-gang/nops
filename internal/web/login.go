package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/secret"
	"github.com/music-gang/nops/internal/store"
)

// The login as the server sees it (docs/dashboard.md#authentication, and
// docs/acl.md#binding-rules). An auth method (auth.go, auth_basic.go) only
// proves who the person is and hands the claims to Done. Everything after
// that is here: the binding rules decide what the login gets, a session token
// is made, and its secret goes in a cookie that Require looks up like any
// token. Logging out revokes it.

const (
	// sessionCookie holds the secret of the session token a login made.
	sessionCookie = "nops_session"
	// tokenCookie holds a token the person pasted on the login page. It has no
	// expiry of its own: the browser forgets it when it closes.
	tokenCookie = "nops_token"

	sessionTTL = 12 * time.Hour

	loginPath  = "/auth/login"
	logoutPath = "/auth/logout"
	tokenPath  = "/auth/token"
)

// Authenticator is an auth method: it checks who a person is and tells the
// server. NewAuth (OpenID Connect) and NewBasicAuth (local users) both
// implement it, and -auth-mode picks exactly one.
type Authenticator interface {
	// Method is the name binding rules give it: "oidc" or "basic".
	Method() string
	// Register adds the method's /auth/* routes to mux. When a person has
	// proved who they are, the method calls host.Done.
	Register(mux *http.ServeMux, host LoginHost)
}

// LoginHost is what an auth method calls back. The server implements it.
type LoginHost interface {
	// Done ends a successful login: it finds what the person gets, makes the
	// session and answers the request.
	Done(w http.ResponseWriter, r *http.Request, p Person, next string)
	// Fail answers a login that did not work with the login page and a message.
	Fail(w http.ResponseWriter, r *http.Request, status int, message, next string)
}

// Person is who an auth method says logged in.
type Person struct {
	// Identity is who the person is for good: the issuer and sub of an OIDC
	// login, or the method and username of a basic one. The username is only
	// what Nops shows.
	Identity string
	// Username is what the records and the pages show.
	Username string
	// Claims are what binding rules select on.
	Claims acl.Claims
}

type userKey struct{}

// UserFrom returns the actor of the request: the person who logged in, or the
// name of the token. It is what Require and the API's token check put in the
// context.
func UserFrom(ctx context.Context) (string, bool) {
	u, ok := ctx.Value(userKey{}).(string)
	return u, ok && u != ""
}

// loginPageData is what the login template needs.
type loginPageData struct {
	Method string
	Error  string
	Next   string
	Base   string // the dashboard's base path (docs/running-nops.md#under-a-sub-path)
}

// loginPage is GET /auth/login: the auth method, and a field for a token.
func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	s.renderLogin(w, http.StatusOK, "", safeNext(r.URL.Query().Get("next")))
}

func (s *server) renderLogin(w http.ResponseWriter, status int, message, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	data := loginPageData{Method: s.auth.Method(), Error: message, Next: next, Base: s.basePath}
	if err := s.tmpl.ExecuteTemplate(w, "login", data); err != nil {
		s.log.Error("render login page", "error", err)
	}
}

// Fail implements LoginHost.
func (s *server) Fail(w http.ResponseWriter, _ *http.Request, status int, message, next string) {
	noStore(w)
	s.renderLogin(w, status, message, next)
}

// Done implements LoginHost.
func (s *server) Done(w http.ResponseWriter, r *http.Request, p Person, next string) {
	noStore(w)
	ctx := r.Context()
	granted := acl.Granted{Management: true} // with the ACL off every login can do everything
	if s.aclOn {
		rules, err := s.access.BindingRules(ctx)
		if err != nil {
			s.serverError(w, r, "read the binding rules", err)
			return
		}
		var bindings []acl.Binding
		for _, b := range rules {
			bindings = append(bindings, acl.Binding{ID: b.ID, AuthMethod: b.AuthMethod, Selector: b.Selector, Type: b.BindType, Name: b.BindName})
		}
		var bindErr error
		if granted, bindErr = acl.Bind(s.auth.Method(), p.Claims, bindings); bindErr != nil {
			s.log.ErrorContext(ctx, "login: a binding rule cannot be evaluated, and binds nothing", "error", bindErr)
		}
		if !granted.Any() {
			s.log.WarnContext(ctx, "login: no binding rule matches", "user", p.Username, "identity", p.Identity)
			http.Error(w, "no binding rule matches your login", http.StatusForbidden)
			return
		}
	}

	if _, err := s.access.DeleteExpiredSessions(ctx); err != nil {
		s.log.WarnContext(ctx, "login: delete the expired sessions", "error", err) // they stay unusable; the next login tries again
	}
	t := store.ACLToken{
		Name: p.Username, Type: store.TokenClient, Policies: granted.Policies, ACL: s.aclOn,
		Origin: store.OriginLogin, Identity: p.Identity, ExpiresAt: s.now().Add(sessionTTL),
		CreatorName: p.Username, CreatorIdentity: p.Identity,
	}
	if granted.Management {
		t.Type, t.Policies = store.TokenManagement, nil
	}
	sec := secret.New()
	t, err := s.access.CreateACLToken(ctx, t, hashToken(sec), store.Audit{Actor: p.Username, Identity: p.Identity})
	if err != nil {
		s.serverError(w, r, "create the session token", err)
		return
	}
	s.clearLoginCookie(w, tokenCookie)
	s.setLoginCookie(w, sessionCookie, sec, sessionTTL)
	s.log.InfoContext(ctx, "login", "user", p.Username, "identity", p.Identity, "accessor_id", t.AccessorID, "type", t.Type)
	http.Redirect(w, r, s.basePath+next, http.StatusFound)
}

// pasteToken is POST /auth/token: log in with a token the person holds, such as
// the bootstrap token or a session token copied from another browser.
func (s *server) pasteToken(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := safeNext(r.PostFormValue("next"))
	pasted := strings.TrimSpace(r.PostFormValue("token"))
	sub, err := s.tokenSubject(r.Context(), pasted)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.log.WarnContext(r.Context(), "login: a pasted token is not valid")
		s.renderLogin(w, http.StatusUnauthorized, "That token is not valid: it is unknown, expired or revoked.", next)
		return
	case err != nil:
		s.serverError(w, r, "check the pasted token", err)
		return
	}
	s.clearLoginCookie(w, sessionCookie)
	s.setLoginCookie(w, tokenCookie, pasted, 0)
	s.log.InfoContext(r.Context(), "login with a token", "actor", sub.actor, "accessor_id", sub.accessorID)
	http.Redirect(w, r, s.basePath+next, http.StatusFound)
}

// logout is POST /auth/logout. A session token is revoked; a pasted token is
// only forgotten by the browser, since it may be the bootstrap token.
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if secret := cookieValue(r, sessionCookie); secret != "" {
		sub, err := s.tokenSubject(r.Context(), secret)
		switch {
		case err == nil && sub.token != nil && sub.token.Origin == store.OriginLogin:
			if err := s.access.RevokeACLToken(r.Context(), sub.accessorID, auditOf(sub)); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.log.ErrorContext(r.Context(), "logout: revoke the session token", "accessor_id", sub.accessorID, "error", err)
			}
		case err != nil && !errors.Is(err, store.ErrNotFound):
			s.log.ErrorContext(r.Context(), "logout: read the session token", "error", err)
		}
	}
	s.clearLoginCookie(w, sessionCookie)
	s.clearLoginCookie(w, tokenCookie)
	http.Redirect(w, r, s.basePath+"/", http.StatusSeeOther)
}

// setLoginCookie keeps a token secret in a cookie. A zero ttl makes a cookie
// the browser forgets when it closes.
func (s *server) setLoginCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: s.basePath + "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *server) clearLoginCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: s.basePath + "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// cookieSubject finds who a dashboard request acts as: the token in one of the
// login cookies. It returns store.ErrNotFound when there is none that works.
func (s *server) cookieSubject(r *http.Request) (*subject, error) {
	for _, name := range []string{tokenCookie, sessionCookie} {
		value := cookieValue(r, name)
		if value == "" {
			continue
		}
		sub, err := s.tokenSubject(r.Context(), value)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if name == sessionCookie {
			sub.sessionSecret = value
		}
		return sub, nil
	}
	return nil, store.ErrNotFound
}

// require lets a request through only with a valid login, and puts who it is in
// its context: the subject, and the actor (UserFrom). Without one, a GET is sent
// to the login and anything else gets 401. An htmx request (every click, form
// and refresh of the dashboard is one) gets a 401 with an HX-Redirect instead:
// htmx loads the login as a full page, where a redirect would be followed by
// the XHR and a 401 swapped nowhere. A request that changes state and comes
// from another origin is refused before anything else
// (http.CrossOriginProtection, with the SameSite=Lax cookie): there is no CSRF
// token to carry through the pages.
func (s *server) require(next http.Handler) http.Handler {
	return s.xorigin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, err := s.cookieSubject(r)
		if errors.Is(err, store.ErrNotFound) {
			if r.Header.Get("HX-Request") == "true" {
				noStore(w)
				w.Header().Set("HX-Redirect", s.basePath+loginPath+"?next="+url.QueryEscape(s.htmxNext(r)))
				http.Error(w, "login required", http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, s.basePath+loginPath+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		if err != nil {
			s.serverError(w, r, "check the login", err)
			return
		}
		ctx := context.WithValue(r.Context(), subjectKey{}, sub)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, userKey{}, sub.actor)))
	}))
}

// htmxNext is where the login brings back an htmx request without a login:
// the page clicked for a boosted link, else the page in the browser (a refresh
// or a form is not worth replaying, and the page shown is where the user is).
// The browser's URL is kept as a path only, through safeNext.
func (s *server) htmxNext(r *http.Request) string {
	if r.Header.Get("HX-Boosted") == "true" && r.Method == http.MethodGet {
		return r.URL.RequestURI()
	}
	cur, err := url.Parse(r.Header.Get("HX-Current-URL"))
	if err != nil {
		return "/"
	}
	path := cur.EscapedPath()
	if s.basePath != "" {
		rest, ok := strings.CutPrefix(path, s.basePath)
		if !ok || (rest != "" && rest[0] != '/') {
			return "/"
		}
		path = rest
	}
	if path == "" {
		path = "/"
	}
	if cur.RawQuery != "" {
		path += "?" + cur.RawQuery
	}
	return safeNext(path)
}
