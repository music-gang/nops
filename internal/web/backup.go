package web

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// sendBackup answers with a backup of the database as a file to save. It
// returns an error only while nothing is sent yet; once the body starts, a
// broken copy is logged and the connection ends short, which the client sees
// as an incomplete download.
func (s *server) sendBackup(w http.ResponseWriter, r *http.Request) error {
	f, err := s.access.Backup(r.Context(), auditOf(subjectOf(r.Context())))
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/vnd.sqlite3")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "nops-"+time.Now().UTC().Format("20060102T150405Z")+".db"))
	h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if _, err := io.Copy(deadlineWriter{w: w, rc: http.NewResponseController(w), timeout: s.writeWait}, f); err != nil {
		s.log.ErrorContext(r.Context(), "send a backup", "error", err)
	}
	return nil
}

// deadlineWriter moves the write deadline of the connection forward by timeout
// before each write: the server's write timeout then cuts a client that
// stalls, not a download that keeps moving. A zero timeout leaves the deadline
// to the server.
type deadlineWriter struct {
	w       io.Writer
	rc      *http.ResponseController
	timeout time.Duration
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	if d.timeout > 0 {
		if err := d.rc.SetWriteDeadline(time.Now().Add(d.timeout)); err != nil {
			return 0, err
		}
	}
	return d.w.Write(p)
}

// apiBackup is GET /api/backup.
func (s *server) apiBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.sendBackup(w, r); err != nil {
		s.apiServerError(w, r, "take a backup", err)
	}
}

type adminBackupData struct {
	adminBase
}

// backupPage is GET /admin/backup.
func (s *server) backupPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "admin_backup", adminBackupData{adminBase: s.adminBase(r, "backup")})
}

// backupDownload is POST /admin/backup: the button of the page. The answer is
// the file, so the browser saves it and the page stays where it is.
func (s *server) backupDownload(w http.ResponseWriter, r *http.Request) {
	if err := s.sendBackup(w, r); err != nil {
		s.serverError(w, r, "take a backup", err)
	}
}
