package web

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/securecookie"
)

// session is what the login backends share: the cookie that carries the state
// of a login in progress (OpenID Connect's round trip to the provider), signed
// and encrypted. The session of a logged-in person is not here: it is a token
// that login.go makes and looks up.
type session struct {
	cookie *securecookie.SecureCookie
	secure bool   // cookies are Secure: the public URL is https
	base   string // the dashboard's base path (docs/running-nops.md#under-a-sub-path), "" at the domain root
	now    func() time.Time
	log    *slog.Logger
}

// newSession creates the cookie mechanism. The key that signs and encrypts the
// cookies is random and lives only in this process: a login in progress does
// not survive a restart.
func newSession(secure bool, base string, log *slog.Logger) (*session, error) {
	hash, block := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(hash); err != nil {
		return nil, fmt.Errorf("web: session key: %w", err)
	}
	if _, err := rand.Read(block); err != nil {
		return nil, fmt.Errorf("web: session key: %w", err)
	}
	sc := securecookie.New(hash, block)
	sc.SetSerializer(securecookie.JSONEncoder{})
	sc.MaxAge(0) // the expiry is checked by the caller, with its own clock

	if log == nil {
		log = slog.Default()
	}
	return &session{cookie: sc, secure: secure, base: base, now: time.Now, log: log}, nil
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
