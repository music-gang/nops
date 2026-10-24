package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// deadlineRecorder is a response writer that records the write deadlines set
// on it, as net/http's own writer takes them.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestDeadlineWriterMovesTheDeadlineAtEachWrite(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := deadlineWriter{w: rec, rc: http.NewResponseController(rec), timeout: time.Minute}
	for range 3 {
		before := time.Now()
		if _, err := w.Write([]byte("block")); err != nil {
			t.Fatal(err)
		}
		last := rec.deadlines[len(rec.deadlines)-1]
		if last.Before(before.Add(time.Minute)) || last.After(time.Now().Add(time.Minute)) {
			t.Errorf("deadline %v, want a minute from the write", last)
		}
	}
	if len(rec.deadlines) != 3 {
		t.Errorf("%d deadlines for 3 writes", len(rec.deadlines))
	}
	if got := rec.Body.String(); got != "blockblockblock" {
		t.Errorf("body = %q", got)
	}
}

func TestDeadlineWriterWithoutTimeoutLeavesTheDeadlineAlone(t *testing.T) {
	// A recorder has no deadlines: a zero timeout must not ask for one.
	rec := httptest.NewRecorder()
	w := deadlineWriter{w: rec, rc: http.NewResponseController(rec)}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
}

// zeros is an endless reader of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// TestBackupDownloadOutlivesTheWriteTimeoutOnlyWhileItMoves runs a real
// server: a download the client reads slowly but steadily is longer than the
// write timeout and still finishes, one the client stops reading is cut.
func TestBackupDownloadOutlivesTheWriteTimeoutOnlyWhileItMoves(t *testing.T) {
	const (
		size    = 32 << 20 // more than the socket buffers hold
		timeout = 500 * time.Millisecond
		// pause is far below timeout: a loaded runner can oversleep, and a
		// pause that reaches the timeout cuts a download that does move.
		pause = 20 * time.Millisecond
	)
	// read takes the body in chunks, waiting pause after each; stallAfter, if
	// positive, is a chunk after which it waits stall instead.
	read := func(t *testing.T, handler http.HandlerFunc, pause time.Duration, stallAfter int, stall time.Duration) int {
		t.Helper()
		ts := httptest.NewUnstartedServer(handler)
		ts.Config.WriteTimeout = timeout
		ts.Start()
		defer ts.Close()
		resp, err := http.Get(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 256<<10)
		total := 0
		for i := 1; ; i++ {
			n, err := io.ReadFull(resp.Body, buf)
			total += n
			if err != nil {
				return total
			}
			if i == stallAfter {
				time.Sleep(stall)
			} else {
				time.Sleep(pause)
			}
		}
	}
	plain := func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeros{}, size)
	}
	moving := func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(deadlineWriter{w: w, rc: http.NewResponseController(w), timeout: timeout}, zeros{}, size)
	}

	t.Run("the server's own timeout cuts a slow download", func(t *testing.T) {
		if got := read(t, plain, pause, 0, 0); got >= size {
			t.Fatalf("got all %d bytes: the download is not longer than the timeout, the test proves nothing", got)
		}
	})
	t.Run("a download that moves finishes", func(t *testing.T) {
		if got := read(t, moving, pause, 0, 0); got != size {
			t.Errorf("got %d bytes, want %d", got, size)
		}
	})
	t.Run("a client that stalls is cut", func(t *testing.T) {
		if got := read(t, moving, 0, 4, 4*timeout); got >= size {
			t.Errorf("got all %d bytes from a client that stopped reading", got)
		}
	})
}
