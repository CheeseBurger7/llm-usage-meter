// Package auth issues and verifies the API keys that tie a request to an org.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Scope is what a key is allowed to do.
type Scope string

const (
	// ScopeIngest allows writing usage events.
	ScopeIngest Scope = "ingest"
	// ScopeRead allows reading usage and cost.
	ScopeRead Scope = "read"
)

// ParseScope turns user input into a Scope.
func ParseScope(s string) (Scope, error) {
	switch Scope(s) {
	case ScopeIngest, ScopeRead:
		return Scope(s), nil
	}
	return "", fmt.Errorf("unknown scope %q (want %q or %q)", s, ScopeIngest, ScopeRead)
}

const (
	keyBrand    = "lum"
	secretBytes = 32
	// secretLen is the length of the hex-encoded secret.
	secretLen = secretBytes * 2
	// PrefixLen is how many leading characters of the secret are stored and
	// shown to humans to tell keys apart.
	PrefixLen = 8
)

// ErrInvalidKey means the key is malformed or not known. The two cases are
// deliberately indistinguishable to the caller.
var ErrInvalidKey = errors.New("auth: invalid API key")

// ErrRevokedKey means the key was valid once and has been revoked.
var ErrRevokedKey = errors.New("auth: API key revoked")

// generateKey returns a new plaintext key, lum_<scope>_<64 hex characters>,
// built from 32 bytes of crypto/rand.
func generateKey(scope Scope) (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return keyBrand + "_" + string(scope) + "_" + hex.EncodeToString(b), nil
}

// parseKey checks the shape of a plaintext key and returns its scope and
// prefix. It lets Verify reject garbage without a database lookup.
func parseKey(key string) (Scope, string, error) {
	parts := strings.Split(key, "_")
	if len(parts) != 3 || parts[0] != keyBrand {
		return "", "", ErrInvalidKey
	}
	scope, err := ParseScope(parts[1])
	if err != nil {
		return "", "", ErrInvalidKey
	}
	secret := parts[2]
	if len(secret) != secretLen {
		return "", "", ErrInvalidKey
	}
	for _, c := range secret {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", "", ErrInvalidKey
		}
	}
	return scope, secret[:PrefixLen], nil
}

// hashKey is what gets stored and looked up: SHA-256 of the whole plaintext
// key. The secret has 256 bits of entropy, so a fast unsalted hash is enough;
// a password hash would only add latency to every request.
func hashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
