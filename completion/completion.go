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
	store   registry.Store
	config  Config
	markers *MarkerWatcher
	idle    *IdleWatcher
}

// NewDetector constructs a Detector. markerDir is the base directory
// marker files live under; if empty, it defaults to
// $TMPDIR/loomux/completion-markers (same convention family as LOOM-4's
// $TMPDIR/loomux/ssh-cm for SSH ControlPath).
func NewDetector(store registry.Store, newExecutor orchestrator.ExecutorFactory, config Config, markerDir string) *Detector {
	markerDir = MarkerDir(markerDir)
	return &Detector{
		store:   store,
		config:  config,
		markers: NewMarkerWatcher(newExecutor, markerDir, defaultPollInterval),
		idle:    NewIdleWatcher(newExecutor, defaultPollInterval),
	}
}

var _ orchestrator.CompletionDetector = (*Detector)(nil)

// Wait resolves task's detection tier and dispatches to the matching
// watcher.
func (d *Detector) Wait(ctx context.Context, task *registry.Task) error {
	target, cfg, tier, err := d.resolve(ctx, task)
	if err != nil {
		return fmt.Errorf("completion: wait: %w", err)
	}

	switch tier {
	case TierMarker:
		return d.markers.Wait(ctx, task, target)
	default: // TierIdle
		timeout := cfg.IdleTimeout
		if timeout <= 0 {
			timeout = defaultIdleTimeout
		}
		return d.idle.Wait(ctx, task, target, timeout)
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

	cfg, ok := d.config[task.AgentType]
	if ok && cfg.Tier == TierMarker {
		return target, cfg, TierMarker, nil
	}
	return target, cfg, TierIdle, nil
}
