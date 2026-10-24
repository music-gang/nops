package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Backup logs the backup with the ACL changes and returns a consistent copy of
// the database: a SQLite file ready to use, with no WAL beside it. The copy is
// already unlinked, so the caller reads it, closes it, and nothing stays on
// disk. It holds every job spec unredacted and the ACL, so it is kept at
// dbFileMode.
//
// The log comes first: if it fails there is no backup (state is persisted
// before acting).
func (s *Store) Backup(ctx context.Context, by Audit) (*os.File, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("backup: begin: %w", err)
	}
	if err := s.insertChange(ctx, tx, by, "backup", "database", ""); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("backup: commit: %w", err)
	}

	// CreateTemp makes the file at 0600, and VACUUM INTO accepts an empty one.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".backup-*")
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	name := tmp.Name()
	tmp.Close()
	// Once open, the file outlives its name: on Linux and macOS the data goes
	// with the last close.
	defer os.Remove(name)
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", name); err != nil {
		return nil, fmt.Errorf("backup: copy the database: %w", err)
	}
	f, err := os.Open(name) // #nosec G304 -- name is the temporary file made above
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	return f, nil
}
