package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// maxTokenName is the longest name a token takes, in bytes.
const maxTokenName = 100

// tokenPrefix starts every token, so a secret scanner and a person can tell
// one from any other string.
const tokenPrefix = "nops_"

// expiryChoice is one answer to "when does it expire" on the Tokens page: a
// fixed list, so there is nothing to validate beyond membership. A zero Days
// is a token that never expires.
type expiryChoice struct {
	Key, Label string
	Days       int
}

var expiryChoices = []expiryChoice{
	{"7", "7 days", 7},
	{"30", "30 days", 30},
	{"90", "90 days", 90},
	{"365", "365 days", 365},
	{"never", "Never", 0},
}

// defaultExpiry is the choice the form starts on.
const defaultExpiry = "90"

// newToken makes the secret of a token: 256 random bits, which is why a plain
// SHA-256 of it is enough to keep (hashToken).
func newToken() string { return tokenPrefix + randomToken() }

// hashToken is what the store keeps of a token: nobody who reads the
// database can use it to call the API.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type tokenRow struct {
	ID, Name, Owner string
	Created         timeView
	Expires         timeView // zero: never
	Expired         bool
}

// createdToken is the token just made: the one time its secret is shown.
type createdToken struct {
	Name, Secret string
}

type tokensData struct {
	baseData
	Tokens  []tokenRow
	Expiry  []expiryChoice
	Default string
	Created *createdToken
}

func (s *server) tokensView(r *http.Request, created *createdToken) (tokensData, error) {
	list, err := s.tokens.Tokens(r.Context())
	if err != nil {
		return tokensData{}, err
	}
	data := tokensData{baseData: s.base(r, "tokens"), Expiry: expiryChoices, Default: defaultExpiry, Created: created}
	now := s.now()
	for _, t := range list {
		data.Tokens = append(data.Tokens, tokenRow{
			ID: t.ID, Name: t.Name, Owner: t.Owner,
			Created: s.when(t.CreatedAt), Expires: s.until(t.ExpiresAt),
			Expired: !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt),
		})
	}
	return data, nil
}

// tokensPage is GET /tokens: every token, and the form to make yours.
func (s *server) tokensPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.tokensView(r, nil)
	if err != nil {
		s.serverError(w, r, "list tokens", err)
		return
	}
	s.render(w, r, "tokens", data)
}

// createToken is POST /tokens: make a token for whoever is logged in and answer
// with the page that shows its secret, the only time it is shown: it is not
// kept, so it is not a redirect that could be repeated.
func (s *server) createToken(w http.ResponseWriter, r *http.Request) {
	owner, ok := UserFrom(r.Context())
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > maxTokenName {
		s.badRequest(w, r, "Name the token", "A token needs a name of "+strconv.Itoa(maxTokenName)+" bytes at most, so you can tell it from the others.")
		return
	}
	var expires time.Time
	choice, found := expiryChoice{}, false
	for _, c := range expiryChoices {
		if c.Key == r.FormValue("expires") {
			choice, found = c, true
		}
	}
	if !found {
		s.badRequest(w, r, "Choose an expiry", "The expiry is one of 7, 30, 90 or 365 days, or never.")
		return
	}
	if choice.Days > 0 {
		expires = s.now().AddDate(0, 0, choice.Days)
	}
	secret := newToken()
	if _, err := s.tokens.CreateToken(r.Context(), owner, name, hashToken(secret), expires); err != nil {
		s.serverError(w, r, "create token", err)
		return
	}
	s.log.InfoContext(r.Context(), "api token created", "owner", owner, "name", name, "expires", choice.Label)
	data, err := s.tokensView(r, &createdToken{Name: name, Secret: secret})
	if err != nil {
		s.serverError(w, r, "list tokens", err)
		return
	}
	s.render(w, r, "tokens", data)
}

// revokeToken is POST /tokens/{id}/revoke: any logged-in user can revoke any
// token (docs/api.md#tokens).
func (s *server) revokeToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := UserFrom(r.Context())
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	err := s.tokens.RevokeToken(r.Context(), r.PathValue("id"))
	switch {
	case err == nil:
		s.log.InfoContext(r.Context(), "api token revoked", "token_id", r.PathValue("id"), "by", actor)
		http.Redirect(w, r, s.basePath+"/tokens", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFoundMessage(w, r, "This token does not exist: someone revoked it already.")
	default:
		s.serverError(w, r, "revoke token", err)
	}
}

// badRequest renders the 400 page of a form that was not filled in right.
func (s *server) badRequest(w http.ResponseWriter, r *http.Request, title, message string) {
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "error", errorData{
		baseData: s.base(r, ""),
		Status:   http.StatusBadRequest,
		Title:    title,
		Message:  message,
	})
}
