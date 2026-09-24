// Package web serves the dashboard and the git webhook. This file is the
// OpenID Connect login backend; session.go holds what every login backend
// shares (the cookie, Require, logout), auth_basic.go the local-users
// backend. The rules are in docs/dashboard.md#authentication.
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
)

const (
	loginTTL     = 10 * time.Minute
	loginCookie  = "nops_login"
	callbackPath = "/auth/callback"
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

// Auth is the OIDC login of the dashboard. It never contacts the provider
// before the first login: an unreachable provider must not stop the engine,
// so discovery is tried at every login until it works. It implements
// Authenticator; *session gives it Require and Require's Register share
// (logout) for free.
type Auth struct {
	*session
	opts   AuthOptions
	client *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
}

// NewAuth creates the OIDC login.
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
	sess, err := newSession(redirect.Scheme == "https", o.Log)
	if err != nil {
		return nil, err
	}
	a := &Auth{session: sess, opts: o, client: o.HTTPClient}
	if a.client == nil {
		a.client = &http.Client{Timeout: 10 * time.Second}
	}
	return a, nil
}

// Register adds the OIDC login routes to mux, on top of the shared ones
// (POST /auth/logout).
func (a *Auth) Register(mux *http.ServeMux) {
	a.session.Register(mux)
	mux.HandleFunc("GET "+loginPath, a.login)
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

	if !a.start(w, who.actor()) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.log.InfoContext(ctx, "login", "user", who.actor())
	http.Redirect(w, r, st.Next, http.StatusFound)
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
