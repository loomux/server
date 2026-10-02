package router

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// provisioningSummary is applied via orchestrator.Complete after a
// dynamic workspace's shell-kind provisioning task goes idle. Not run
// through RoutingModel.Relay — that's for condensing agent output
// specifically (design spec §6), not a provisioning script's.
const provisioningSummary = "workspace provisioned"

// Router composes the workspace registry, orchestrator, completion
// detection (via the orchestrator it wraps), and credential vault into
// the dispatch pipeline (design spec §2, §6).
type Router struct {
	store registry.Store
	orch  *orchestrator.Orchestrator
	// newExecutor is needed independently of orch: CapturePane isn't
	// exposed through Orchestrator's public API, so Router builds its
	// own executor the same way Orchestrator does internally.
	newExecutor orchestrator.ExecutorFactory
	creds       *credentials.Resolver
	agentTypes  AgentTypeRegistry
	model       RoutingModel
	// markerDir is the same effective marker directory the real
	// completion.Detector watches (see app.build, which resolves
	// completion.MarkerDir once and passes it to both) — needed here so
	// launchAgent can compute completion.MarkerPath for a TierMarker
	// agent-type's launch env (LOOM-32) without depending on the
	// Detector itself.
	markerDir string
}

// New constructs a Router. markerDir must be the same effective marker
// directory the CompletionDetector behind orch was constructed with
// (typically completion.MarkerDir(cfg.MarkerDir), resolved once by the
// caller and passed to both) — a mismatch would mean a TierMarker
// agent-type's hook script is told to touch a path completion.Detector
// never watches.
func New(store registry.Store, orch *orchestrator.Orchestrator, newExecutor orchestrator.ExecutorFactory,
	creds *credentials.Resolver, agentTypes AgentTypeRegistry, model RoutingModel, markerDir string) *Router {
	return &Router{
		store:       store,
		orch:        orch,
		newExecutor: newExecutor,
		creds:       creds,
		agentTypes:  agentTypes,
		model:       model,
		markerDir:   markerDir,
	}
}

// Dispatch runs one turn: routes message, resolves or provisions a
// workspace, then finds the task already open for that workspace +
// conversation or launches a fresh one (see dispatchToAgent), waits for
// completion, relays the captured output, and applies the result.
// Returns the chat-appropriate reply. opts is passed through to the
// routing model's Decide call unmodified (e.g. WithWorkspaceHint,
// LOOM-46) — Router itself has no opinion on what to do with them.
func (r *Router) Dispatch(ctx context.Context, conversationID, message string, opts ...DispatchOption) (string, error) {
	workspaces, err := r.store.ListWorkspaces(ctx)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	targets, err := r.store.ListTargets(ctx)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	decision, err := r.model.Decide(ctx, message, snapshotWorkspaces(workspaces), snapshotTargets(targets), opts...)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: routing failed: %w", err)
	}

	var workspaceID string
	switch decision.Action {
	case ActionAnswerDirectly:
		if err := r.logTurn(ctx, conversationID, "", message, decision.DirectAnswer); err != nil {
			return "", fmt.Errorf("router: dispatch: %w", err)
		}
		return decision.DirectAnswer, nil

	case ActionUseWorkspace:
		workspaceID = decision.WorkspaceID

	case ActionProvisionWorkspace:
		workspaceID, err = r.provisionWorkspace(ctx, conversationID, decision.NewWorkspace)
		if err != nil {
			return "", fmt.Errorf("router: dispatch: %w", err)
		}

	default:
		return "", fmt.Errorf("router: dispatch: unknown decision action %q", decision.Action)
	}

	return r.dispatchToAgent(ctx, workspaceID, conversationID, decision.AgentType, message)
}

