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
