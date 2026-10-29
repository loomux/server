package completion

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// pollHub runs one poll loop per target, shared by every wait on it
// (LOOM-254). Each tick asks, in one target operation (targets.Poller),
// about every waiting turn's marker file and pane, where each wait used
// to make its own FileExists and PaneExited calls: ten turns on one
// remote target cost one ssh round trip a second, not twenty, so they
// no longer fill the target's operation budget and queue everything
// else behind them.
type pollHub struct {
	interval time.Duration

	mu    sync.Mutex
	loops map[string]*pollLoop // by pollKey
}

func newPollHub(interval time.Duration) *pollHub {
	return &pollHub{interval: interval, loops: map[string]*pollLoop{}}
}

// pollKey tells targets apart, and a target from the same one edited
// since: a wait that starts after an edit polls through an executor for
// the edited target, as it did before polls were shared.
func pollKey(t *registry.Target) string {
	return t.ID + "@" + strconv.FormatInt(t.UpdatedAt.UnixNano(), 10)
}

// pollLoop polls one target for its subscribers, one Poll per tick.
type pollLoop struct {
	poller targets.Poller
	kick   chan struct{} // poll now: a new subscriber is waiting
	cancel context.CancelFunc
	subs   map[*pollSub]struct{} // guarded by pollHub.mu
}

// pollSub is one wait's share of a pollLoop: a marker file or a pane.
type pollSub struct {
	file, pane string
	out        chan pollOutcome // the latest poll's answer, buffered 1
}

type pollOutcome struct {
	exists  bool // file exists
	running bool // pane's process is still running
	err     error
}

// subscribe adds a wait on file or pane to target's loop, starting the
// loop if it's the first; nil if exec can't poll in batches. The
// returned func ends the subscription, and the loop with its last one.
func (h *pollHub) subscribe(target *registry.Target, exec targets.TargetExecutor, file, pane string) (*pollSub, func()) {
	poller, ok := exec.(targets.Poller)
	if !ok || h == nil {
		return nil, nil
	}
	key := pollKey(target)
	sub := &pollSub{file: file, pane: pane, out: make(chan pollOutcome, 1)}
	h.mu.Lock()
	loop := h.loops[key]
	if loop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		loop = &pollLoop{poller: poller, kick: make(chan struct{}, 1), cancel: cancel, subs: map[*pollSub]struct{}{}}
		h.loops[key] = loop
		go h.run(ctx, loop)
	}
	loop.subs[sub] = struct{}{}
	h.mu.Unlock()
	// A wait checks at once, as it did polling on its own.
	select {
	case loop.kick <- struct{}{}:
	default:
	}
	return sub, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(loop.subs, sub)
		if len(loop.subs) == 0 && h.loops[key] == loop {
			delete(h.loops, key)
			loop.cancel()
		}
	}
}

// run polls on every tick, and at once for a new subscriber. A poll that
// outlasts the interval delays the next one rather than piling up.
func (h *pollHub) run(ctx context.Context, loop *pollLoop) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-loop.kick:
		case <-ticker.C:
		}
		h.poll(ctx, loop)
	}
}

// poll asks about every current subscriber in one Poll and hands each
// its answer. One that subscribed meanwhile gets the next poll's.
func (h *pollHub) poll(ctx context.Context, loop *pollLoop) {
	h.mu.Lock()
	subs := make([]*pollSub, 0, len(loop.subs))
	for sub := range loop.subs {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	if len(subs) == 0 {
		return
	}
	var req targets.PollRequest
	files, panes := map[string]int{}, map[string]int{}
	for _, sub := range subs {
		if _, ok := files[sub.file]; sub.file != "" && !ok {
			files[sub.file] = len(req.Files)
			req.Files = append(req.Files, sub.file)
		}
		if _, ok := panes[sub.pane]; sub.pane != "" && !ok {
			panes[sub.pane] = len(req.Panes)
			req.Panes = append(req.Panes, sub.pane)
		}
	}
	res, err := loop.poller.Poll(ctx, req)
	if ctx.Err() != nil {
		return // the last subscriber left
	}
	for _, sub := range subs {
		o := pollOutcome{err: err}
		if err == nil {
			if sub.file != "" {
				o.exists = res.Exists[files[sub.file]]
			}
			if sub.pane != "" {
				o.running = res.Running[panes[sub.pane]]
			}
		}
		// Only this loop sends, so after a drain the send can't block.
		select {
		case <-sub.out:
		default:
		}
		sub.out <- o
	}
}

// checker is how one wait polls its marker file or its pane: through
// the target's shared loop when its executor can batch, else with its
// own FileExists or PaneExited calls every interval, as before LOOM-254.
type checker struct {
	exec       targets.TargetExecutor
	file, pane string

	sub    *pollSub
	stop   func()
	ticker *time.Ticker
	first  bool
}

func (h *pollHub) checker(target *registry.Target, exec targets.TargetExecutor, file, pane string, interval time.Duration) *checker {
	c := &checker{exec: exec, file: file, pane: pane, first: true}
	if c.sub, c.stop = h.subscribe(target, exec, file, pane); c.sub == nil {
		c.ticker = time.NewTicker(interval)
	}
	return c
}

func (c *checker) close() {
	if c.stop != nil {
		c.stop()
	}
	if c.ticker != nil {
		c.ticker.Stop()
	}
}

// next waits for the next check, then reports whether the file exists,
// or the pane's exit (nil while it runs), and the error checking. A
// pane the shared poll didn't see running is asked about with
// PaneExited, which says how it ended, or that it never was. Done ctx
// is ctx.Err().
func (c *checker) next(ctx context.Context) (exists bool, exit *targets.PaneExit, err error) {
	if c.sub != nil {
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		case o := <-c.sub.out:
			if o.err != nil || c.file != "" || o.running {
				return o.exists, nil, o.err
			}
		}
		exit, err = c.exec.PaneExited(ctx, c.pane)
		return false, exit, err
	}
	if !c.first {
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		case <-c.ticker.C:
		}
	}
	c.first = false
	if c.file != "" {
		exists, err = c.exec.FileExists(ctx, c.file)
		return exists, nil, err
	}
	exit, err = c.exec.PaneExited(ctx, c.pane)
	return false, exit, err
}
