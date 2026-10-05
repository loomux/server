package router

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// What a target health probe (LOOM-86) prints: tmux's version line (empty
// when there is no tmux) and the KiB free on the filesystem holding the
// workspace root.
const (
	healthProbeTmuxPrefix = "loomux-health-tmux:"
	healthProbeDiskPrefix = "loomux-health-disk-kb:"
)

// healthProbeTimeout bounds one health probe of one target, and
// agentProbeTimeout the re-probe of its agent CLIs that follows.
const (
	healthProbeTimeout = 20 * time.Second
	agentProbeTimeout  = time.Minute
	// healthRecheckAfter is how old an unhealthy record must be before a
	// dispatch probes again rather than trusting it: one this fresh
	// fails the dispatch at once instead of waiting out another probe.
	healthRecheckAfter = 30 * time.Second
)

// MinProvisionDiskFree is the least space the workspace root's
// filesystem must have free for a workspace to be provisioned there
// (LOOM-86): a clone into a nearly full disk fails half-way, or fills it.
const MinProvisionDiskFree = 1 << 30

// TargetUnhealthyError is a dispatch refused because the target it needs
// failed its health probe — looked at again just now, not only on the
// record (LOOM-86). Reason is the probe's, in plain language. When the
// target couldn't be reached it wraps targets.ErrUnreachable.
type TargetUnhealthyError struct {
	TargetName  string
	Reason      string
	Unreachable bool
}

func (e *TargetUnhealthyError) Error() string {
	return fmt.Sprintf("target %q can't be used right now: %s", e.TargetName, e.Reason)
}

func (e *TargetUnhealthyError) Unwrap() error {
	if e.Unreachable {
		return targets.ErrUnreachable
	}
	return nil
}

// healthProbeCommand is the POSIX sh a health probe runs on target: tmux
// -V, and df of the workspace root — or, before the first workspace
// exists, of its nearest existing parent. It always exits 0, so a target
// that answers at all is reachable.
func healthProbeCommand(target *registry.Target) string {
	root := `"$HOME"/` + defaultWorkspaceRoot
	if target.WorkspaceRoot != "" {
		root = shellQuote(target.WorkspaceRoot)
	}
	script := `d=` + root + `
while [ ! -d "$d" ]; do n=$(dirname -- "$d"); [ "$n" = "$d" ] && break; d=$n; done
echo "` + healthProbeTmuxPrefix + `$(tmux -V 2>/dev/null | head -n 1)"
echo "` + healthProbeDiskPrefix + `$(df -Pk -- "$d" 2>/dev/null | awk 'NR==2 {print $4}')"
exit 0
`
	return "sh -c " + shellQuote(script)
}

// parseHealthProbe reads healthProbeCommand's output; diskFree is -1 when
// df didn't say.
func parseHealthProbe(out string) (tmux string, diskFree int64) {
	diskFree = -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, healthProbeTmuxPrefix):
			tmux = strings.TrimSpace(strings.TrimPrefix(line, healthProbeTmuxPrefix))
		case strings.HasPrefix(line, healthProbeDiskPrefix):
			if kb, err := strconv.ParseInt(strings.TrimPrefix(line, healthProbeDiskPrefix), 10, 64); err == nil && kb >= 0 {
				diskFree = kb * 1024
			}
		}
	}
	return tmux, diskFree
}

// ProbeTarget probes targetID's health and records it (LOOM-86) and, if
// the target answered, re-probes its agent CLIs too. An unreachable or
// broken target is a result (TargetHealth.Error says why), not an error;
// an unknown target is registry.ErrNotFound.
func (r *Router) ProbeTarget(ctx context.Context, targetID string) (*registry.TargetHealth, []*registry.TargetAgent, error) {
	target, err := r.store.GetTarget(ctx, targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("router: probe target: %w", err)
	}
	h, err := r.probeHealth(ctx, target)
	if err != nil {
		return nil, nil, fmt.Errorf("router: probe target: %w", err)
	}
	agents := []*registry.TargetAgent{}
	if h.Reachable {
		// Bounded: a CLI hanging on --version or its auth check, with no
		// timeout(1) on the target, mustn't stall the periodic probe of
		// every other target.
		agentCtx, cancel := context.WithTimeout(ctx, agentProbeTimeout)
		agents, err = r.RefreshTargetAgents(agentCtx, targetID)
		cancel()
		if err != nil {
			r.logger.Warn("target agent probe failed", "target_id", targetID, "error", err)
			agents = []*registry.TargetAgent{}
		}
	}
	return h, agents, nil
}

// ProbeAllTargets probes every registered target in turn (LOOM-86): the
// periodic health check.
func (r *Router) ProbeAllTargets(ctx context.Context) {
	list, err := r.store.ListTargets(ctx)
	if err != nil {
		r.logger.Error("target health probe: list targets", "error", err)
		return
	}
	for _, t := range list {
		if ctx.Err() != nil {
			return
		}
		if _, _, err := r.ProbeTarget(ctx, t.ID); err != nil {
			r.logger.Error("target health probe failed", "target_id", t.ID, "error", err)
		}
	}
}

