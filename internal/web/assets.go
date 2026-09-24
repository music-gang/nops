package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sync"
)

// Static files are served with a day-long cache (noStoreExempt), so a changed
// file must have a new address or a browser keeps the old one with the new
// pages around it. The templates ask for a file through the "asset" function,
// which appends a version taken from the file's own bytes: it changes exactly
// when the file does, and never otherwise, so the cache stays useful.

// assetVersion is the first 10 hex digits of the SHA-256 of a file of fsys.
func assetVersion(fsys fs.FS, name string) (string, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", fmt.Errorf("asset %s: %w", name, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:10], nil
}

var (
	assetMu       sync.Mutex
	assetVersions = map[string]string{}
)

// asset is the address of a static file with its version: /static/app.css?v=….
// The versions are computed once per file, since the files are embedded and
// cannot change while the process runs. A file that does not exist is an
// error, so the page fails loud (a logged 500) instead of linking a 404.
func asset(name string) (string, error) {
	assetMu.Lock()
	defer assetMu.Unlock()
	v, ok := assetVersions[name]
	if !ok {
		sub, err := fs.Sub(staticFiles, "static")
		if err != nil {
			return "", fmt.Errorf("asset %s: %w", name, err)
		}
		if v, err = assetVersion(sub, name); err != nil {
			return "", err
		}
		assetVersions[name] = v
	}
	return "/static/" + name + "?v=" + v, nil
}
