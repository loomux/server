package health

import "time"

// SetDeepTimeout swaps Deep's overall deadline for a test.
func SetDeepTimeout(d time.Duration) (restore func()) {
	old := deepTimeout
	deepTimeout = d
	return func() { deepTimeout = old }
}
