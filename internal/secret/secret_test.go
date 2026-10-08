package secret

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestNewIsPrefixAnd256Bits(t *testing.T) {
	s := New()
	if !regexp.MustCompile(`^nops_[A-Za-z0-9_-]{43}$`).MatchString(s) {
		t.Fatalf("New() = %q, want nops_ and 43 base64url characters", s)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, Prefix))
	if err != nil || len(b) != 32 {
		t.Errorf("the random part decodes to %d bytes (error %v), want 32", len(b), err)
	}
}

func TestNewIsNotRepeated(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		s := New()
		if seen[s] {
			t.Fatalf("New() returned %q twice", s)
		}
		seen[s] = true
	}
}

func TestValid(t *testing.T) {
	good := New()
	for _, tc := range []struct {
		name, in string
		want     bool
	}{
		{"a generated secret", good, true},
		{"empty", "", false},
		{"no prefix", strings.TrimPrefix(good, Prefix), false},
		{"too short", good[:len(good)-1], false},
		{"too long", good + "A", false},
		{"not base64url", Prefix + strings.Repeat("!", 43), false},
		{"a trailing newline", good + "\n", false},
	} {
		if got := Valid(tc.in); got != tc.want {
			t.Errorf("%s: Valid(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}
