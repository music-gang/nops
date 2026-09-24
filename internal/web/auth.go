// Package web serves the dashboard and the git webhook. This file is the
// login: OpenID Connect against the provider set in the configuration, a
// signed session cookie, and the middleware that puts the logged-in user, the
// actor of a decision, in the request context. The rules are in
// docs/dashboard.md#authentication.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/securecookie"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "nops_session"
	loginCookie   = "nops_login"

	sessionTTL = 12 * time.Hour
	loginTTL   = 10 * time.Minute

	loginPath    = "/auth/login"
	callbackPath = "/auth/callback"
	logoutPath   = "/auth/logout"
)

// AuthOptions configures NewAuth.
type AuthOptions struct {
	// Issuer, ClientID and ClientSecret identify nops at the OIDC provider.
	Issuer, ClientID, ClientSecret string
	// RedirectURL is <public-url>/auth/callback.
	RedirectURL string
	// AllowedUsers match the preferred_username or the email of the user,
	// AllowedGroups the values of its groups claim. A user must match one.
	AllowedUsers, AllowedGroups []string
	// HTTPClient talks to the provider. Nil: a client with a 10 second timeout.
	HTTPClient *http.Client
	Log        *slog.Logger
}

// Auth is the login of the dashboard. It never contacts the provider before
// the first login: an unreachable provider must not stop the engine, so
// discovery is tried at every login until it works.
type Auth struct {
	opts    AuthOptions
	client  *http.Client
	log     *slog.Logger
	cookie  *securecookie.SecureCookie
	secure  bool // cookies are Secure: the public URL is https
	xorigin *http.CrossOriginProtection
	now     func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
}

// NewAuth creates the login. The key that signs and encrypts the cookies is
// random and lives only in this process: a restart logs everybody out.
func NewAuth(o AuthOptions) (*Auth, error) {
	if o.Issuer == "" || o.ClientID == "" || o.ClientSecret == "" {
		return nil, errors.New("web: the OIDC issuer, client ID and client secret are required")
	}
	if len(o.AllowedUsers) == 0 && len(o.AllowedGroups) == 0 {
		return nil, errors.New("web: an allowlist of users or groups is required")
	}
	redirect, err := url.Parse(o.RedirectURL)
	if err != nil || redirect.Host == "" {
		return nil, fmt.Errorf("web: invalid redirect URL %q", o.RedirectURL)
	}
	hash, block := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(hash); err != nil {
		return nil, fmt.Errorf("web: session key: %w", err)
	}
	if _, err := rand.Read(block); err != nil {
		return nil, fmt.Errorf("web: session key: %w", err)
	}
	sc := securecookie.New(hash, block)
	sc.SetSerializer(securecookie.JSONEncoder{})
	sc.MaxAge(0) // the expiry is checked by Auth, with its own clock

	a := &Auth{
		opts:    o,
		client:  o.HTTPClient,
		log:     o.Log,
		cookie:  sc,
		secure:  redirect.Scheme == "https",
		xorigin: http.NewCrossOriginProtection(),
		now:     time.Now,
	}
	if a.client == nil {
		a.client = &http.Client{Timeout: 10 * time.Second}
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a, nil
}

// Register adds the login routes to mux.
func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+loginPath, a.login)
	mux.HandleFunc("GET "+callbackPath, a.callback)
	mux.Handle("POST "+logoutPath, a.xorigin.Handler(http.HandlerFunc(a.logout)))
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
func (a *Auth) Require(next http.Handler) http.Handler {
	return a.xorigin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := a.session(r)
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

// loginState is what survives the round trip to the provider.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
}

