// Package secret makes the secrets Nops generates: the tokens, and what
// "nops secret generate" prints (docs/cli.md).
package secret

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// Prefix starts every secret, so a secret scanner and a person can tell one
// from any other string.
const Prefix = "nops_"

// New returns a secret: Prefix and 256 random bits, which is why a plain
// SHA-256 of it is enough to keep.
func New() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("secret: crypto/rand: %v", err)) // the platform has no randomness: nothing safe to do
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Valid reports whether s is what New makes: Prefix and 32 bytes in base64url.
// Nops refuses a bootstrap token that is not.
func Valid(s string) bool {
	rest, ok := strings.CutPrefix(s, Prefix)
	if !ok {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	// The decoder skips line breaks, so compare what it read with what it was given.
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == rest
}
