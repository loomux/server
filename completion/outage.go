package completion

import (
	"errors"
	"time"

	"github.com/Loomux/server/targets"
)

// unreachableGrace is how long a wait rides out a target it can't reach
// before giving up on the turn (a broken ssh connection, the network
// sidecar restarting): one failed poll is no reason to fail a turn the
// agent is still working on.
var unreachableGrace = time.Minute

// outage tracks how long a watcher's polls have been failing because the
// target was unreachable.
type outage struct {
	since time.Time
	// grace overrides unreachableGrace when set (WithUnreachableGrace).
	grace time.Duration
}

// tolerate reports whether err may be ridden out: the target is
// unreachable, and hasn't been for longer than the grace.
func (o *outage) tolerate(err error) bool {
	if !errors.Is(err, targets.ErrUnreachable) {
		return false
	}
	if o.since.IsZero() {
		o.since = time.Now()
	}
	grace := o.grace
	if grace <= 0 {
		grace = unreachableGrace
	}
	return time.Since(o.since) < grace
}

// clear records a poll that reached the target.
func (o *outage) clear() { o.since = time.Time{} }
