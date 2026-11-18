// Package token makes the random ids, credentials and bearer tokens Berth hands out, and the
// SHA-256 berthd keeps instead of a token.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Hex is n random bytes from the operating system, hex encoded.
func Hex(n int) (string, error) {
	b, err := random(n)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Base64 is n random bytes from the operating system, unpadded URL safe base64.
func Base64(n int) (string, error) {
	b, err := random(n)
	if err != nil {
		return "", err
	}
	defer clear(b)
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash is what berthd stores in place of a token.
func Hash(t string) [32]byte { return sha256.Sum256([]byte(t)) }

// Matches compares t with a stored hash in constant time.
func Matches(t string, hash [32]byte) bool {
	got := Hash(t)
	return subtle.ConstantTimeCompare(got[:], hash[:]) == 1
}

func random(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("random source: %w", err)
	}
	return b, nil
}
