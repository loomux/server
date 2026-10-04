package router_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/targets"
)

// healthScript answers target health probes (LOOM-86) and hands anything
// else to the agent probe script.
type healthScript struct {
	tmux        string
	diskKB      string
	unreachable bool
	probes      int
}

func (h *availabilityHarness) scriptHealth(hs *healthScript) {
	h.exec.runOnce = func(command string) (string, error) {
		if hs.unreachable {
			return "", fmt.Errorf("%w: ssh: connect to host jet01 port 22: Connection timed out", targets.ErrUnreachable)
		}
		if strings.Contains(command, router.HealthProbeTmuxPrefix) {
			hs.probes++
			return router.HealthProbeTmuxPrefix + hs.tmux + "\n" + router.HealthProbeDiskPrefix + hs.diskKB + "\n", nil
		}
		return h.probes.runOnce(command)
	}
}

func (h *availabilityHarness) recordHealth(t *testing.T, th registry.TargetHealth) {
	t.Helper()
	th.TargetID = h.target.ID
	if err := h.store.SetTargetHealth(context.Background(), &th); err != nil {
		t.Fatalf("SetTargetHealth: %v", err)
	}
}

func (h *availabilityHarness) health(t *testing.T) *registry.TargetHealth {
	t.Helper()
	got, err := h.store.GetTargetHealth(context.Background(), h.target.ID)
	if err != nil {
		t.Fatalf("GetTargetHealth: %v", err)
	}
	return got
}

func (h *availabilityHarness) existingWorkspace(t *testing.T) *registry.Workspace {
	t.Helper()
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return ws
}

// A probe records reachability, tmux version and disk free, and refreshes
// the target's agent CLIs while it's there.
func TestProbeTarget_RecordsHealthAndAgents(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")
	h.scriptHealth(&healthScript{tmux: "tmux 3.4", diskKB: "1048576"})

	got, agents, err := h.r.ProbeTarget(context.Background(), h.target.ID)
	if err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}
	if !got.Reachable || got.Error != "" || got.TmuxVersion != "tmux 3.4" || got.DiskFreeBytes != 1<<30 || got.ProbedAt.IsZero() {
		t.Errorf("ProbeTarget = %+v, want reachable, tmux 3.4, 1 GiB free", got)
	}
	if stored := h.health(t); stored.TmuxVersion != "tmux 3.4" || stored.DiskFreeBytes != 1<<30 {
		t.Errorf("stored = %+v, want the probe recorded", stored)
	}
	if len(agents) != 2 || h.agentRecord(t, "claude-code") == nil || !h.agentRecord(t, "claude-code").Available {
		t.Errorf("agents = %+v, want both agent types re-probed, claude-code available", agents)
	}
	if _, _, err := h.r.ProbeTarget(context.Background(), "no-such-target"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("ProbeTarget(unknown) err = %v, want ErrNotFound", err)
	}
}

// An unreachable target is recorded as such, with why — a result, not an
// error — and its agents aren't probed.
func TestProbeTarget_Unreachable(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.scriptHealth(&healthScript{unreachable: true})

	got, agents, err := h.r.ProbeTarget(context.Background(), h.target.ID)
	if err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}
	if got.Reachable || !strings.Contains(got.Error, "Connection timed out") || got.DiskFreeBytes != -1 {
		t.Errorf("ProbeTarget = %+v, want unreachable with the ssh reason", got)
	}
	if len(agents) != 0 || h.agentRecord(t, "claude-code") != nil {
		t.Errorf("agents probed on an unreachable target: %+v", agents)
	}
	if stored := h.health(t); stored.Reachable {
		t.Errorf("stored = %+v, want unreachable", stored)
	}
}

// Reachable but without tmux, a target can't run anything: unhealthy.
func TestProbeTarget_NoTmux(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.scriptHealth(&healthScript{tmux: "", diskKB: "1048576"})

	got, _, err := h.r.ProbeTarget(context.Background(), h.target.ID)
	if err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}
	if !got.Reachable || !strings.Contains(got.Error, "tmux") {
		t.Errorf("ProbeTarget = %+v, want reachable but unhealthy: no tmux", got)
	}
}

