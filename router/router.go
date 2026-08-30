package router

import (
	"context"
	"fmt"

	"github.com/google/uuid"

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
}

// New constructs a Router.
func New(store registry.Store, orch *orchestrator.Orchestrator, newExecutor orchestrator.ExecutorFactory,
	creds *credentials.Resolver, agentTypes AgentTypeRegistry, model RoutingModel) *Router {
	return &Router{
		store:       store,
		orch:        orch,
		newExecutor: newExecutor,
		creds:       creds,
		agentTypes:  agentTypes,
		model:       model,
	}
}

// Dispatch runs one turn: routes message, resolves or provisions a
// workspace, then finds the task already open for that workspace +
// conversation or launches a fresh one (see dispatchToAgent), waits for
// completion, relays the captured output, and applies the result.
// Returns the chat-appropriate reply.
func (r *Router) Dispatch(ctx context.Context, conversationID, message string) (string, error) {
	workspaces, err := r.store.ListWorkspaces(ctx)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	decision, err := r.model.Decide(ctx, message, snapshotWorkspaces(workspaces))
	if err != nil {
		return "", fmt.Errorf("router: dispatch: routing failed: %w", err)
	}

	var workspaceID string
	switch decision.Action {
	case ActionAnswerDirectly:
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
func (r *Router) provisionWorkspace(ctx context.Context, conversationID string, spec ProvisionSpec) (string, error) {
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

	return result.Reply, nil
}

// launchAgent resolves the agent-type's launch command and applicable
// credentials, then launches a fresh agent-kind task. The chat message
// itself is sent by dispatchToAgent afterward, uniformly with the
// continuation path.
func (r *Router) launchAgent(ctx context.Context, workspaceID, conversationID, agentType string) (*registry.Task, error) {
	command, err := r.agentTypes.LaunchCommand(agentType)
	if err != nil {
		return nil, fmt.Errorf("router: dispatch: %w", err)
	}

	secrets, err := r.creds.Resolve(ctx, workspaceID, agentType)
	if err != nil {
		return nil, fmt.Errorf("router: dispatch: resolve credentials: %w", err)
	}
	prefix, err := credentials.ShellEnvPrefix(secrets)
	if err != nil {
		return nil, fmt.Errorf("router: dispatch: %w", err)
	}

	task, err := r.orch.Launch(ctx, workspaceID, conversationID, registry.TaskKindAgent, agentType, prefix+command)
	if err != nil {
		return nil, fmt.Errorf("router: dispatch: launch: %w", err)
	}
	return task, nil
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

// executorFor resolves the TargetExecutor for the target a task's
// workspace runs on — the same lookup Orchestrator.executorFor performs
// internally, necessarily duplicated here since CapturePane isn't part
// of Orchestrator's public surface.
func (r *Router) executorFor(ctx context.Context, task *registry.Task) (targets.TargetExecutor, error) {
	ws, err := r.store.GetWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return nil, err
	}
	target, err := r.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return nil, err
	}
	return r.newExecutor(target)
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
