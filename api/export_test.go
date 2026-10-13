package api

import "time"

// SetBodyReadTimeout shortens the request-body read deadline for a test.
func SetBodyReadTimeout(d time.Duration) (restore func()) {
	old := bodyReadTimeout
	bodyReadTimeout = d
	return func() { bodyReadTimeout = old }
}

// WithClock sets the clock sessions are stamped and expired by, so a test
// can step past a sliding window instead of sleeping through it (LOOM-200).
func WithClock(now func() time.Time) Option {
	return func(s *Server) { s.now = now }
}
