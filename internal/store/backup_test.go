package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupIsAWorkingDatabaseWithItsOwnAuditRow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openAt(t, filepath.Join(dir, "nops.db"))
	d := mustCreate(t, s, newDep("api"))
	if err := s.PutACLPolicy(ctx, ACLPolicy{Name: "ops", Rules: `namespace "*" { policy = "read" }`}, admin); err != nil {
		t.Fatal(err)
	}
	backer := Audit{AccessorID: "acc-backup", Actor: "cron"}

	f, err := s.Backup(ctx, backer)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	defer f.Close()
	if fi, _ := f.Stat(); fi.Mode().Perm() != dbFileMode {
		t.Errorf("mode = %v, want %v: the copy holds the specs unredacted", fi.Mode().Perm(), os.FileMode(dbFileMode))
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.backup-*")); len(left) != 0 {
		t.Errorf("files left beside the database: %v", left)
	}

	copyPath := filepath.Join(t.TempDir(), "restored.db")
	out, err := os.OpenFile(copyPath, os.O_CREATE|os.O_WRONLY, dbFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, f); err != nil {
		t.Fatal(err)
	}
	out.Close()
	if entries, _ := os.ReadDir(filepath.Dir(copyPath)); len(entries) != 1 {
		t.Errorf("the restored copy has files beside it: %v", entries)
	}

	r, err := Open(copyPath)
	if err != nil {
		t.Fatalf("Open the backup: %v", err)
	}
	defer r.Close()
	if got, err := r.GetDeployment(ctx, d.ID); err != nil || got.JobSpec != d.JobSpec {
		t.Errorf("deployment in the backup = %+v, %v, want the one saved", got, err)
	}
	if _, err := r.ACLPolicy(ctx, "ops"); err != nil {
		t.Errorf("ACL policy in the backup: %v", err)
	}
	changes, err := r.ACLChanges(ctx, 10)
	if err != nil || len(changes) == 0 {
		t.Fatalf("ACLChanges = %v, %v", changes, err)
	}
	if c := changes[0]; c.Action != "backup" || c.Kind != "database" || c.AccessorID != "acc-backup" || c.Actor != "cron" {
		t.Errorf("latest change = %+v, want the backup by acc-backup", c)
	}
}

func TestBackupTakesNoCopyWhenItCannotLogIt(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, filepath.Join(dir, "nops.db"))
	s.db.Close()
	if f, err := s.Backup(context.Background(), admin); err == nil {
		f.Close()
		t.Fatal("Backup on a closed database succeeded")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.backup-*")); len(left) != 0 {
		t.Errorf("files left after a failure: %v", left)
	}
}
