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
	"fmt"
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
}

// Config maps agent-type name to its AgentConfig. An agent-type with no
// entry — including every shell-kind task, whose AgentType is always ""
// — falls back to TierIdle.
type Config map[string]AgentConfig

const (
	defaultIdleTimeout  = 30 * time.Second
	defaultPollInterval = 250 * time.Millisecond
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
}

// NewDetector constructs a Detector. markerDir is the base directory
// marker files live under; if empty, it defaults to
// $TMPDIR/loomux/completion-markers (same convention family as LOOM-4's
// $TMPDIR/loomux/ssh-cm for SSH ControlPath).
func NewDetector(store registry.Store, newExecutor orchestrator.ExecutorFactory, config Config, markerDir string) *Detector {
	markerDir = MarkerDir(markerDir)
	return &Detector{
		store:        store,
		newExecutor:  newExecutor,
		config:       config,
		markers:      NewMarkerWatcher(newExecutor, markerDir, defaultPollInterval),
		idle:         NewIdleWatcher(newExecutor, defaultPollInterval),
		pollInterval: defaultPollInterval,
	}
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exited := make(chan *targets.PaneExit, 1)
	go func() {
		if exit := d.watchExit(ctx, exec, task); exit != nil {
			exited <- exit
			cancel()
		}
	}()

	switch tier {
	case TierMarker:
		err = d.markers.Wait(ctx, task, target)
	default: // TierIdle
		timeout := cfg.IdleTimeout
		if timeout <= 0 {
			timeout = defaultIdleTimeout
		}
		err = d.idle.Wait(ctx, task, target, timeout)
	}
	select {
	case exit := <-exited:
		return &orchestrator.ProcessExitedError{Status: exit.Status, Output: exit.Output}
	default:
		return err
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
