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

// Dispatch runs one full turn: routes message, resolves or provisions a
// workspace, resolves the agent-type's launch command and applicable
// credentials, launches, waits for completion, relays the captured
// output, and applies it via Complete. Returns the chat-appropriate
// reply.
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

// dispatchToAgent resolves the agent-type's launch command and
// applicable credentials, launches an agent-kind task, waits for
// completion, relays the captured output, and applies it via Complete.
func (r *Router) dispatchToAgent(ctx context.Context, workspaceID, conversationID, agentType, message string) (string, error) {
	command, err := r.agentTypes.LaunchCommand(agentType)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	secrets, err := r.creds.Resolve(ctx, workspaceID, agentType)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: resolve credentials: %w", err)
	}
	prefix, err := credentials.ShellEnvPrefix(secrets)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	task, err := r.orch.Launch(ctx, workspaceID, conversationID, registry.TaskKindAgent, agentType, prefix+command)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: launch: %w", err)
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

	condensed, err := r.model.Relay(ctx, captured)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: relay: %w", err)
	}

	if err := r.orch.Complete(ctx, task.ID, condensed); err != nil {
		return "", fmt.Errorf("router: dispatch: complete: %w", err)
	}
	return condensed, nil
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
