package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sha256Hex(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookDisabledWhenNoSecret(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	rec := ts.do("POST", "/webhook/git", strings.NewReader("{}"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 (no route registered without a secret)", rec.Code)
	}
	if ts.trig != 0 {
		t.Error("Trigger must not be called")
	}
}

func TestWebhookGitHub(t *testing.T) {
	const secret = "shared-secret"
	body := `{"ref":"refs/heads/main"}`
	tests := []struct {
		name string
		sig  string
		want int
	}{
		{"valid", "sha256=" + sha256Hex(secret, body), http.StatusAccepted},
		{"wrong secret", "sha256=" + sha256Hex("other", body), http.StatusUnauthorized},
		{"not hex", "sha256=not-hex", http.StatusUnauthorized},
		{"no prefix", sha256Hex(secret, body), http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, secret)
			req := httptest.NewRequest("POST", "/webhook/git", strings.NewReader(body))
			req.Header.Set("X-Hub-Signature-256", tt.sig)
			rec := httptest.NewRecorder()
			ts.h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d", rec.Code, tt.want)
			}
			wantTrig := 0
			if tt.want == http.StatusAccepted {
				wantTrig = 1
			}
			if ts.trig != wantTrig {
				t.Errorf("Trigger called %d times, want %d", ts.trig, wantTrig)
			}
		})
	}
}

func TestWebhookGitea(t *testing.T) {
	const secret = "shared-secret"
	body := `{"ref":"refs/heads/main"}`
	tests := []struct {
		name string
		sig  string
		want int
	}{
		{"valid", sha256Hex(secret, body), http.StatusAccepted},
		{"wrong secret", sha256Hex("other", body), http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, secret)
			req := httptest.NewRequest("POST", "/webhook/git", strings.NewReader(body))
			req.Header.Set("X-Gitea-Signature", tt.sig)
			rec := httptest.NewRecorder()
			ts.h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestWebhookGitLab(t *testing.T) {
	const secret = "shared-secret"
	tests := []struct {
		name  string
		token string
		want  int
	}{
		{"valid", secret, http.StatusAccepted},
		{"wrong token", "not-the-secret", http.StatusUnauthorized},
		{"empty", "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, secret)
			req := httptest.NewRequest("POST", "/webhook/git", strings.NewReader(`{}`))
			if tt.token != "" {
				req.Header.Set("X-Gitlab-Token", tt.token)
			}
			rec := httptest.NewRecorder()
			ts.h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestWebhookNoRecognizedHeader(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "shared-secret")
	rec := ts.do("POST", "/webhook/git", strings.NewReader(`{}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if ts.trig != 0 {
		t.Error("Trigger must not be called")
	}
}

// The webhook needs no session: it is authenticated by its own secret.
func TestWebhookNeedsNoSession(t *testing.T) {
	const secret = "shared-secret"
	body := `{}`
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, secret)
	req := httptest.NewRequest("POST", "/webhook/git", strings.NewReader(body))
	req.Header.Set("X-Gitea-Signature", sha256Hex(secret, body))
	rec := httptest.NewRecorder()
	ts.h.ServeHTTP(rec, req) // no cookie at all
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", rec.Code)
	}
}