// A target last seen unreachable is looked at again before a dispatch is
// refused; still down, the dispatch fails at once, saying why, without
// trying to launch anything.
func TestDispatch_RecordedUnreachable_FailsFastWithReason(t *testing.T) {
	h := newAvailabilityHarness(t)
	ws := h.existingWorkspace(t)
	h.recordHealth(t, registry.TargetHealth{Reachable: false, Error: "no answer", DiskFreeBytes: -1,
		ProbedAt: time.Now().Add(-time.Minute)})
	hs := &healthScript{unreachable: true}
	h.scriptHealth(hs)
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	_, err := h.r.Dispatch(context.Background(), "conv-1", "go")
	var unhealthy *router.TargetUnhealthyError
	if !errors.As(err, &unhealthy) || !errors.Is(err, targets.ErrUnreachable) {
		t.Fatalf("Dispatch err = %v, want a TargetUnhealthyError wrapping ErrUnreachable", err)
	}
	if !strings.Contains(err.Error(), "jet01") || !strings.Contains(err.Error(), "Connection timed out") {
		t.Errorf("err = %q, want the target and the fresh probe's reason", err)
	}
	if got := router.ClassifyError(err); got != registry.ErrorClassTargetUnreachable {
		t.Errorf("class = %q, want target_unreachable", got)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 0 {
		t.Errorf("launched %v on an unreachable target", cmds)
	}
}

// Recovered since the last probe: the re-probe records it healthy and the
// dispatch goes ahead.
func TestDispatch_RecordedUnreachable_RecoveredGoesAhead(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	ws := h.existingWorkspace(t)
	h.recordHealth(t, registry.TargetHealth{Reachable: false, Error: "no answer", DiskFreeBytes: -1,
		ProbedAt: time.Now().Add(-time.Minute)})
	hs := &healthScript{tmux: "tmux 3.4", diskKB: "1048576"}
	h.scriptHealth(hs)
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if hs.probes != 1 {
		t.Errorf("health probes = %d, want one re-probe", hs.probes)
	}
	if got := h.health(t); !got.Reachable || got.Error != "" {
		t.Errorf("health after recovery = %+v, want reachable", got)
	}
}

// A target found down moments ago isn't probed again: the dispatch fails
// at once on the recorded reason, rather than waiting out another probe.
func TestDispatch_RecentlyUnreachable_NoReprobe(t *testing.T) {
	h := newAvailabilityHarness(t)
	ws := h.existingWorkspace(t)
	h.recordHealth(t, registry.TargetHealth{Reachable: false, Error: "no answer", DiskFreeBytes: -1,
		ProbedAt: time.Now().Add(-5 * time.Second)})
	hs := &healthScript{tmux: "tmux 3.4", diskKB: "1048576"}
	h.scriptHealth(hs)
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	_, err := h.r.Dispatch(context.Background(), "conv-1", "go")
	var unhealthy *router.TargetUnhealthyError
	if !errors.As(err, &unhealthy) || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("Dispatch err = %v, want a TargetUnhealthyError with the recorded reason", err)
	}
	if hs.probes != 0 {
		t.Errorf("health probes = %d, want none for a record seconds old", hs.probes)
	}
}

// A healthy (or never probed) target costs a dispatch no extra probe.
func TestDispatch_HealthyTarget_NoReprobe(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	ws := h.existingWorkspace(t)
	h.recordHealth(t, registry.TargetHealth{Reachable: true, TmuxVersion: "tmux 3.4", DiskFreeBytes: 1 << 40})
	hs := &healthScript{tmux: "tmux 3.4", diskKB: "1"}
	h.scriptHealth(hs)
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if hs.probes != 0 {
		t.Errorf("health probes = %d, want none for a healthy target", hs.probes)
	}
}

// Provisioning is refused on a target whose workspace root is nearly full
// — before any workspace row is written.
func TestDispatch_ProvisionRefusedOnLowDisk(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	h.recordHealth(t, registry.TargetHealth{Reachable: true, TmuxVersion: "tmux 3.4", DiskFreeBytes: 100 << 20})
	h.scriptHealth(&healthScript{tmux: "tmux 3.4", diskKB: fmt.Sprint(100 << 10)})
	h.decide(h.provisionDecision("codex"))

	_, err := h.r.Dispatch(context.Background(), "conv-1", "go")
	var unhealthy *router.TargetUnhealthyError
	if !errors.As(err, &unhealthy) || errors.Is(err, targets.ErrUnreachable) {
		t.Fatalf("Dispatch err = %v, want a TargetUnhealthyError (not unreachable)", err)
	}
	if !strings.Contains(err.Error(), "free") {
		t.Errorf("err = %q, want it to say the disk is nearly full", err)
	}
	if got := router.ClassifyError(err); got != registry.ErrorClassTargetUnhealthy {
		t.Errorf("class = %q, want target_unhealthy", got)
	}
	if ws := h.workspaces(t); len(ws) != 0 {
		t.Errorf("workspaces = %+v, want none written", ws)
	}
}

// Low disk doesn't stop work in a workspace that already exists.
func TestDispatch_LowDisk_ExistingWorkspaceStillUsable(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	ws := h.existingWorkspace(t)
	h.recordHealth(t, registry.TargetHealth{Reachable: true, TmuxVersion: "tmux 3.4", DiskFreeBytes: 100 << 20})
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
}

// run_command on a target that is still down fails fast too.
func TestRunCommand_RecordedUnreachable_FailsFast(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.recordHealth(t, registry.TargetHealth{Reachable: false, Error: "no answer", DiskFreeBytes: -1})
	h.scriptHealth(&healthScript{unreachable: true})
	h.decide(router.Decision{Action: router.ActionRunCommand, TargetID: h.target.ID, Command: "uptime"})

	_, err := h.r.Dispatch(context.Background(), "conv-1", "run `uptime` on jet01")
	var unhealthy *router.TargetUnhealthyError
	if !errors.As(err, &unhealthy) {
		t.Fatalf("Dispatch err = %v, want a TargetUnhealthyError", err)
	}
}

// A probe its caller cancels records nothing: it says nothing about the
// target.
func TestProbeTarget_CancelledRecordsNothing(t *testing.T) {
	h := newAvailabilityHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.exec.runOnce = func(string) (string, error) {
		cancel()
		return "", context.Canceled
	}
	if _, _, err := h.r.ProbeTarget(ctx, h.target.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("ProbeTarget err = %v, want context.Canceled", err)
	}
	if _, err := h.store.GetTargetHealth(context.Background(), h.target.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a cancelled probe was recorded: err = %v", err)
	}
}

// The routing model is told what's wrong with a target, so it can avoid
// it or say so.
func TestDispatch_TargetSnapshotCarriesHealthProblem(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.recordHealth(t, registry.TargetHealth{Reachable: false, Error: "no answer", DiskFreeBytes: -1,
		ProbedAt: time.Now().UTC()})
	h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"})
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got := h.model.LastDecideTargets
	if len(got) != 1 || !strings.Contains(got[0].Problem, "no answer") || !strings.Contains(got[0].Problem, "checked 0s ago") {
		t.Errorf("snapshot = %+v, want the target's problem", got)
	}
}
