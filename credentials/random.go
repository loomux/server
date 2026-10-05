package credentials

import (
	"crypto/rand"
	"encoding/hex"
)

func randomToken() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
