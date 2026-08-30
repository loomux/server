package api

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Config is api's own process configuration, loaded from environment
// variables — this repo's established convention (see
// registry/sqlite.KeyFromEnv, router/llmrouter.ConfigFromEnv,
// app.LoadConfig).
type Config struct {
	// PasswordHash is a bcrypt hash (see HashPassword) — the single v1
	// user's credential. Required: there is no unauthenticated fallback
	// (design spec §9: "no 'it's just me so no auth' shortcut").
	PasswordHash []byte
	// Addr is the address Server listens on. Plain HTTP is assumed — the
	// deployment model is a reverse proxy in front terminating TLS (see
	// README.md).
	Addr string
	// SessionTTL is the sliding-expiration window (see WithSessionTTL).
	SessionTTL time.Duration
}

const (
	envPasswordHash = "LOOMUX_AUTH_PASSWORD_HASH"
	envAddr         = "LOOMUX_HTTP_ADDR"
	envSessionTTL   = "LOOMUX_SESSION_TTL"

	defaultAddr = ":8080"
)

// LoadConfig reads Config from the environment, failing fast on a
// missing or malformed password hash and on a malformed (not merely
// absent) session TTL.
func LoadConfig() (Config, error) {
	hash := os.Getenv(envPasswordHash)
	if hash == "" {
		return Config{}, fmt.Errorf("api: %s is not set", envPasswordHash)
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return Config{}, fmt.Errorf("api: %s is not a valid bcrypt hash: %w", envPasswordHash, err)
	}

	addr := os.Getenv(envAddr)
	if addr == "" {
		addr = defaultAddr
	}

	ttl := defaultSessionTTL
	if raw := os.Getenv(envSessionTTL); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("api: %s is not a valid duration: %w", envSessionTTL, err)
		}
		ttl = d
	}

	return Config{PasswordHash: []byte(hash), Addr: addr, SessionTTL: ttl}, nil
}
