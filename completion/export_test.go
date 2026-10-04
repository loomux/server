package completion

import "time"

// SetUnreachableGrace shortens how long a wait rides out an unreachable
// target, for a test.
func SetUnreachableGrace(d time.Duration) (restore func()) {
	old := unreachableGrace
	unreachableGrace = d
	return func() { unreachableGrace = old }
}
