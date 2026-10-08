package sqlite

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
)

// masterKeySize is 32 bytes, for AES-256.
const masterKeySize = 32

// KeyFromEnv reads and decodes a base64-encoded 32-byte AES-256 key from
// the named environment variable. Loomux has no key-management story of
// its own yet — this mirrors the ecosystem's existing convention of
// injecting secrets via env at process start (this repo's own future
// equivalent of command-center's bin/env.sh pattern), keeping the key
// itself out of anything the database touches.
func KeyFromEnv(varName string) ([]byte, error) {
	encoded := os.Getenv(varName)
	if encoded == "" {
		return nil, fmt.Errorf("sqlite: environment variable %s is not set", varName)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %s is not valid base64: %w", varName, err)
	}
	if len(key) != masterKeySize {
		return nil, fmt.Errorf("sqlite: %s must decode to %d bytes (AES-256), got %d", varName, masterKeySize, len(key))
	}
	return key, nil
}

// seal encrypts plaintext with AES-GCM under key, returning
// nonce||ciphertext as a single blob (a fresh random nonce per call). The
// blob opens only with the same aad, which binds it to what it belongs
// to (a credential's or SSH key's row id, so a ciphertext copied onto
// another row doesn't decrypt there).
func seal(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("sqlite: generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// open decrypts a blob seal produced, given the same aad. A wrong key,
// wrong aad or tampered/corrupted data all surface as an error here
// (AES-GCM is authenticated — there's no way to distinguish "wrong key"
// from "tampered data" without weakening that guarantee, so this doesn't
// try to).
func open(key, data, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, errors.New("sqlite: ciphertext too short")
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("sqlite: decrypt: %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sqlite: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sqlite: new gcm: %w", err)
	}
	return gcm, nil
}

// Option configures a Store at Open time.
type Option func(*Store)

// WithMasterKey configures the AES-256 key used to encrypt/decrypt
// credential values. Without it, credential operations fail fast with a
// clear error rather than silently storing plaintext or panicking.
func WithMasterKey(key []byte) Option {
	return func(s *Store) { s.masterKey = key }
}
