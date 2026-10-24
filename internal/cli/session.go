package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The session nops login saves (docs/cli.md): the URL it is for, its secret and
// when it ends. One file holds one session, in the user's configuration
// directory, readable by that user only.

// userConfigDir is where the session is kept. A test points it elsewhere.
var userConfigDir = os.UserConfigDir

type savedSession struct {
	Addr      string    `json:"addr"`
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
}

func sessionFile() (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the configuration directory of your user: %w", err)
	}
	return filepath.Join(dir, "nops", "session"), nil
}

// loadSession returns the saved session, or nil when there is none. A file that
// is there and not a session is an error: it names the file.
func loadSession() (*savedSession, error) {
	path, err := sessionFile()
	if err != nil {
		return nil, nil // nowhere to keep one, so none was saved
	}
	b, err := os.ReadFile(path) // #nosec G304 -- sessionFile is a fixed place in the user's config directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the saved session: %w", err)
	}
	var s savedSession
	if err := json.Unmarshal(b, &s); err != nil || s.Addr == "" || s.Secret == "" {
		return nil, fmt.Errorf("the saved session %s is not a session: run nops login again", path)
	}
	return &s, nil
}

// save writes the session through a temporary file, so a crash never leaves half of one.
func (s savedSession) save() error {
	path, err := sessionFile()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save the session: %w", err)
	}
	b, err := json.Marshal(s) // #nosec G117 -- the file is this command line's own session, kept at mode 0600
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "session-*") // mode 0600
	if err != nil {
		return fmt.Errorf("save the session: %w", err)
	}
	defer os.Remove(tmp.Name()) // gone after the rename; cleans up after a failure
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("save the session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save the session: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("save the session: %w", err)
	}
	return nil
}

// forgetSession deletes the saved session, if there is one.
func forgetSession() error {
	path, err := sessionFile()
	if err != nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("forget the session: %w", err)
	}
	return nil
}