// provisionWorkspace creates a new workspace row and runs its setup via
// a shell-kind Launch. Design spec §2 says the row is inserted "on
// success," but orchestrator.Launch structurally requires an existing
// workspace row before it can open a session — there's no way to run
// the provisioning command before the row exists. So the row is created
// first (Status: Provisioning signals "not ready yet"); if the shell
// task fails, the row is deliberately left in place for inspection,
// matching Fail's own established philosophy elsewhere (never silently
// clean up something that went wrong). This also can't distinguish "the
// script actually succeeded" from "it failed" beyond "the pane went
// idle" — the idle heuristic has no concept of exit codes.
//
// That leave-in-place policy covers a failing provisioning *task* only.
// The target is resolved before the row is written (LOOM-64): the
// target_id comes from the routing model, which must not be trusted, and
// a row whose target doesn't exist can never be launched — it would just
// be orphaned. The Store's own referential integrity isn't relied on for
// this; registry.Store is pluggable.
func (r *Router) provisionWorkspace(ctx context.Context, conversationID string, spec ProvisionSpec) (string, error) {
	if spec.TargetID == "" {
		return "", fmt.Errorf("provision workspace: no target_id given")
	}
	if _, err := r.store.GetTarget(ctx, spec.TargetID); err != nil {
		return "", fmt.Errorf("provision workspace: resolve target %q: %w", spec.TargetID, err)
	}

	ws := &registry.Workspace{
		ID:          uuid.NewString(),
		Name:        spec.Name,
		Path:        spec.Path,
		TargetID:    spec.TargetID,
		GitRemote:   spec.GitRemote,
		Description: spec.Description,
		Tags:        spec.Tags,
		Status:      registry.WorkspaceStatusProvisioning,
		IsDynamic:   true,
	}
	if err := r.store.CreateWorkspace(ctx, ws); err != nil {
		return "", fmt.Errorf("provision workspace: %w", err)
	}

	task, err := r.orch.Launch(ctx, ws.ID, conversationID, registry.TaskKindShell, "", spec.ProvisionCommand)
	if err != nil {
		return "", fmt.Errorf("provision workspace: launch: %w", err)
	}
	if err := r.orch.WaitForCompletion(ctx, task.ID); err != nil {
		return "", fmt.Errorf("provision workspace: wait for completion: %w", err)
	}
	if err := r.orch.Complete(ctx, task.ID, provisioningSummary); err != nil {
		return "", fmt.Errorf("provision workspace: complete: %w", err)
	}
	return ws.ID, nil
}

// dispatchToAgent finds the task already open for this workspace +
// conversation, or launches a fresh one (design spec §3 step 2: "If no
// task is currently running for that workspace + conversation, the
// orchestrator opens a tmux pane... and launches the configured agent
// CLI there, interactively. If one's already running, the message is
// sent into it as the next turn."), waits for the turn's completion
// signal, relays the captured output, and applies the result — tearing
// the task down via Complete only if RelayResult.Done says the task
// itself (not just the turn) is finished (spec §3 step 3), otherwise
// just updating the rolling summary and leaving the task open
// (registry.TaskStatusAwaitingInput) for a future turn.
func (r *Router) dispatchToAgent(ctx context.Context, workspaceID, conversationID, agentType, message string) (string, error) {
	task, err := r.findActiveTask(ctx, workspaceID, conversationID)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	if task != nil {
		live, err := r.sessionIsLive(ctx, task)
		if err != nil {
			return "", fmt.Errorf("router: dispatch: %w", err)
		}
		if !live {
			// The tracked session is gone — idle-reaped (LOOM-16), crashed,
			// or manually killed. Design spec's continuation model treats
			// this as recoverable, not an error: fail the stale task so
			// findActiveTask won't keep finding it (and won't trip its
			// more-than-one-active-task guard once the fresh one below
			// exists), then fall through to a fresh launch exactly as if
			// no active task had been found at all.
			if err := r.orch.Fail(ctx, task.ID, "session no longer exists"); err != nil {
				return "", fmt.Errorf("router: dispatch: %w", err)
			}
			task = nil
		}
	}
	if task == nil {
		task, err = r.launchAgent(ctx, workspaceID, conversationID, agentType)
		if err != nil {
			return "", err
		}
	}

	// Every turn — the first as much as a follow-up — is delivered by
	// typing it into the pane: a freshly launched agent CLI runs
	// interactively (spec §3) and has nothing to act on until its first
	// message arrives, exactly like any later turn on the same task.
	if err := r.orch.SendMessage(ctx, task.ID, message); err != nil {
		return "", fmt.Errorf("router: dispatch: send message: %w", err)
	}
	if err := r.orch.WaitForCompletion(ctx, task.ID); err != nil {
		return "", fmt.Errorf("router: dispatch: wait for completion: %w", err)
	}

	exec, err := r.executorFor(ctx, task)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	captured, err := exec.CapturePane(ctx, task.TmuxSession)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: capture pane: %w", err)
	}

	result, err := r.model.Relay(ctx, captured)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: relay: %w", err)
	}

	if result.Done {
		if err := r.orch.Complete(ctx, task.ID, result.Reply); err != nil {
			return "", fmt.Errorf("router: dispatch: complete: %w", err)
		}
	} else if err := r.store.SetWorkspaceRollingSummary(ctx, workspaceID, result.Reply); err != nil {
		return "", fmt.Errorf("router: dispatch: update rolling summary: %w", err)
	}

	if err := r.logTurn(ctx, conversationID, task.ID, message, result.Reply); err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	return result.Reply, nil
}

