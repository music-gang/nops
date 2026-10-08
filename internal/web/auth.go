// Package web serves the dashboard and the git webhook. This file is the
// OpenID Connect auth method; login.go holds what every auth method shares
// (the session token, Require, logout), auth_basic.go the local-users
// method. The rules are in docs/dashboard.md#authentication.
package web

import (
	"context"
	"crypto/rand"
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
	"golang.org/x/oauth2"

	"github.com/music-gang/nops/internal/acl"
)

const (
	loginTTL     = 10 * time.Minute
	loginCookie  = "nops_login"
	callbackPath = "/auth/callback"
	startPath    = "/auth/oidc"
)

// AuthOptions configures NewAuth.
type AuthOptions struct {
	// Issuer, ClientID and ClientSecret identify nops at the OIDC provider.
	Issuer, ClientID, ClientSecret string
	// RedirectURL is <public-url>/auth/callback.
	RedirectURL string
	// HTTPClient talks to the provider. Nil: a client with a 10 second timeout.
	HTTPClient *http.Client
	// BasePath is the dashboard's base path (docs/running-nops.md#under-a-sub-path),
	// "" at the domain root. It is not part of RedirectURL: the caller
	// already builds that from the full public URL, which carries any base
	// path as its own path component.
	BasePath string
	Log      *slog.Logger
}

// Auth is the OIDC login of the dashboard. It never contacts the provider
// before the first login: an unreachable provider must not stop the engine,
// so discovery is tried at every login until it works. It implements
// Authenticator.
type Auth struct {
	*session
	opts   AuthOptions
	client *http.Client
	host   LoginHost

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
}

// NewAuth creates the OIDC login.
func NewAuth(o AuthOptions) (*Auth, error) {
	if o.Issuer == "" || o.ClientID == "" || o.ClientSecret == "" {
		return nil, errors.New("web: the OIDC issuer, client ID and client secret are required")
	}
	redirect, err := url.Parse(o.RedirectURL)
	if err != nil || redirect.Host == "" {
		return nil, fmt.Errorf("web: invalid redirect URL %q", o.RedirectURL)
	}
	sess, err := newSession(redirect.Scheme == "https", o.BasePath, o.Log)
	if err != nil {
		return nil, err
	}
	a := &Auth{session: sess, opts: o, client: o.HTTPClient}
	if a.client == nil {
		a.client = &http.Client{Timeout: 10 * time.Second}
	}
	return a, nil
}

// Method implements Authenticator.
func (a *Auth) Method() string { return "oidc" }

// Register adds the OIDC routes to mux: GET /auth/oidc sends the person to the
// provider, and the provider sends them back to GET /auth/callback.
func (a *Auth) Register(mux *http.ServeMux, host LoginHost) {
	a.host = host
	mux.HandleFunc("GET "+startPath, a.start)
	mux.HandleFunc("GET "+callbackPath, a.callback)
}

// loginState is what survives the round trip to the provider.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
}

func (a *Auth) start(w http.ResponseWriter, r *http.Request) {
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
	if !a.setCookie(w, loginCookie, a.base+"/auth", st, loginTTL) {
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
	a.clearCookie(w, loginCookie, a.base+"/auth") // single use, whatever happens next
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
	a.host.Done(w, r, Person{
		// The issuer and sub are what the provider never reassigns: the username is only what Nops shows.
		Identity: "oidc:" + a.opts.Issuer + "#" + who.Subject,
		Username: who.actor(),
		Claims:   acl.Claims{Username: who.actor(), Sub: who.Subject, Groups: who.Groups},
	}, st.Next)
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
	return &oauth2.Config{
		ClientID:     a.opts.ClientID,
		ClientSecret: a.opts.ClientSecret,
		Endpoint:     a.provider.Endpoint(),
		RedirectURL:  a.opts.RedirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	}, a.provider, a.verifier, nil
}

// user is what the provider says about the person who logged in.
type user struct {
	Subject           string
	PreferredUsername string
	Email             string
	Groups            []string
}

// actor is the name recorded with a decision, and the username a selector reads.
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

// identify reads the claims of the ID token and, when the groups or a name are
// not in it, those of the userinfo endpoint (Authelia, for one, keeps most
// claims out of the ID token). A userinfo answer for another subject is an
// error.
func (a *Auth) identify(ctx context.Context, p *oidc.Provider, idToken *oidc.IDToken, token *oauth2.Token) (user, error) {
	u := user{Subject: idToken.Subject}
	var c claims
	if err := idToken.Claims(&c); err != nil {
		return u, fmt.Errorf("decode ID token claims: %w", err)
	}
	c.merge(&u)

	needGroups := len(u.Groups) == 0
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

// safeNext keeps the redirect after a login on this site: a path, never a URL.
//
// Browsers drop tabs and newlines from a URL before parsing it, and read a
// backslash as a slash, so "/<TAB>/evil.example.com" and "/\evil.example.com"
// would both leave the site as "//evil.example.com". Every control character
// and every backslash is therefore refused, not only the ones at the start.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	for _, r := range next {
		if r < ' ' || r == 0x7f || r == '\\' {
			return "/"
		}
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" {
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
