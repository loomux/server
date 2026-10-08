package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"
)

// loginDevices gives each device that has logged in successfully its own
// login backoff (LOOM-151). The global loginThrottle alone let anyone who
// can reach /login keep the owner out: one wrong guess per backoff window
// held it at its cap, and the owner's correct password got 429 too. A
// login that presents a known device's token is throttled on that
// device's counter instead, so a stranger's failures never slow it.
//
// A device token is a random id and an HMAC of it, keyed from the
// password hash: stateless, unforgeable without the hash, and void as
// soon as the password changes. It proves only that this browser logged
// in before — never a session, never a way past the password.
type loginDevices struct {
	key       []byte
	base, max time.Duration

	mu        sync.Mutex
	throttles map[string]*loginThrottle
}

const (
	deviceIDLen  = 16
	deviceMACLen = 16
	// maxDeviceThrottles bounds the per-device counters. Only a correct
	// password mints a device, so reaching it means many logins from
	// many browsers; forgetting the counters then only resets backoffs.
	maxDeviceThrottles = 256
)

func newLoginDevices(passwordHash []byte, base, max time.Duration) *loginDevices {
	key := sha256.Sum256(append([]byte("loomux-login-device\x00"), passwordHash...))
	return &loginDevices{key: key[:], base: base, max: max, throttles: map[string]*loginThrottle{}}
}

func (d *loginDevices) mac(id []byte) []byte {
	m := hmac.New(sha256.New, d.key)
	m.Write(id)
	return m.Sum(nil)[:deviceMACLen]
}

// issue mints a new device token.
func (d *loginDevices) issue() (string, error) {
	id := make([]byte, deviceIDLen)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(append(id, d.mac(id)...)), nil
}

// throttle returns the backoff of the device token names, or nil if the
// token isn't one this server issued under the current password.
func (d *loginDevices) throttle(token string) *loginThrottle {
	if token == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != deviceIDLen+deviceMACLen {
		return nil
	}
	id, sum := raw[:deviceIDLen], raw[deviceIDLen:]
	if !hmac.Equal(sum, d.mac(id)) {
		return nil
	}
	key := hex.EncodeToString(id)

	d.mu.Lock()
	defer d.mu.Unlock()
	th, ok := d.throttles[key]
	if !ok {
		if len(d.throttles) >= maxDeviceThrottles {
			d.throttles = map[string]*loginThrottle{}
		}
		th = newLoginThrottle(d.base, d.max)
		d.throttles[key] = th
	}
	return th
}
