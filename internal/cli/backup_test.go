package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupSavesTheFilePrivately(t *testing.T) {
	s, addr := newStub(t, map[string]answer{"GET /api/backup": {200, "SQLite format 3\x00 and the rest"}})
	dir := t.TempDir()
	target := filepath.Join(dir, "nops.db")

	code, out, errOut := run(t, envFor(addr), "", "backup", target)
	if code != 0 || out != "saved the backup to "+target+"\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got, _ := os.ReadFile(target); string(got) != "SQLite format 3\x00 and the rest" {
		t.Errorf("file = %q, want the body of the answer", got)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600: the backup holds secrets", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("files in the directory = %v, want only the backup", entries)
	}
	if got := s.seen[0]; got.method != "GET" || got.auth != "Bearer "+token {
		t.Errorf("request = %+v, want a GET with the token", got)
	}
}

func TestBackupRefusedLeavesTheFileAsItWas(t *testing.T) {
	_, addr := newStub(t, map[string]answer{"GET /api/backup": {403, `{"error":"this token does not allow that"}`}})
	dir := t.TempDir()
	target := filepath.Join(dir, "nops.db")
	if err := os.WriteFile(target, []byte("last week"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := run(t, envFor(addr), "", "backup", target)
	if code != 1 || !strings.Contains(errOut, "this token does not allow that") {
		t.Errorf("exit %d, stderr %q, want 1 and the message of the API", code, errOut)
	}
	if got, _ := os.ReadFile(target); string(got) != "last week" {
		t.Errorf("file = %q, want the old backup untouched", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("files in the directory = %v, want no temporary file", entries)
	}
}

func TestBackupReplacesAnOldFile(t *testing.T) {
	_, addr := newStub(t, map[string]answer{"GET /api/backup": {200, "new"}})
	target := filepath.Join(t.TempDir(), "nops.db")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, envFor(addr), "", "backup", target); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Errorf("file = %q, want new", got)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 even over a file that was looser", fi.Mode().Perm())
	}
}

func TestBackupNeedsOneFile(t *testing.T) {
	_, addr := newStub(t, nil)
	if code, _, _ := run(t, envFor(addr), "", "backup"); code != 2 {
		t.Errorf("exit %d without a file, want 2", code)
	}
}
