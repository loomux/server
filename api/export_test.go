package api

import "time"

// SetBodyReadTimeout shortens the request-body read deadline for a test.
func SetBodyReadTimeout(d time.Duration) (restore func()) {
	old := bodyReadTimeout
	bodyReadTimeout = d
	return func() { bodyReadTimeout = old }
}