// logTurn persists one turn's user/assistant message pair. Called only
// once a reply is actually available (design spec
// docs/design/message-logging-design.md, "Where it's written") — a
// Dispatch call that errors before producing a reply leaves no trace
// here; the caller already learns about the failure synchronously via
// Dispatch's own returned error.
func (r *Router) logTurn(ctx context.Context, conversationID, taskID, userMessage, assistantReply string) error {
	if err := r.store.CreateMessage(ctx, &registry.Message{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		TaskID:         taskID,
		Role:           registry.MessageRoleUser,
		Content:        userMessage,
	}); err != nil {
		return fmt.Errorf("log turn: user message: %w", err)
	}
	if err := r.store.CreateMessage(ctx, &registry.Message{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		TaskID:         taskID,
		Role:           registry.MessageRoleAssistant,
		Content:        assistantReply,
	}); err != nil {
		return fmt.Errorf("log turn: assistant message: %w", err)
	}
	return nil
}

// launchAgent resolves the agent-type's launch command and applicable
// credentials, verifies the agent-type's declared version range if it
// has one (design spec §10 axis 3 — before anything else, so a failing
// check never creates a task record at all: "fails loud at launch" here
// means the dispatch attempt itself fails, not a task that gets created
// then flips to Failed), then launches a fresh agent-kind task. The chat
// message itself is sent by dispatchToAgent afterward, uniformly with
// the continuation path.
//
// The task ID is minted here, before the launch command is built
// (LOOM-32) — not left to orchestrator.Launch to mint afterward — so it
// can be embedded into the command itself via agentEnvPrefix:
// LOOMUX_TASK_ID always, and for a TierMarker agent-type,
// LOOMUX_MARKER_PATH too, so a hook/notify script running inside the
// launched session can actually resolve which marker file to touch on
// completion (the gap this ticket fixes — TierMarker was previously
// declared but nonfunctional for any real deployment). Generalizes
// across every agent-type entry, including both real ones
// (app.DefaultAgentTypes' "claude-code" and "codex") — the fix lives in
// this one shared path, not per-adapter code, since neither adapter is
// more than a registry entry (see LOOM-22).
func (r *Router) launchAgent(ctx context.Context, workspaceID, conversationID, agentType string) (*registry.Task, error) {
	entry, err := r.agentTypes.Get(agentType)
	if err != nil {
		return nil, fmt.Errorf("router: dispatch: %w", err)
	}

	if entry.VersionCheck != nil {
		if err := r.verifyAgentVersion(ctx, workspaceID, *entry.VersionCheck); err != nil {
			return nil, fmt.Errorf("router: dispatch: %w", err)
		}
	}

	secrets, err := r.creds.Resolve(ctx, workspaceID, agentType)
	if err != nil {
		return nil, fmt.Errorf("router: dispatch: resolve credentials: %w", err)
	}
	prefix, err := credentials.ShellEnvPrefix(secrets)
	if err != nil {
		return nil, fmt.Errorf("router: dispatch: %w", err)
	}

	taskID := uuid.NewString()
	envPrefix, err := r.agentEnvPrefix(taskID, entry)
	if err != nil {
		return nil, fmt.Errorf("router: dispatch: %w", err)
	}

	task, err := r.orch.LaunchWithID(ctx, workspaceID, conversationID, registry.TaskKindAgent, agentType, taskID, prefix+envPrefix+entry.LaunchTemplate)
	if err != nil {
		return nil, fmt.Errorf("router: dispatch: launch: %w", err)
	}
	return task, nil
}

// agentEnvPrefix returns the shell-prefix env assignments (same
// VAR='value' shape and shell-quoting safety as credentials.
// ShellEnvPrefix, reused here rather than duplicated) that tell a
// launched agent process its own task ID — and, for a TierMarker
// agent-type, the exact marker file path a hook/notify script should
// touch on completion (LOOM-32). LOOMUX_TASK_ID is set regardless of
// tier — cheap and generically useful for any hook needing task-scoped
// behavior; LOOMUX_MARKER_PATH only when Tier == TierMarker, since it's
// meaningless otherwise and completion.MarkerWatcher never watches for
// it under any other tier.
func (r *Router) agentEnvPrefix(taskID string, entry AgentType) (string, error) {
	env := map[string]string{"LOOMUX_TASK_ID": taskID}
	if entry.Tier == completion.TierMarker {
		env["LOOMUX_MARKER_PATH"] = completion.MarkerPath(r.markerDir, taskID)
	}
	return credentials.ShellEnvPrefix(env)
}

