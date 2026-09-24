package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/securecookie"
)

// session is the cookie-based login mechanism shared by every
// authentication backend: today OpenID Connect (auth.go), and local users
// (auth_basic.go). A signed and encrypted cookie holds a sessionData, a
// http.CrossOriginProtection wrapper guards every write, and logout is
// identical whichever backend issued the session. A backend only decides
// *how* a request earns one (OIDC's callback, a username and password form)
// and calls start; everything else — Require, the actor in the request
// context, the cookie itself — lives here once.
type session struct {
	cookie  *securecookie.SecureCookie
	secure  bool // cookies are Secure: the public URL is https
	xorigin *http.CrossOriginProtection
	now     func() time.Time
	log     *slog.Logger
}

const (
	sessionCookie = "nops_session"
	sessionTTL    = 12 * time.Hour

	loginPath  = "/auth/login"
	logoutPath = "/auth/logout"
)

type sessionData struct {
	User    string `json:"u"`
	Expires int64  `json:"e"`
}

// newSession creates the login's session mechanism. The key that signs and
// encrypts the cookies is random and lives only in this process: a restart
// logs everybody out, whichever backend they logged in with.
func newSession(secure bool, log *slog.Logger) (*session, error) {
	hash, block := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(hash); err != nil {
		return nil, fmt.Errorf("web: session key: %w", err)
	}
	if _, err := rand.Read(block); err != nil {
		return nil, fmt.Errorf("web: session key: %w", err)
	}
	sc := securecookie.New(hash, block)
	sc.SetSerializer(securecookie.JSONEncoder{})
	sc.MaxAge(0) // the expiry is checked by session, with its own clock

	if log == nil {
		log = slog.Default()
	}
	return &session{
		cookie:  sc,
		secure:  secure,
		xorigin: http.NewCrossOriginProtection(),
		now:     time.Now,
		log:     log,
	}, nil
}

// Register adds the routes every backend shares: POST /auth/logout. Each
// backend's own Register calls this and adds its own login route(s) on top.
func (s *session) Register(mux *http.ServeMux) {
	mux.Handle("POST "+logoutPath, s.xorigin.Handler(http.HandlerFunc(s.logout)))
}

type userKey struct{}

// UserFrom returns the logged-in user set by Require: the actor to record
// with a decision.
func UserFrom(ctx context.Context) (string, bool) {
	u, ok := ctx.Value(userKey{}).(string)
	return u, ok && u != ""
}

// Require lets a request through only with a valid session, and puts the user
// in its context. Without one, a GET is sent to the login and anything else
// gets 401. A request that changes state and comes from another origin is
// refused before anything else (http.CrossOriginProtection, with the
// SameSite=Lax cookie): there is no CSRF token to carry through the pages.
func (s *session) Require(next http.Handler) http.Handler {
	return s.xorigin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.currentUser(r)
		if !ok {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, loginPath+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	}))
}

// start issues a fresh session cookie for actor: the last step of a
// successful login, whichever backend performed it.
func (s *session) start(w http.ResponseWriter, actor string) bool {
	sess := sessionData{User: actor, Expires: s.now().Add(sessionTTL).Unix()}
	return s.setCookie(w, sessionCookie, "/", sess, sessionTTL)
}

func (s *session) logout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	s.clearCookie(w, sessionCookie, "/")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// currentUser reads the user of a valid, unexpired session cookie.
func (s *session) currentUser(r *http.Request) (string, bool) {
	var sd sessionData
	if err := s.cookie.Decode(sessionCookie, cookieValue(r, sessionCookie), &sd); err != nil {
		return "", false
	}
	if sd.User == "" || s.now().Unix() > sd.Expires {
		return "", false
	}
	return sd.User, true
}

func (s *session) setCookie(w http.ResponseWriter, name, path string, v any, ttl time.Duration) bool {
	enc, err := s.cookie.Encode(name, v)
	if err != nil {
		s.log.Error("encode cookie", "cookie", name, "error", err)
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: enc, Path: path, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	return true
}

func (s *session) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }
