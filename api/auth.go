package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword bcrypt-hashes a plaintext password for storage in
// LOOMUX_AUTH_PASSWORD_HASH — the raw password itself is never meant to
// be configured or stored. loomuxd -hash-password exposes this to
// operators without needing external tooling.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("api: hash password: %w", err)
	}
	return string(hash), nil
}

// checkPassword verifies password against hash (a bcrypt hash, e.g. from
// HashPassword / LOOMUX_AUTH_PASSWORD_HASH). bcrypt's own comparison is
// already constant-time with respect to the password content.
func checkPassword(hash []byte, password string) error {
	return bcrypt.CompareHashAndPassword(hash, []byte(password))
}

// newToken generates a fresh random bearer token (32 bytes of
// crypto/rand, base64url-encoded) and its SHA-256 hex digest. Only the
// hash is ever persisted (registry.Session.TokenHash) — raw is returned
// to the client exactly once, at login, so a database compromise alone
// never hands out a working credential.
func newToken() (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("api: generate token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashToken(raw), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
