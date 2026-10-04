// Package completion implements the real tiered CompletionDetector
// (design spec §5) behind orchestrator.CompletionDetector: tiers 1+2
// (native hook / prompt-engineered self-report) share one marker-watching
// mechanism (marker.go); tier 3 (idle-time heuristic, the true last
// resort) polls a task's pane for CapturePane output going quiet
// (idle.go). Detector (this file) resolves which tier applies to a given
// task and dispatches to the right one.
package completion

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// Tier identifies which completion-detection strategy applies. Native
// hooks and prompt-engineered self-report (spec §5 tiers 1 and 2) share
// one detection mechanism — the only difference between them is how the
// agent was told to signal, not the detection code — so they collapse to
// a single Tier value here.
type Tier int

const (
	// TierMarker watches a side-channel marker file (tiers 1+2).
	TierMarker Tier = iota + 1
	// TierIdle polls the pane and treats "no change for a while" as
	// completion (tier 3) — the true last resort per the design spec,
	// not a default agents opt into by omission.
	TierIdle
	// TierExit completes when the pane's process exits (LOOM-71) — design
	// spec §3's "completion for a Loomux-driven shell task is just script
	// exit". It is not configured per agent-type: every
	// registry.TaskKindCommand task uses it, and nothing else does.
	TierExit
)

// AgentConfig is the minimal per-agent-type configuration completion
// detection needs. This is deliberately NOT the full agent-type registry
// design spec §6 describes (launch command templates, etc.) — that
// remains a separate, later concern. This shape will need reconciling
// with whatever that eventual registry looks like.
type AgentConfig struct {
	Tier Tier
	// IdleTimeout is only meaningful when Tier == TierIdle. Zero means
	// "use the package default."
	IdleTimeout time.Duration
	// MaxTurnDuration bounds one turn (LOOM-76): past it, Wait gives up
	// with an *orchestrator.TurnTimeoutError, the pane left running. Zero
	// means the package default (an hour); negative means unbounded.
	// Doesn't apply to TierExit, whose callers bound their own waits.
	MaxTurnDuration time.Duration
	// NoProgressTimeout, for TierMarker only, ends a turn whose pane
	// hasn't changed at all for this long while no marker came — an
	// agent blocked on an approval prompt, say (LOOM-76). Zero means the
	// package default (ten minutes); negative disables it.
	NoProgressTimeout time.Duration
	// DetectPrompt, for TierMarker only, reads the prompt an agent is
	// stopped at off its pane, nil for none (LOOM-97). A prompt showing
	// on two consecutive progress checks ends the wait with an
	// *orchestrator.NeedsAttentionError. nil disables it.
	DetectPrompt func(screen string) *registry.Attention
}

// Config maps agent-type name to its AgentConfig. An agent-type with no
// entry — including every shell-kind task, whose AgentType is always ""
// — falls back to TierIdle.
type Config map[string]AgentConfig

const (
	defaultIdleTimeout = 30 * time.Second
	// defaultPollInterval paces marker, idle and exit checks: each is an ssh
	// round trip on a remote target, so it is kept slow enough that many
	// waiting tasks stay well inside a target's ssh budget (LOOM-84).
	defaultPollInterval = time.Second
	// LOOM-76 defaults: generous enough for real work, finite so a stuck
	// turn surfaces instead of holding the request open indefinitely.
	defaultMaxTurnDuration   = time.Hour
	defaultNoProgressTimeout = 10 * time.Minute
	// defaultProgressPollInterval is how often the no-progress check
	// captures the pane — far less often than marker polling, since over
	// SSH each capture is a round trip.
	defaultProgressPollInterval = 5 * time.Second
)

// Detector is the real tiered orchestrator.CompletionDetector
// implementation.
type Detector struct {
	store        registry.Store
	newExecutor  orchestrator.ExecutorFactory
	config       Config
	markers      *MarkerWatcher
	idle         *IdleWatcher
	pollInterval time.Duration
	// progressPollInterval paces the no-progress check (LOOM-76).
	progressPollInterval time.Duration
}

