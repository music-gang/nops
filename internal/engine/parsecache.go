package engine

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/hashicorp/nomad/api"
)

// parseEntry is a cached nomadx.ParseHCL result, success or failure: content
// unchanged means the same result, error included, so a broken file is not
// re-sent to Nomad on every drift tick either.
type parseEntry struct {
	job *api.Job
	err error
}

// parseCacheKey identifies a (content, vars) pair without holding either in
// the map key.
func parseCacheKey(content, vars string) string {
	return hashOf(content) + ":" + hashOf(vars)
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// cachedParse looks up a previous result for (content, vars) in the cache
// snapshot taken at the start of the cycle.
func (e *Engine) cachedParse(prev map[string]parseEntry, key string) (parseEntry, bool) {
	entry, ok := prev[key]
	return entry, ok
}

// snapshotParseCache returns the cache built by the previous cycle, to look
// hits up in, without holding the lock while parsing.
func (e *Engine) snapshotParseCache() map[string]parseEntry {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.parseCache
}

// replaceParseCache swaps in the cache built by the cycle that just ran. It
// entirely replaces the previous one, so an entry for a file no longer in the
// snapshot is dropped: the cache never grows across cycles.
func (e *Engine) replaceParseCache(next map[string]parseEntry) {
	e.mu.Lock()
	e.parseCache = next
	e.mu.Unlock()
}

// replaceObservations swaps in the observations built by the cycle that just
// ran, for the same reason: a job removed from the repo disappears from it.
func (e *Engine) replaceObservations(next map[jobKey]Observation) {
	e.mu.Lock()
	e.observations = next
	e.mu.Unlock()
}
