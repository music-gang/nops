package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/store"
)

const sqliteMagic = "SQLite format 3\x00"

func TestAPIBackupNeedsTheBackupCapability(t *testing.T) {
	s := newACLServer(t, &fakeStore{}, &fakeEngine{})

	for name, rules := range map[string]string{
		"read":      `namespace "*" { policy = "write" }`,
		"fetch":     `capabilities = ["fetch"]`,
		"pause all": `capabilities = ["pause"]`,
	} {
		if rec := s.api("GET", "/api/backup", s.clientWith(rules), ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
	}
	if changes, _ := s.tokens.ACLChanges(t.Context(), 10); hasBackup(changes) {
		t.Error("a refused request was logged as a backup")
	}

	for name, tok := range map[string]string{
		"the capability": s.clientWith(`capabilities = ["backup"]`),
		"management":     s.boot,
	} {
		rec := s.api("GET", "/api/backup", tok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200: %s", name, rec.Code, rec.Body)
		}
		if !strings.HasPrefix(rec.Body.String(), sqliteMagic) {
			t.Errorf("%s: the body is not a SQLite file", name)
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="nops-`) || !strings.HasSuffix(cd, `.db"`) {
			t.Errorf("%s: Content-Disposition = %q", name, cd)
		}
	}
}

func TestBackupIsInTheAuditWithTheTokenUsed(t *testing.T) {
	s := newACLServer(t, &fakeStore{}, &fakeEngine{})
	tok := s.clientWith(`capabilities = ["backup"]`)
	if rec := s.api("GET", "/api/backup", tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	rec := s.api("GET", "/api/acl/changes", s.boot, "")
	var out struct {
		Changes []apiChange `json:"changes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var found *apiChange
	for i, c := range out.Changes {
		if c.Action == "backup" {
			found = &out.Changes[i]
		}
	}
	if found == nil || found.Kind != "database" || found.AccessorID == "" || found.Actor != "client" {
		t.Errorf("changes = %+v, want a backup by the token used", out.Changes)
	}
}

func hasBackup(changes []store.ACLChange) bool {
	for _, c := range changes {
		if c.Action == "backup" {
			return true
		}
	}
	return false
}

func TestDashboardBackup(t *testing.T) {
	t.Run("with the capability", func(t *testing.T) {
		ts := dashboardAs(t, &fakeStore{}, &fakeEngine{}, `capabilities = ["backup"]`)
		if rec := ts.page("GET", "/admin", nil); !strings.Contains(rec.Body.String(), `href="/admin/backup"`) {
			t.Error("the Administration menu has no Backup")
		}
		rec := ts.page("GET", "/admin/backup", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Download backup") {
			t.Fatalf("GET: status %d, want the page with its button", rec.Code)
		}
		// The pages are boosted by default: a boosted form would swap the file in as text.
		if !strings.Contains(rec.Body.String(), `action="/admin/backup" hx-boost="false"`) {
			t.Error("the form of the button is boosted: the browser would show the file instead of saving it")
		}
		rec = ts.page("POST", "/admin/backup", nil)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), sqliteMagic) {
			t.Fatalf("POST: status %d, want the SQLite file", rec.Code)
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
			t.Errorf("Content-Disposition = %q, want an attachment", cd)
		}
	})

	t.Run("a management token", func(t *testing.T) {
		ts := dashboardAs(t, &fakeStore{}, &fakeEngine{}, "")
		if rec := ts.page("GET", "/admin", nil); !strings.Contains(rec.Body.String(), `href="/admin/backup"`) {
			t.Error("the Administration menu has no Backup")
		}
	})

	t.Run("without the capability", func(t *testing.T) {
		ts := dashboardAs(t, &fakeStore{}, &fakeEngine{}, `namespace "*" { policy = "write" }`)
		if rec := ts.page("GET", "/admin", nil); strings.Contains(rec.Body.String(), "/admin/backup") {
			t.Error("the Administration menu shows Backup")
		}
		for _, m := range []string{"GET", "POST"} {
			if rec := ts.page(m, "/admin/backup", nil); rec.Code != http.StatusForbidden {
				t.Errorf("%s: status %d, want 403", m, rec.Code)
			}
		}
	})

	t.Run("cross origin", func(t *testing.T) {
		ts := dashboardAs(t, &fakeStore{}, &fakeEngine{}, "")
		req := httptest.NewRequest("POST", "/admin/backup", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(ts.login)
		rec := httptest.NewRecorder()
		ts.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status %d, want 403", rec.Code)
		}
	})
}