type sessionData struct {
	User    string `json:"u"`
	Expires int64  `json:"e"`
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	cfg, _, _, err := a.discover(r.Context())
	if err != nil {
		a.log.ErrorContext(r.Context(), "login: identity provider unavailable", "issuer", a.opts.Issuer, "error", err)
		http.Error(w, "login is unavailable: the identity provider cannot be reached", http.StatusServiceUnavailable)
		return
	}
	st := loginState{
		State:    randomToken(),
		Nonce:    randomToken(),
		Verifier: oauth2.GenerateVerifier(),
		Next:     safeNext(r.URL.Query().Get("next")),
		Expires:  a.now().Add(loginTTL).Unix(),
	}
	if !a.setCookie(w, loginCookie, "/auth", st, loginTTL) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, cfg.AuthCodeURL(st.State, oauth2.S256ChallengeOption(st.Verifier), oidc.Nonce(st.Nonce)), http.StatusFound)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	ctx := r.Context()
	var st loginState
	err := a.cookie.Decode(loginCookie, cookieValue(r, loginCookie), &st)
	a.clearCookie(w, loginCookie, "/auth") // single use, whatever happens next
	if err != nil || a.now().Unix() > st.Expires {
		http.Error(w, "login expired: start again", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		a.log.WarnContext(ctx, "login: the provider refused", "error", e, "description", q.Get("error_description"))
		http.Error(w, "login refused by the identity provider", http.StatusForbidden)
		return
	}
	if !equal(q.Get("state"), st.State) || q.Get("code") == "" {
		http.Error(w, "invalid login response", http.StatusBadRequest)
		return
	}

	cfg, provider, verifier, err := a.discover(ctx)
	if err != nil {
		a.log.ErrorContext(ctx, "login: identity provider unavailable", "issuer", a.opts.Issuer, "error", err)
		http.Error(w, "login is unavailable: the identity provider cannot be reached", http.StatusServiceUnavailable)
		return
	}
	ctx = oidc.ClientContext(ctx, a.client)
	token, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		a.log.ErrorContext(ctx, "login: code exchange failed", "error", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		a.log.ErrorContext(ctx, "login: the provider returned no ID token")
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	idToken, err := verifier.Verify(ctx, raw)
	if err != nil {
		a.log.ErrorContext(ctx, "login: invalid ID token", "error", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	if !equal(idToken.Nonce, st.Nonce) {
		a.log.ErrorContext(ctx, "login: ID token nonce mismatch")
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}

	who, err := a.identify(ctx, provider, idToken, token)
	if err != nil {
		a.log.ErrorContext(ctx, "login: reading the user's claims", "error", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	if !a.allowed(who) {
		a.log.WarnContext(ctx, "login: user not in the allowlist", "user", who.actor())
		http.Error(w, "you are not allowed to use nops", http.StatusForbidden)
		return
	}

	sess := sessionData{User: who.actor(), Expires: a.now().Add(sessionTTL).Unix()}
	if !a.setCookie(w, sessionCookie, "/", sess, sessionTTL) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.log.InfoContext(ctx, "login", "user", sess.User)
	http.Redirect(w, r, st.Next, http.StatusFound)
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	a.clearCookie(w, sessionCookie, "/")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// session reads the user of a valid, unexpired session cookie.
func (a *Auth) session(r *http.Request) (string, bool) {
	var s sessionData
	if err := a.cookie.Decode(sessionCookie, cookieValue(r, sessionCookie), &s); err != nil {
		return "", false
	}
	if s.User == "" || a.now().Unix() > s.Expires {
		return "", false
	}
	return s.User, true
}

// discover returns the OAuth2 configuration and the verifier, contacting the
// provider only until it has answered once.
func (a *Auth) discover(ctx context.Context) (*oauth2.Config, *oidc.Provider, *oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider == nil {
		p, err := oidc.NewProvider(oidc.ClientContext(ctx, a.client), a.opts.Issuer)
		if err != nil {
			return nil, nil, nil, err
		}
		a.provider = p
		a.verifier = p.Verifier(&oidc.Config{ClientID: a.opts.ClientID, Now: a.now})
	}
	scopes := []string{oidc.ScopeOpenID, "profile", "email"}
	if len(a.opts.AllowedGroups) > 0 {
		scopes = append(scopes, "groups")
	}
	return &oauth2.Config{
		ClientID:     a.opts.ClientID,
		ClientSecret: a.opts.ClientSecret,
		Endpoint:     a.provider.Endpoint(),
		RedirectURL:  a.opts.RedirectURL,
		Scopes:       scopes,
	}, a.provider, a.verifier, nil
}

// user is what the provider says about the person who logged in.
type user struct {
	Subject           string
	PreferredUsername string
	Email             string
	Groups            []string
}

// actor is the name recorded with a decision.
func (u user) actor() string {
	switch {
	case u.PreferredUsername != "":
		return u.PreferredUsername
	case u.Email != "":
		return u.Email
	}
	return u.Subject
}

type claims struct {
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	EmailVerified     *bool    `json:"email_verified"`
	Groups            []string `json:"groups"`
}

func (c claims) merge(into *user) {
	if into.PreferredUsername == "" {
		into.PreferredUsername = c.PreferredUsername
	}
	// An email the provider says is not verified is not an identity.
	if into.Email == "" && (c.EmailVerified == nil || *c.EmailVerified) {
		into.Email = c.Email
	}
	if len(into.Groups) == 0 {
		into.Groups = c.Groups
	}
}

// identify reads the claims of the ID token and, when what the allowlist
// needs is not in it, those of the userinfo endpoint (Authelia, for one, keeps
// most claims out of the ID token). A userinfo answer for another subject is
// an error.
func (a *Auth) identify(ctx context.Context, p *oidc.Provider, idToken *oidc.IDToken, token *oauth2.Token) (user, error) {
	u := user{Subject: idToken.Subject}
	var c claims
	if err := idToken.Claims(&c); err != nil {
		return u, fmt.Errorf("decode ID token claims: %w", err)
	}
	c.merge(&u)

	needGroups := len(a.opts.AllowedGroups) > 0 && len(u.Groups) == 0
	needName := u.PreferredUsername == "" && u.Email == ""
	if (needGroups || needName) && p.UserInfoEndpoint() != "" {
		info, err := p.UserInfo(oidc.ClientContext(ctx, a.client), oauth2.StaticTokenSource(token))
		if err != nil {
			return u, fmt.Errorf("userinfo: %w", err)
		}
		if info.Subject != idToken.Subject {
			return u, errors.New("userinfo is about another subject than the ID token")
		}
		var ic claims
		if err := info.Claims(&ic); err != nil {
			return u, fmt.Errorf("decode userinfo claims: %w", err)
		}
		ic.merge(&u)
	}
	return u, nil
}

func (a *Auth) allowed(u user) bool {
	for _, want := range a.opts.AllowedUsers {
		if (u.PreferredUsername != "" && strings.EqualFold(u.PreferredUsername, want)) ||
			(u.Email != "" && strings.EqualFold(u.Email, want)) {
			return true
		}
	}
	for _, want := range a.opts.AllowedGroups {
		for _, g := range u.Groups {
			if g == want {
				return true
			}
		}
	}
	return false
}

func (a *Auth) setCookie(w http.ResponseWriter, name, path string, v any, ttl time.Duration) bool {
	enc, err := a.cookie.Encode(name, v)
	if err != nil {
		a.log.Error("encode cookie", "cookie", name, "error", err)
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: enc, Path: path, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	return true
}

func (a *Auth) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// safeNext keeps the redirect after a login on this site: a path, never a URL.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("web: crypto/rand: %v", err)) // the platform has no randomness: nothing safe to do
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }
