// Package secret makes the secrets Nops generates: the API tokens, and what
// "nops secret generate" prints (docs/cli.md).
package secret

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
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
