package web

import (
	"container/list"
	"crypto/sha256"
	"fmt"
	"math"
	"sync"
	"time"
)

// failureLimit counts the failed logins of each username, the ones that do not
// exist included, so the limit does not tell them apart. max failures lock a
// username until window has passed since its first one; a successful login
// clears the count. The counters live in memory: Nops is one instance, and a
// restart resets them.
//
// The counters are bounded. An entry is born only from a failed password
// comparison, and loginSlots caps those at a few per second, so the entries
// are no more than that rate times window, whatever usernames are tried. Each
// one is dropped when its window ends.
//
// A nil *failureLimit is a limit that is off: every method is a no-op.
type failureLimit struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	now     func() time.Time
	entries map[[sha256.Size]byte]*failure
	// order holds the entries by the end of their window. The window is fixed,
	// so that is the order they were born in, and the expired ones are always
	// at the front.
	order *list.List
}

type failure struct {
	key   [sha256.Size]byte
	count int
	until time.Time
	elem  *list.Element
}

// newFailureLimit returns nil, the limit that is off, when max or window is 0.
func newFailureLimit(max int, window time.Duration) *failureLimit {
	if max <= 0 || window <= 0 {
		return nil
	}
	return &failureLimit{
		max: max, window: window, now: time.Now,
		entries: make(map[[sha256.Size]byte]*failure),
		order:   list.New(),
	}
}

// expire drops the entries whose window has ended. Its caller holds mu.
func (l *failureLimit) expire(now time.Time) {
	for e := l.order.Front(); e != nil; e = l.order.Front() {
		f := e.Value.(*failure)
		if f.until.After(now) {
			return
		}
		l.drop(f)
	}
}

func (l *failureLimit) drop(f *failure) {
	l.order.Remove(f.elem)
	delete(l.entries, f.key)
}

// locked tells whether username is locked and, if so, for how long.
func (l *failureLimit) locked(username string) (time.Duration, bool) {
	if l == nil {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.expire(now)
	f := l.entries[sha256.Sum256([]byte(username))]
	if f == nil || f.count < l.max {
		return 0, false
	}
	return f.until.Sub(now), true
}

// fail counts a failed login of username. It returns true when this one
// locked it, with the time it stays locked.
func (l *failureLimit) fail(username string) (time.Duration, bool) {
	if l == nil {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.expire(now)
	key := sha256.Sum256([]byte(username))
	f := l.entries[key]
	if f == nil {
		f = &failure{key: key, until: now.Add(l.window)}
		f.elem = l.order.PushBack(f)
		l.entries[key] = f
	}
	f.count++
	return f.until.Sub(now), f.count == l.max
}

// clear forgets the failures of username, after a successful login.
func (l *failureLimit) clear(username string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if f := l.entries[sha256.Sum256([]byte(username))]; f != nil {
		l.drop(f)
	}
}

// lockedError is a login refused because its username is locked.
type lockedError struct{ retryAfter time.Duration }

func (e *lockedError) Error() string { return "too many failed logins" }

// seconds is the Retry-After header: whole seconds, rounded up.
func (e *lockedError) seconds() int { return int(math.Ceil(e.retryAfter.Seconds())) }

// wait says how long to wait, in whole minutes rounded up.
func (e *lockedError) wait() string {
	if m := int(math.Ceil(e.retryAfter.Minutes())); m > 1 {
		return fmt.Sprintf("%d minutes", m)
	}
	return "1 minute"
}