// DetectorOption configures a Detector.
type DetectorOption func(*Detector)

// WithProgressPollInterval overrides how often the no-progress check
// (LOOM-76) captures the pane. Mainly for tests.
func WithProgressPollInterval(d time.Duration) DetectorOption {
	return func(det *Detector) { det.progressPollInterval = d }
}

// NewDetector constructs a Detector. markerDir is the configured marker
// directory (LOOMUX_MARKER_DIR); empty means the per-user default that
// ResolveMarkerDir creates on each task's target.
func NewDetector(store registry.Store, newExecutor orchestrator.ExecutorFactory, config Config, markerDir string, opts ...DetectorOption) *Detector {
	d := &Detector{
		store:                store,
		newExecutor:          newExecutor,
		config:               config,
		markers:              NewMarkerWatcher(newExecutor, markerDir, defaultPollInterval),
		idle:                 NewIdleWatcher(newExecutor, defaultPollInterval),
		pollInterval:         defaultPollInterval,
		progressPollInterval: defaultProgressPollInterval,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

var _ orchestrator.CompletionDetector = (*Detector)(nil)

// Wait resolves task's detection tier and dispatches to the matching
// watcher. For the marker and idle tiers it also watches the pane's
// process (LOOM-71): if that exits first, nothing will ever signal the
// turn complete — an agent CLI that isn't installed writes no marker —
// so Wait returns an *orchestrator.ProcessExitedError at once instead of
// hanging (marker) or reporting a dead pane's frozen output as a finished
// turn (idle).
func (d *Detector) Wait(ctx context.Context, task *registry.Task) error {
	target, cfg, tier, err := d.resolve(ctx, task)
	if err != nil {
		return fmt.Errorf("completion: wait: %w", err)
	}
	exec, err := d.newExecutor(target)
	if err != nil {
		return fmt.Errorf("completion: wait: %w", err)
	}

	if tier == TierExit {
		return d.waitExit(ctx, exec, task)
	}

	// The turn's bounds (LOOM-76) cancel waitCtx with a TurnTimeoutError
	// as the cause, telling them apart from the caller cancelling.
	waitCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if limit := orDefault(cfg.MaxTurnDuration, defaultMaxTurnDuration); limit > 0 {
		timer := time.AfterFunc(limit, func() {
			cancel(&orchestrator.TurnTimeoutError{Reason: orchestrator.TimeoutMaxTurnDuration, Limit: limit})
		})
		defer timer.Stop()
	}
	if limit := orDefault(cfg.NoProgressTimeout, defaultNoProgressTimeout); tier == TierMarker && (limit > 0 || cfg.DetectPrompt != nil) {
		go d.watchProgress(waitCtx, exec, task, limit, cfg.DetectPrompt, cancel)
	}
	// A settle bound (orchestrator.WithSettle) ends the turn once the pane
	// stops changing, whatever the tier's own signal.
	if settle := orchestrator.SettleFrom(ctx); settle > 0 && tier == TierMarker {
		go func() {
			if d.idle.Wait(waitCtx, task, target, settle) == nil {
				cancel(errSettled)
			}
		}()
	}
	exited := make(chan *targets.PaneExit, 1)
	go func() {
		if exit := d.watchExit(waitCtx, exec, task); exit != nil {
			exited <- exit
			cancel(nil)
		}
	}()

	switch tier {
	case TierMarker:
		err = d.markers.Wait(waitCtx, task, target)
	default: // TierIdle
		timeout := cfg.IdleTimeout
		if timeout <= 0 {
			timeout = defaultIdleTimeout
		}
		err = d.idle.Wait(waitCtx, task, target, timeout)
	}
	select {
	case exit := <-exited:
		return &orchestrator.ProcessExitedError{Status: exit.Status, Output: exit.Output}
	default:
	}
	if err != nil && ctx.Err() == nil {
		cause := context.Cause(waitCtx)
		var timeout *orchestrator.TurnTimeoutError
		var attention *orchestrator.NeedsAttentionError
		switch {
		case errors.As(cause, &timeout):
			return timeout
		case errors.As(cause, &attention):
			return attention
		case errors.Is(cause, errSettled):
			return nil
		}
	}
	return err
}

// errSettled is a wait's cancellation cause when its pane settled
// (orchestrator.WithSettle): the turn is over.
var errSettled = errors.New("completion: the pane settled")

// orDefault resolves a LOOM-76 bound: zero means def, negative disabled
// (returned as 0).
func orDefault(d, def time.Duration) time.Duration {
	switch {
	case d == 0:
		return def
	case d < 0:
		return 0
	}
	return d
}

// watchProgress ends the wait (via cancel) once the pane's contents have
// been unchanged for limit (zero: never), or once detect finds the same
// prompt on two consecutive checks (LOOM-97) — twice, so a prompt caught
// mid-redraw isn't reported. Capture errors are ignored: this is only an
// extra bound, and the tier's own watcher reports a pane it can't reach.
func (d *Detector) watchProgress(ctx context.Context, exec targets.TargetExecutor, task *registry.Task, limit time.Duration,
	detect func(string) *registry.Attention, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(d.progressPollInterval)
	defer ticker.Stop()
	var last string
	var lastChange time.Time
	var lastPrompt *registry.Attention
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		out, err := exec.CapturePane(ctx, task.TmuxSession)
		if err != nil {
			continue
		}
		if detect != nil {
			prompt := detect(out)
			if prompt != nil && lastPrompt != nil && reflect.DeepEqual(prompt, lastPrompt) {
				cancel(&orchestrator.NeedsAttentionError{Attention: prompt})
				return
			}
			lastPrompt = prompt
		}
		now := time.Now()
		if lastChange.IsZero() || out != last {
			last, lastChange = out, now
			continue
		}
		if limit > 0 && now.Sub(lastChange) >= limit {
			cancel(&orchestrator.TurnTimeoutError{Reason: orchestrator.TimeoutNoProgress, Limit: limit})
			return
		}
	}
}

// waitExit is TierExit: poll until the pane's process has exited. Any
// error checking is returned — this is the task's only completion signal.
func (d *Detector) waitExit(ctx context.Context, exec targets.TargetExecutor, task *registry.Task) error {
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()
	for {
		exit, err := exec.PaneExited(ctx, task.TmuxSession)
		if err != nil {
			return fmt.Errorf("completion: wait: %w", err)
		}
		if exit != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// watchExit polls alongside another tier's watcher until the pane's
// process exits (returning how) or ctx is done (nil). Errors checking are
// ignored: here the exit is only an extra signal, and the tier's own
// watcher already reports a pane it can't reach in its own way.
func (d *Detector) watchExit(ctx context.Context, exec targets.TargetExecutor, task *registry.Task) *targets.PaneExit {
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()
	for {
		if exit, err := exec.PaneExited(ctx, task.TmuxSession); err == nil && exit != nil {
			return exit
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// resolve looks up task's target once (needed regardless of tier, to
// hand to whichever watcher is chosen) and picks the detection tier:
// the agent-type's configured tier, falling back to the idle heuristic
// for an unconfigured agent-type (matching the spec's framing of idle
// detection as the true last resort). Marker detection (tiers 1+2)
// works identically for local and remote targets — MarkerWatcher goes
// through TargetExecutor.FileExists/RemoveFile either way (LOOM-11) —
// so there's no target-kind restriction here; resolve never probes
// reachability, and an unreachable target's failure surfaces as a plain
// error from Wait, same as any other TargetExecutor operation.
func (d *Detector) resolve(ctx context.Context, task *registry.Task) (*registry.Target, AgentConfig, Tier, error) {
	ws, err := d.store.GetWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return nil, AgentConfig{}, 0, err
	}
	target, err := d.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return nil, AgentConfig{}, 0, err
	}

	if task.Kind == registry.TaskKindCommand {
		return target, AgentConfig{}, TierExit, nil
	}
	cfg, ok := d.config[task.AgentType]
	if ok && cfg.Tier == TierMarker {
		return target, cfg, TierMarker, nil
	}
	return target, cfg, TierIdle, nil
}