// verifyAgentVersion runs vc.Command via a one-shot TargetExecutor.RunOnce
// against workspaceID's target, extracts a comparable version via
// vc.Parse, and checks it falls within [vc.Min, vc.Max).
func (r *Router) verifyAgentVersion(ctx context.Context, workspaceID string, vc VersionCheck) error {
	exec, err := r.executorForWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("version check: %w", err)
	}

	output, err := exec.RunOnce(ctx, vc.Command)
	if err != nil {
		return fmt.Errorf("version check: run %q: %w", vc.Command, err)
	}
	version, err := vc.Parse(output)
	if err != nil {
		return fmt.Errorf("version check: parse output of %q: %w", vc.Command, err)
	}
	if err := CheckVersionRange(version, vc.Min, vc.Max); err != nil {
		return fmt.Errorf("version check: %w", err)
	}
	return nil
}

// findActiveTask returns the non-terminal task (if any) already open
// for this workspace + conversation. A registry.TaskStatusHumanTakeover
// task counts as active too, so a message arriving mid-takeover cleanly
// refuses via orchestrator.SendMessage's own ErrHumanTakeover rather
// than silently launching a second, conflicting session. More than one
// match is a correctness violation this shouldn't be able to reach —
// surfaced as an error rather than silently picking one.
func (r *Router) findActiveTask(ctx context.Context, workspaceID, conversationID string) (*registry.Task, error) {
	tasks, err := r.store.ListTasksByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	var active *registry.Task
	for _, t := range tasks {
		if t.ConversationID != conversationID {
			continue
		}
		switch t.Status {
		case registry.TaskStatusRunning, registry.TaskStatusAwaitingInput, registry.TaskStatusHumanTakeover:
			if active != nil {
				return nil, fmt.Errorf("more than one active task for workspace %q conversation %q", workspaceID, conversationID)
			}
			active = t
		}
	}
	return active, nil
}

// sessionIsLive reports whether task's tracked tmux session still
// actually exists. false doesn't necessarily mean anything went wrong —
// design spec's continuation model expects a session can vanish for
// reasons outside Router's control (the idle reaper, LOOM-16; a crash; a
// human manually killing it) and treats that as recoverable via a fresh
// launch, not an error.
func (r *Router) sessionIsLive(ctx context.Context, task *registry.Task) (bool, error) {
	exec, err := r.executorFor(ctx, task)
	if err != nil {
		return false, err
	}
	return exec.HasSession(ctx, task.TmuxSession)
}

// executorFor resolves the TargetExecutor for the target a task's
// workspace runs on — the same lookup Orchestrator.executorFor performs
// internally, necessarily duplicated here since CapturePane isn't part
// of Orchestrator's public surface.
func (r *Router) executorFor(ctx context.Context, task *registry.Task) (targets.TargetExecutor, error) {
	return r.executorForWorkspace(ctx, task.WorkspaceID)
}

// executorForWorkspace resolves the TargetExecutor for the target a
// workspace runs on — the workspace-scoped half of executorFor, needed
// on its own by verifyAgentVersion (design spec §10 axis 3), which runs
// before any task exists.
func (r *Router) executorForWorkspace(ctx context.Context, workspaceID string) (targets.TargetExecutor, error) {
	ws, err := r.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	target, err := r.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return nil, err
	}
	return r.newExecutor(target)
}

// snapshotTargets projects registered targets into the TargetSnapshot
// shape the routing model sees: id, name and kind only (no Host, User or
// SSHKeyRef — see TargetSnapshot).
func snapshotTargets(targets []*registry.Target) []TargetSnapshot {
	out := make([]TargetSnapshot, len(targets))
	for i, t := range targets {
		out[i] = TargetSnapshot{
			ID:   t.ID,
			Name: t.Name,
			Kind: string(t.Kind),
		}
	}
	return out
}

func snapshotWorkspaces(workspaces []*registry.Workspace) []WorkspaceSnapshot {
	out := make([]WorkspaceSnapshot, len(workspaces))
	for i, ws := range workspaces {
		out[i] = WorkspaceSnapshot{
			ID:           ws.ID,
			Name:         ws.Name,
			Description:  ws.Description,
			Tags:         ws.Tags,
			Capabilities: ws.Capabilities,
		}
	}
	return out
}
