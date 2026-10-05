package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var secretInPage = regexp.MustCompile(`nops_[A-Za-z0-9_-]{20,}`)

func TestTokensPageNeedsALogin(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	if rec := ts.do("GET", "/tokens", nil); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), loginPath) {
		t.Errorf("GET /tokens without a session: status %d, Location %q, want a redirect to the login", rec.Code, rec.Header().Get("Location"))
	}
	for _, target := range []string{"/tokens", "/tokens/x/revoke"} {
		if rec := ts.do("POST", target, formBody(url.Values{"name": {"x"}, "expires": {"7"}})); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without a session: status %d, want 401", target, rec.Code)
		}
	}
}

// Everyone who logs in sees every token, whoever made it.
func TestTokensPageListsEveryonesTokens(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	_, err := ts.tokens.CreateToken(t.Context(), "bob", "bobs-script", "h1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.tokens.CreateToken(t.Context(), "alice", "old-one", "h2", testNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	page := ts.get("/tokens")
	mustContain(t, page, "bobs-script", "bob", "old-one", "Expired", "never", `href="/tokens"`)
	mustNotContain(t, page, "Token “")
}

func TestCreateTokenShowsTheSecretOnceAndKeepsItsHash(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	cookie := mintSession(t, ts.auth, "alice")

	rec := ts.do("POST", "/tokens", formBody(url.Values{"name": {"ci"}, "expires": {"30"}}), cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	secret := secretInPage.FindString(rec.Body.String())
	if secret == "" {
		t.Fatal("the page does not show the new token")
	}
	mustContain(t, rec.Body.String(), "ci", "alice")

	// It works, as alice, and what the store holds is its hash.
	if owner, err := ts.tokens.TokenOwner(t.Context(), hashToken(secret)); err != nil || owner != "alice" {
		t.Errorf("TokenOwner = %q, %v, want alice", owner, err)
	}
	if rec := ts.api("GET", "/api/jobs", secret, ""); rec.Code != http.StatusOK {
		t.Errorf("the new token on /api/jobs: status %d, want 200", rec.Code)
	}
	list, err := ts.tokens.Tokens(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("Tokens = %+v, %v, want the one token", list, err)
	}
	if want := testNow.AddDate(0, 0, 30); !list[0].ExpiresAt.Equal(want) {
		t.Errorf("expires at %v, want %v: 30 days from now", list[0].ExpiresAt, want)
	}

	// Never again: not on the list, not in the logs.
	if again := secretInPage.FindString(ts.get("/tokens")); again != "" {
		t.Errorf("the list shows a secret: %q", again)
	}
	if strings.Contains(ts.logs.String(), secret) {
		t.Error("the secret is in the logs")
	}
}

func TestCreateTokenExpiry(t *testing.T) {
	for key, wantDays := range map[string]int{"7": 7, "30": 30, "90": 90, "365": 365, "never": 0} {
		ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
		rec := ts.do("POST", "/tokens", formBody(url.Values{"name": {"x"}, "expires": {key}}), mintSession(t, ts.auth, "alice"))
		if rec.Code != http.StatusOK {
			t.Errorf("expires=%s: status %d, want 200", key, rec.Code)
			continue
		}
		list, err := ts.tokens.Tokens(t.Context())
		if err != nil || len(list) != 1 {
			t.Fatalf("expires=%s: Tokens = %+v, %v", key, list, err)
		}
		if wantDays == 0 {
			if !list[0].ExpiresAt.IsZero() {
				t.Errorf("expires=never: expires at %v, want never", list[0].ExpiresAt)
			}
		} else if want := testNow.AddDate(0, 0, wantDays); !list[0].ExpiresAt.Equal(want) {
			t.Errorf("expires=%s: expires at %v, want %v", key, list[0].ExpiresAt, want)
		}
	}
}

func TestCreateTokenRefusesABadForm(t *testing.T) {
	for name, form := range map[string]url.Values{
		"no name":        {"expires": {"30"}},
		"blank name":     {"name": {"   "}, "expires": {"30"}},
		"long name":      {"name": {strings.Repeat("a", maxTokenName+1)}, "expires": {"30"}},
		"no expiry":      {"name": {"ci"}},
		"unlisted":       {"name": {"ci"}, "expires": {"3650"}},
		"negative":       {"name": {"ci"}, "expires": {"-1"}},
		"empty expiry":   {"name": {"ci"}, "expires": {""}},
		"never, in caps": {"name": {"ci"}, "expires": {"NEVER"}},
	} {
		ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
		rec := ts.do("POST", "/tokens", formBody(form), mintSession(t, ts.auth, "alice"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
		if list, _ := ts.tokens.Tokens(t.Context()); len(list) != 0 {
			t.Errorf("%s: a token was made: %+v", name, list)
		}
	}
}

// Any logged-in user can revoke any token, and it stops working at once.
func TestRevokeToken(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	secret, tok := makeToken(t, ts, "bob", time.Time{})
	if rec := ts.api("GET", "/api/jobs", secret, ""); rec.Code != http.StatusOK {
		t.Fatalf("before the revoke: status %d, want 200", rec.Code)
	}

	rec := ts.do("POST", "/tokens/"+tok.ID+"/revoke", nil, mintSession(t, ts.auth, "alice"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/tokens" {
		t.Fatalf("revoke: status %d, Location %q, want 303 to /tokens", rec.Code, rec.Header().Get("Location"))
	}
	if rec := ts.api("GET", "/api/jobs", secret, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("after the revoke: status %d, want 401", rec.Code)
	}
	if rec := ts.do("POST", "/tokens/"+tok.ID+"/revoke", nil, mintSession(t, ts.auth, "alice")); rec.Code != http.StatusNotFound {
		t.Errorf("revoking it again: status %d, want 404", rec.Code)
	}
}

func TestTokenWritesRefuseCrossOrigin(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	_, tok := makeToken(t, ts, "bob", time.Time{})
	for _, target := range []string{"/tokens", "/tokens/" + tok.ID + "/revoke"} {
		req := httptest.NewRequest("POST", target, formBody(url.Values{"name": {"x"}, "expires": {"7"}}))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(mintSession(t, ts.auth, "alice"))
		rec := httptest.NewRecorder()
		ts.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s: status %d, want 403", target, rec.Code)
		}
	}
	if list, _ := ts.tokens.Tokens(t.Context()); len(list) != 1 {
		t.Errorf("Tokens = %+v, want only bob's, untouched", list)
	}
}