// probeHealth runs the health probe on target and records the result,
// and the target's gauges. It errors only if ctx ends first (nothing is
// recorded) or the result can't be stored.
func (r *Router) probeHealth(ctx context.Context, target *registry.Target) (*registry.TargetHealth, error) {
	h := &registry.TargetHealth{TargetID: target.ID, DiskFreeBytes: -1}
	start := time.Now()
	exec, err := r.newExecutor(target)
	if err == nil {
		probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
		var out string
		out, err = exec.RunOnce(probeCtx, healthProbeCommand(target))
		cancel()
		if err == nil {
			h.Reachable = true
			h.TmuxVersion, h.DiskFreeBytes = parseHealthProbe(out)
			if h.TmuxVersion == "" {
				h.Error = "tmux is not installed (or not on the PATH), so nothing can run there"
			}
		}
	}
	h.Latency = time.Since(start)
	// A probe cut short by its caller says nothing about the target:
	// recording it would mark a healthy target broken.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		h.Error = probeFailureReason(err)
	}
	h.ProbedAt = time.Now().UTC()
	if err := r.store.SetTargetHealth(context.WithoutCancel(ctx), h); err != nil {
		return nil, fmt.Errorf("record health of target %q: %w", target.Name, err)
	}
	r.metrics.SetTargetUp(target.Name, string(target.Kind), h.Reachable)
	r.metrics.SetTargetDiskFree(target.Name, h.DiskFreeBytes)
	r.logger.Info("target probed", "target_id", target.ID, "reachable", h.Reachable, "error", h.Error,
		"latency_ms", h.Latency.Milliseconds(), "tmux", h.TmuxVersion, "disk_free_bytes", h.DiskFreeBytes)
	return h, nil
}

// probeFailureReason is why a health probe couldn't run, in plain words.
func probeFailureReason(err error) string {
	if u, ok := targets.AsUnreachable(err); ok {
		if u.Failure == targets.SSHOther && u.Detail != "" {
			return "unreachable: " + u.Hint() + ": " + u.Detail
		}
		return "unreachable: " + u.Hint()
	}
	if errors.Is(err, targets.ErrUnreachable) {
		reason := strings.TrimPrefix(err.Error(), targets.ErrUnreachable.Error())
		reason = strings.TrimSpace(strings.TrimPrefix(reason, ":"))
		if reason == "" {
			return "unreachable"
		}
		return "unreachable: " + reason
	}
	return "the health check failed: " + err.Error()
}

// healthProblem is why h makes its target unfit for a dispatch — for a
// new workspace (provision) as well when provision is set — or "" if it
// isn't.
func healthProblem(h *registry.TargetHealth, provision bool) string {
	if h.Error != "" {
		return h.Error
	}
	if provision && h.DiskFreeBytes >= 0 && h.DiskFreeBytes < MinProvisionDiskFree {
		return fmt.Sprintf("only %s free on the workspace root's disk; a new workspace needs at least %s",
			formatBytes(h.DiskFreeBytes), formatBytes(MinProvisionDiskFree))
	}
	return ""
}

// requireHealthyTarget refuses a dispatch to target if its last health
// probe found it unfit (LOOM-86) and a fresh probe agrees, with a
// *TargetUnhealthyError saying why. A record under healthRecheckAfter
// old is trusted as it stands. A target recorded healthy, or never
// probed, costs nothing: the dispatch itself finds out. If the record
// can't be read, the dispatch goes ahead — health is advice, not a lock.
func (r *Router) requireHealthyTarget(ctx context.Context, target *registry.Target, provision bool) error {
	h, err := r.store.GetTargetHealth(ctx, target.ID)
	if errors.Is(err, registry.ErrNotFound) {
		return nil
	}
	if err != nil {
		r.logger.Warn("read target health", "target_id", target.ID, "error", err)
		return nil
	}
	if healthProblem(h, provision) == "" {
		return nil
	}
	// It may have recovered since: unless it was just checked, look
	// again before refusing.
	if time.Since(h.ProbedAt) < healthRecheckAfter {
		return &TargetUnhealthyError{TargetName: target.Name, Reason: healthProblem(h, provision), Unreachable: !h.Reachable}
	}
	if h, err = r.probeHealth(ctx, target); err != nil {
		r.logger.Warn("re-probe target health", "target_id", target.ID, "error", err)
		return nil
	}
	if problem := healthProblem(h, provision); problem != "" {
		return &TargetUnhealthyError{TargetName: target.Name, Reason: problem, Unreachable: !h.Reachable}
	}
	return nil
}

// requireWorkspaceTargetHealthy is requireHealthyTarget for the target
// workspaceID runs on.
func (r *Router) requireWorkspaceTargetHealthy(ctx context.Context, workspaceID string) error {
	ws, err := r.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	target, err := r.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return err
	}
	return r.requireHealthyTarget(ctx, target, false)
}

// targetProblem is the routing snapshot's view of a target's recorded
// health: what's wrong with it, or "" if nothing (or never probed).
func (r *Router) targetProblem(ctx context.Context, targetID string) (string, error) {
	h, err := r.store.GetTargetHealth(ctx, targetID)
	if errors.Is(err, registry.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	problem := healthProblem(h, false)
	if problem == "" {
		return "", nil
	}
	// Its age, so the model can weigh a stale record: work sent there
	// anyway is re-checked before it is refused.
	return fmt.Sprintf("%s (checked %s ago)", problem, time.Since(h.ProbedAt).Round(time.Minute)), nil
}

// formatBytes renders n in the largest binary unit that keeps it >= 1.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
