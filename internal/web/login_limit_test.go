package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// limitedApp is a basic login with alice and bob, locking a username after
// three failures for fifteen minutes, on a clock the test moves.
func limitedApp(t *testing.T) (*basicApp, *testClock) {
	t.Helper()
	users := "alice:" + bcryptHash(t, "s3cret") + "\nbob:" + bcryptHash(t, "hunter2") + "\n"
	ba := newBasicAppFrom(t, BasicAuthOptions{
		UsersFile: writeUsersFile(t, users), MaxFailures: 3, FailureWindow: 15 * time.Minute,
	}, "", false)
	clock := &testClock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	ba.auth.limit.now = clock.now
	return ba, clock
}

func TestLoginLimitLocksAfterTheFailures(t *testing.T) {
	ba, _ := limitedApp(t)
	for i := 1; i <= 3; i++ {
		if rec := ba.login("alice", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i, rec.Code)
		}
	}
	// The password is checked no more: the right one gets the same answer.
	checks := strings.Count(ba.logs.String(), "login: invalid credentials")
	for _, password := range []string{"wrong", "s3cret"} {
		rec := ba.login("alice", password)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("locked, password %q: status %d, want 429", password, rec.Code)
		}
		if got := rec.Header().Get("Retry-After"); got != "900" {
			t.Errorf("Retry-After = %q, want 900", got)
		}
		mustContain(t, rec.Body.String(), "Too many failed logins. Try again in 15 minutes.")
		if cookie(rec, sessionCookie) != nil {
			t.Error("a locked login must not set a session cookie")
		}
	}
	if got := strings.Count(ba.logs.String(), "login: invalid credentials"); got != checks {
		t.Errorf("a locked login checked the password: %d warnings, want %d", got, checks)
	}
	mustContain(t, ba.logs.String(), "login: username locked", "user=alice")

	// Another username is not affected.
	if rec := ba.login("bob", "hunter2"); rec.Code != http.StatusFound {
		t.Errorf("bob: status %d, want 302", rec.Code)
	}
}

func TestLoginLimitLocksTheCommandLineToo(t *testing.T) {
	ba, _ := limitedApp(t)
	// The form and the command line share the count.
	ba.login("alice", "wrong")
	ba.cliLogin(`{"username":"alice","password":"wrong"}`)
	ba.login("alice", "wrong")

	rec := ba.cliLogin(`{"username":"alice","password":"s3cret"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "900" {
		t.Errorf("Retry-After = %q, want 900", got)
	}
	if got := apiErrorOf(t, rec); got != "too many failed logins: try again in 15 minutes" {
		t.Errorf("error = %q", got)
	}
}

// A username that is not in the file is locked like one that is, so the limit
// does not say which exist.
func TestLoginLimitTreatsAnUnknownUsernameAlike(t *testing.T) {
	ba, _ := limitedApp(t)
	for i := 0; i < 3; i++ {
		if rec := ba.login("mallory", "guess"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i+1, rec.Code)
		}
	}
	rec := ba.login("mallory", "guess")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "900" {
		t.Errorf("status %d, Retry-After %q, want 429 and 900", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestLoginLimitWindowIsFixedFromTheFirstFailure(t *testing.T) {
	ba, clock := limitedApp(t)
	ba.login("alice", "wrong")
	clock.advance(10 * time.Minute)
	ba.login("alice", "wrong")
	ba.login("alice", "wrong")

	// Later failures don't move the end of the window: 5 minutes are left.
	rec := ba.login("alice", "s3cret")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "300" {
		t.Fatalf("status %d, Retry-After %q, want 429 and 300", rec.Code, rec.Header().Get("Retry-After"))
	}
	mustContain(t, rec.Body.String(), "Try again in 5 minutes.")

	clock.advance(5 * time.Minute)
	if rec := ba.login("alice", "s3cret"); rec.Code != http.StatusFound {
		t.Errorf("after the window: status %d, want 302", rec.Code)
	}
}

func TestLoginLimitSuccessClearsTheCount(t *testing.T) {
	ba, _ := limitedApp(t)
	ba.login("alice", "wrong")
	ba.login("alice", "wrong")
	if rec := ba.login("alice", "s3cret"); rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	// Two more failures make four in the window, but the count restarted.
	ba.login("alice", "wrong")
	ba.login("alice", "wrong")
	if rec := ba.login("alice", "s3cret"); rec.Code != http.StatusFound {
		t.Errorf("status %d, want 302: the success should have cleared the count", rec.Code)
	}
}

func TestLoginLimitOffWithAZero(t *testing.T) {
	for name, o := range map[string]BasicAuthOptions{
		"no failures": {MaxFailures: 0, FailureWindow: time.Minute},
		"no window":   {MaxFailures: 1, FailureWindow: 0},
	} {
		t.Run(name, func(t *testing.T) {
			o.UsersFile = writeUsersFile(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
			ba := newBasicAppFrom(t, o, "", false)
			for i := 0; i < 20; i++ {
				if rec := ba.login("alice", "wrong"); rec.Code != http.StatusUnauthorized {
					t.Fatalf("failure %d: status %d, want 401", i+1, rec.Code)
				}
			}
			if rec := ba.login("alice", "s3cret"); rec.Code != http.StatusFound {
				t.Errorf("status %d, want 302", rec.Code)
			}
		})
	}
}

// The counters are bounded by time: a username that is never tried again
// leaves no entry once its window ends.
func TestFailureLimitDropsTheExpired(t *testing.T) {
	l := newFailureLimit(3, time.Minute)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	for i := 0; i < 1000; i++ {
		l.fail(fmt.Sprintf("user-%d", i))
		now = now.Add(time.Millisecond)
	}
	if len(l.entries) != 1000 || l.order.Len() != 1000 {
		t.Fatalf("entries = %d, order = %d, want 1000", len(l.entries), l.order.Len())
	}
	now = now.Add(time.Minute)
	l.locked("anyone")
	if len(l.entries) != 0 || l.order.Len() != 0 {
		t.Errorf("after the window: entries = %d, order = %d, want 0", len(l.entries), l.order.Len())
	}
}

func TestFailureLimitClear(t *testing.T) {
	l := newFailureLimit(3, time.Minute)
	l.fail("alice")
	l.fail("bob")
	l.clear("alice")
	l.clear("nobody")
	if len(l.entries) != 1 || l.order.Len() != 1 {
		t.Errorf("entries = %d, order = %d, want 1", len(l.entries), l.order.Len())
	}
}

// A request waits for a slot while others compare passwords, and gives up when
// it ends.
func TestLoginSlotsBoundTheComparisons(t *testing.T) {
	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	ba.auth.slots = make(chan struct{}, 1)
	ba.auth.slots <- struct{}{} // taken by another comparison

	done := make(chan *http.Cookie, 1)
	go func() { done <- cookie(ba.login("alice", "s3cret"), sessionCookie) }()
	select {
	case <-done:
		t.Fatal("the login ran with no slot free")
	case <-time.After(100 * time.Millisecond):
	}
	<-ba.auth.slots
	select {
	case c := <-done:
		if c == nil {
			t.Error("the login did not get a session once a slot was free")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the login did not run once a slot was free")
	}
	if len(ba.auth.slots) != 0 {
		t.Error("the login kept its slot")
	}

	ba.auth.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ba.auth.check(ctx, "alice", "s3cret"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
