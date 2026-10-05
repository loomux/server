package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// ProvisionTimeout bounds how long provisioning — a clone, say — may run
// before the workspace is failed (LOOM-90; LOOM-60's bounded
// provisioning). The recipe is left running in its session for a human to
// inspect.
const ProvisionTimeout = 10 * time.Minute

// NoTargetsReply is the answer to a request that needs a machine when no
// target is registered (LOOM-68): with nothing to provision a workspace
// on or run a command on, the user is told how to register one rather
// than handed a routing error.
const NoTargetsReply = "There's no machine to run that on yet: Loomux has no registered targets. " +
	"Register one first, on the Targets page or with POST /api/v1/targets, then ask again."

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
	// logger receives structured records for routing decisions,
	// provisioning and agent dispatch (LOOM-63). Never nil — New
	// defaults it to a discarding logger.
	logger *slog.Logger
	// pending holds install offers (LOOM-71) and unconfirmed shell
	// commands (LOOM-72) awaiting confirmation.
	pending pendingActions
	// commandTimeout bounds how long a direct shell command's turn waits
	// for it to exit (LOOM-72); see WithCommandTimeout.
	commandTimeout time.Duration
	// metrics records Prometheus observations for dispatches, routing
	// decisions, and task lifecycle events (LOOM-103). Never nil — New
	// defaults it to a no-op nil *metrics.Metrics.
	metrics *metrics.Metrics
	// conversations serializes turns per conversation (LOOM-83).
	conversations convLocks
	// onLateReply, if set, is told of each late reply relayed (LOOM-121);
	// see WithLateReplyHook.
	onLateReply func(task *registry.Task, reply string)
}

// Option configures optional Router behaviour (functional options, as
// api.Option).
type Option func(*Router)

// WithLogger sets the logger Router reports routing decisions,
// provisioning and dispatch to (LOOM-63). Without it, or with a nil
// logger, nothing is logged.
func WithLogger(l *slog.Logger) Option {
	return func(r *Router) {
		if l != nil {
			r.logger = l
		}
	}
}

// WithCommandTimeout sets how long a direct shell command's turn waits
// for the command to exit (LOOM-72) before replying that it's still
// running and leaving it to run. Zero or negative keeps the default.
func WithCommandTimeout(d time.Duration) Option {
	return func(r *Router) {
		if d > 0 {
			r.commandTimeout = d
		}
	}
}

// WithMetrics sets the Prometheus metrics bundle the router should
// record into (LOOM-103). A nil value is accepted and ignored.
func WithMetrics(m *metrics.Metrics) Option {
	return func(r *Router) {
		r.metrics = m
	}
}

// WithLateReplyHook sets a func told of each agent reply relayed after
// its turn ended (LOOM-121) — for a notification: no dispatch finishing
// carries it. It runs on the relaying goroutine, so it must not block.
func WithLateReplyHook(f func(task *registry.Task, reply string)) Option {
	return func(r *Router) { r.onLateReply = f }
}

// New constructs a Router. markerDir must be the same effective marker
// directory the CompletionDetector behind orch was constructed with
// (the configured LOOMUX_MARKER_DIR, or "" for each target's per-user
// default, which both resolve via completion.ResolveMarkerDir) — a mismatch would mean a TierMarker
// agent-type's hook script is told to touch a path completion.Detector
// never watches.
func New(store registry.Store, orch *orchestrator.Orchestrator, newExecutor orchestrator.ExecutorFactory,
	creds *credentials.Resolver, agentTypes AgentTypeRegistry, model RoutingModel, markerDir string, opts ...Option) *Router {
	r := &Router{
		store:          store,
		orch:           orch,
		newExecutor:    newExecutor,
		creds:          creds,
		agentTypes:     agentTypes,
		model:          model,
		markerDir:      markerDir,
		logger:         slog.New(slog.DiscardHandler),
		commandTimeout: defaultCommandTimeout,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Dispatch runs one turn: routes message, resolves or provisions a
// workspace, then finds the task already open for that workspace +
// conversation or launches a fresh one (see act), waits for
// completion, relays the captured output, and applies the result.
// Returns the chat-appropriate reply. opts is passed through to the
// routing model's Decide call unmodified (e.g. WithWorkspaceHint,
// LOOM-46) — Router itself has no opinion on what to do with them.
//
// Each stage is logged (LOOM-63): dispatch start/finish, the routing
// decision, provisioning, and the agent turn, with any failure at error
// level. The message body itself is never logged — only its length.
func (r *Router) Dispatch(ctx context.Context, conversationID, message string, opts ...DispatchOption) (string, error) {
	defer r.conversations.lock(conversationID)()
	start := time.Now()
	var o DispatchOptions
	for _, opt := range opts {
		opt(&o)
	}
	// Every stage below that logs the turn does so through logTurn, which
	// reads these back; carrying them on ctx saves threading them through
	// each of those paths.
	ctx = withTurnLog(ctx, turnLog{dispatchID: o.DispatchID, userMessageLogged: o.UserMessageLogged})
	log := r.logger.With("conversation_id", conversationID)
	if o.DispatchID != "" {
		log = log.With("dispatch_id", o.DispatchID)
	}
	log.Info("dispatch started", "message_len", len(message))

	m := &dispatchMetrics{outcome: metrics.OutcomeSuccess}
	defer func() {
		r.metrics.RecordDispatch(m.action, m.outcome, m.errClass)
		r.metrics.RecordDispatchDuration("total", time.Since(start))
	}()

	// A prompt the conversation's agent is stopped at (LOOM-97) takes
	// this message as its answer, before anything else.
	if task, err := r.attentionTask(ctx, conversationID); err != nil {
		log.Error("dispatch failed", "stage", "find prompt", "error", err)
		m.outcome, m.errClass = metrics.OutcomeFailure, classifyDispatchError(err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	} else if task != nil {
		m.action = "answer_prompt"
		reply, handled, err := r.answerAttention(ctx, log, task, message, start)
		if handled {
			if err != nil {
				m.outcome, m.errClass = metrics.OutcomeFailure, classifyDispatchError(err)
			}
			return reply, err
		}
	}

	// An install, clone, or run offer made on this conversation's
	// previous turn is answered here, before routing: the routing model
	// plays no part in deciding that these run, nor in what they run
	// (LOOM-71, LOOM-72, LOOM-90).
	// A card's Approve or Deny (LOOM-123) answers one offer: if that
	// offer isn't the one awaiting an answer any more, nothing runs and
	// the message isn't routed — a stale "yes" must not become a request.
	if id := o.ConfirmationID; id != "" {
		if p, live := r.pending.peek(conversationID, time.Now()); !live || p.id != id {
			log.Info("stale confirmation", "confirmation_id", id)
			m.action = "confirmation_stale"
			return r.finishTurn(ctx, log, conversationID, message, "", staleConfirmationReply, "confirmation_stale", start)
		}
	}
	if p, live := r.pending.take(conversationID, time.Now()); !live {
		r.resolveOffer(ctx, log, p, registry.ConfirmationExpired)
	} else {
		if isConfirmation(message, p) {
			ctx = r.withTurnOrigin(ctx, p.targetID)
			refusal, err := r.offerRefusal(ctx, p)
			if err != nil {
				// Nothing runs: its card mustn't stay pending (LOOM-133).
				r.resolveOffer(ctx, log, p, registry.ConfirmationDenied)
				log.Error("dispatch failed", "stage", "recheck policy", "error", err)
				m.outcome = metrics.OutcomeFailure
				m.errClass = classifyDispatchError(err)
				return "", fmt.Errorf("router: dispatch: policy: %w", err)
			}
			if refusal != "" {
				// Nothing runs, so its card mustn't read "Approved".
				r.resolveOffer(ctx, log, p, registry.ConfirmationDenied)
				log.Info("confirmed offer refused by target policy", "target_id", p.targetID, "agent_type", p.agentType)
				m.action = "policy_refused"
				return r.finishTurn(ctx, log, conversationID, message, "", refusal, "policy_refused", start)
			}
			// A policy confirmation is checked again by confirmPolicy, which
			// settles its record itself.
			if p.kind != pendingPolicyConfirm {
				r.resolveOffer(ctx, log, p, registry.ConfirmationApproved)
			}
			if p.kind == pendingCloneRemote || p.kind == pendingPolicyConfirm {
				tl := turnLogFrom(ctx)
				tl.carriesOut = true
				ctx = withTurnLog(ctx, tl)
			}
			switch p.kind {
			case pendingCloneRemote:
				log.Info("clone confirmed", "target_id", p.targetID)
				m.action = "clone_remote"
				return r.act(ctx, log, conversationID, p.message, Decision{
					Action: ActionProvisionWorkspace, AgentType: p.agentType, NewWorkspace: *p.provision,
				}, true, start, m)
			case pendingPolicyConfirm:
				reply, err := r.confirmPolicy(ctx, log, conversationID, p, start, m)
				if err != nil {
					m.outcome = metrics.OutcomeFailure
					m.errClass = classifyDispatchError(err)
				}
				return reply, err
			case pendingRunCommand:
				m.action = string(ActionRunCommand)
				reply, err := r.confirmCommand(ctx, log, conversationID, message, p, start)
				if err != nil {
					m.outcome = metrics.OutcomeFailure
					m.errClass = classifyDispatchError(err)
				}
				return reply, err
			default:
				m.action = "install_agent"
				reply, err := r.confirmInstall(ctx, log, conversationID, message, p, start)
				if err != nil {
					m.outcome = metrics.OutcomeFailure
					m.errClass = classifyDispatchError(err)
				}
				return reply, err
			}
		}
		r.resolveOffer(ctx, log, p, registry.ConfirmationDenied)
		log.Info("offer declined", "agent_type", p.agentType, "target_id", p.targetID)
	}

	workspaces, err := r.store.ListWorkspaces(ctx)
	if err != nil {
		log.Error("dispatch failed", "stage", "list workspaces", "error", err)
		m.outcome = metrics.OutcomeFailure
		m.errClass = classifyDispatchError(err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	targets, err := r.store.ListTargets(ctx)
	if err != nil {
		log.Error("dispatch failed", "stage", "list targets", "error", err)
		m.outcome = metrics.OutcomeFailure
		m.errClass = classifyDispatchError(err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	targetSnapshots, err := r.snapshotTargets(ctx, targets)
	if err != nil {
		log.Error("dispatch failed", "stage", "list target agents", "error", err)
		m.outcome = metrics.OutcomeFailure
		m.errClass = classifyDispatchError(err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	offered := snapshotWorkspaces(workspaces, targets)
	history, openTask, lastWS, lastWSName, err := r.conversationContext(ctx, conversationID, offered)
	if err != nil {
		log.Error("dispatch failed", "stage", "conversation context", "error", err)
		m.outcome = metrics.OutcomeFailure
		m.errClass = classifyDispatchError(err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	decideOpts := append(append([]DispatchOption(nil), opts...), withConversation(history, openTask, lastWS, lastWSName))
	var hinted DispatchOptions
	for _, opt := range opts {
		opt(&hinted)
	}
	openWS := ""
	if openTask != nil {
		openWS = openTask.WorkspaceID
	}
	if n := len(offered); n > MaxOfferedWorkspaces {
		offered = capOffered(offered, MaxOfferedWorkspaces, message, hinted.WorkspaceHint, lastWS, openWS)
		log.Debug("workspaces offered to routing capped", "workspaces", n, "offered", len(offered))
	}

	routeStart := time.Now()
	decision, err := r.model.Decide(ctx, message, offered, targetSnapshots, decideOpts...)
	r.metrics.RecordDispatchDuration("route", time.Since(routeStart))
	if err != nil {
		log.Error("routing failed", "error", err)
		m.outcome = metrics.OutcomeFailure
		m.errClass = string(registry.ErrorClassInternal)
		return "", fmt.Errorf("router: dispatch: routing failed: %w", err)
	}
	// No target means nowhere to provision or run anything (LOOM-68). The
	// routing model isn't offered either action then, but whichever model
	// is plugged in, the answer is the same clear one rather than a
	// failure further down.
	if len(targets) == 0 && (decision.Action == ActionProvisionWorkspace || decision.Action == ActionRunCommand) {
		log.Info("no targets registered", "decided_action", string(decision.Action))
		decision = Decision{Action: ActionAnswerDirectly, DirectAnswer: NoTargetsReply}
	}
	decision, affinityFrom := ApplyAffinity(decision, openTask)
	var substitutedFrom, targetName string
	// The open task's pane is already running its agent: nothing to check.
	if openTask == nil || decision.WorkspaceID != openTask.WorkspaceID {
		decision, substitutedFrom, targetName = r.checkAgentChoice(ctx, decision, message)
	}
	if substitutedFrom != "" {
		tl := turnLogFrom(ctx)
		tl.replyNote = substitutionNote(decision.AgentType, substitutedFrom, targetName)
		tl.agentNote = agentSubstitutionNote(decision.AgentType, substitutedFrom, targetName)
		ctx = withTurnLog(ctx, tl)
	}
	logDecision(log, decision, substitutedFrom, affinityFrom)
	r.metrics.RecordRoutingDecision(string(decision.Action))

	// Where the turn runs is recorded with its messages, whatever it ends
	// in.
	if target, err := r.policyTarget(ctx, decision); err == nil && target != nil {
		ctx = r.withTurnOrigin(ctx, target.ID)
	}

	// The target's policy (LOOM-89) has the last word, whatever the
	// routing model chose.
	var continuing string
	if openTask != nil && decision.Action == ActionUseWorkspace && decision.WorkspaceID == openTask.WorkspaceID {
		continuing = openTask.TaskID
	}
	if reply, handled, err := r.enforcePolicy(ctx, log, conversationID, message, decision, continuing, start); handled {
		m.action = string(decision.Action)
		if err != nil {
			m.outcome, m.errClass = metrics.OutcomeFailure, classifyDispatchError(err)
		}
		return reply, err
	}

	reply, err := r.act(ctx, log, conversationID, message, decision, false, start, m)
	if err == nil {
		reply = withReplyNote(turnLogFrom(ctx).replyNote, reply)
	}
	return reply, err
}

func withReplyNote(note, reply string) string {
	if note == "" {
		return reply
	}
	return note + "\n\n" + reply
}

// dispatchMetrics holds the labels for the top-level dispatch counter.
type dispatchMetrics struct {
	action   string
	outcome  string
	errClass string
}

// act carries out a routing decision for message. cloneConfirmed is set
// when the user has just confirmed cloning the decision's git remote
// (see askToClone), so it isn't asked about again.
func (r *Router) act(ctx context.Context, log *slog.Logger, conversationID, message string, decision Decision,
	cloneConfirmed bool, start time.Time, m *dispatchMetrics) (string, error) {
	var workspaceID string
	var err error
	switch decision.Action {
	case ActionAnswerDirectly:
		m.action = string(decision.Action)
		if err := r.logTurn(ctx, conversationID, "", message, decision.DirectAnswer); err != nil {
			log.Error("dispatch failed", "stage", "log turn", "error", err)
			m.outcome = metrics.OutcomeFailure
			m.errClass = classifyDispatchError(err)
			return "", fmt.Errorf("router: dispatch: %w", err)
		}
		log.Info("dispatch finished", "action", string(decision.Action), "duration_ms", time.Since(start).Milliseconds())
		return decision.DirectAnswer, nil

	case ActionUseWorkspace:
		workspaceID = decision.WorkspaceID
		m.action = string(decision.Action)

	case ActionProvisionWorkspace:
		if cloneConfirmed {
			m.action = "clone_remote"
		} else {
			m.action = string(decision.Action)
		}
		// The spec is the routing model's: validated before anything —
		// even an install offer that would carry it — happens (LOOM-90).
		if err := decision.NewWorkspace.Validate(); err != nil {
			log.Error("dispatch failed", "stage", "validate provisioning", "error", err)
			m.outcome = metrics.OutcomeFailure
			m.errClass = classifyDispatchError(err)
			return "", fmt.Errorf("router: dispatch: provision workspace: %w", err)
		}
		// A repository the routing model chose, rather than one the user
		// typed, is never cloned — and an agent started in it, with
		// whatever hooks its own config declares — without the user
		// confirming it (LOOM-90 re-review).
		if spec := decision.NewWorkspace; spec.Kind == ProvisionGitClone && !cloneConfirmed &&
			!strings.Contains(message, spec.GitRemote) {
			m.action = "clone_confirmation_requested"
			reply, err := r.askToClone(ctx, log, conversationID, message, decision, start)
			if err != nil {
				m.outcome = metrics.OutcomeFailure
				m.errClass = classifyDispatchError(err)
			}
			return reply, err
		}
		// Probe for the agent before writing a workspace row, so a target
		// without the agent's CLI gets an install offer instead of a
		// workspace whose first launch can only fail (LOOM-71). A
		// target_id that doesn't resolve is left to provisionWorkspace,
		// which refuses it (LOOM-64).
		if target, terr := r.store.GetTarget(ctx, decision.NewWorkspace.TargetID); terr == nil {
			if _, err := r.requireAgent(ctx, target, decision.AgentType); err != nil {
				var unavailable *AgentUnavailableError
				if errors.As(err, &unavailable) {
					spec := decision.NewWorkspace
					reply, err := r.replyWithOffer(ctx, log, conversationID, message, unavailable, "", &spec, start)
					m.action = "agent_unavailable"
					if err != nil {
						m.outcome = metrics.OutcomeFailure
						m.errClass = classifyDispatchError(err)
					}
					return reply, err
				}
				log.Error("dispatch failed", "stage", "probe agent", "error", err)
				m.outcome = metrics.OutcomeFailure
				m.errClass = classifyDispatchError(err)
				return "", fmt.Errorf("router: dispatch: %w", err)
			}
		}
		provisionStart := time.Now()
		workspaceID, err = r.provisionWorkspace(ctx, conversationID, decision.NewWorkspace)
		r.metrics.RecordDispatchDuration("provision", time.Since(provisionStart))
		if err != nil {
			m.outcome = metrics.OutcomeFailure
			m.errClass = classifyDispatchError(err)
			return "", fmt.Errorf("router: dispatch: %w", err)
		}

	case ActionRunCommand:
		m.action = string(decision.Action)
		reply, err := r.runCommand(ctx, log, conversationID, message, decision, start)
		if err != nil {
			m.outcome = metrics.OutcomeFailure
			m.errClass = classifyDispatchError(err)
		}
		return reply, err

	default:
		log.Error("dispatch failed", "stage", "route", "action", string(decision.Action), "error", "unknown decision action")
		m.outcome = metrics.OutcomeFailure
		m.errClass = string(registry.ErrorClassInternal)
		return "", fmt.Errorf("router: dispatch: unknown decision action %q", decision.Action)
	}

	agentStart := time.Now()
	reply, err := r.dispatchToAgent(ctx, workspaceID, conversationID, decision.AgentType, message)
	r.metrics.RecordDispatchDuration("agent", time.Since(agentStart))
	if err != nil {
		var unavailable *AgentUnavailableError
		if errors.As(err, &unavailable) {
			reply, err := r.replyWithOffer(ctx, log, conversationID, message, unavailable, workspaceID, nil, start)
			m.action = "agent_unavailable"
			if err != nil {
				m.outcome = metrics.OutcomeFailure
				m.errClass = classifyDispatchError(err)
			}
			return reply, err
		}
		m.outcome = metrics.OutcomeFailure
		m.errClass = classifyDispatchError(err)
		return "", err
	}
	log.Info("dispatch finished", "action", string(decision.Action), "workspace_id", workspaceID,
		"duration_ms", time.Since(start).Milliseconds())
	return reply, nil
}

// askToClone ends a turn whose decision would clone a repository the user
// didn't name, showing what would be cloned, onto which target and where,
// and registering the offer: a "yes" as the next message carries the
// original request on (LOOM-90 re-review).
func (r *Router) askToClone(ctx context.Context, log *slog.Logger, conversationID, message string, decision Decision,
	start time.Time) (string, error) {
	spec := decision.NewWorkspace
	target, err := r.store.GetTarget(ctx, spec.TargetID)
	if err != nil {
		log.Error("dispatch failed", "stage", "resolve target", "target_id", spec.TargetID, "error", err)
		return "", fmt.Errorf("router: dispatch: provision workspace: resolve target %q: %w", spec.TargetID, err)
	}
	if err := r.offer(ctx, conversationID, pendingInstall{
		kind:      pendingCloneRemote,
		agentType: decision.AgentType,
		targetID:  target.ID,
		provision: &spec,
		message:   message,
	}, registry.Confirmation{Kind: registry.ConfirmationCloneRemote, TargetName: target.Name,
		Workspace: spec.Name, GitRemote: spec.GitRemote}); err != nil {
		log.Error("dispatch failed", "stage", "record offer", "error", err)
		return "", fmt.Errorf("router: dispatch: provision workspace: %w", err)
	}
	reply := fmt.Sprintf("To do this I'd clone a repository you didn't name:\n\n    %s\n\ninto %s on %s, "+
		"and start %s there. Reply \"yes\" to go ahead. Any other reply cancels.",
		spec.GitRemote, workspaceDisplayPath(target, spec.Name), target.Name, decision.AgentType)
	if err := r.logTurn(ctx, conversationID, "", message, reply); err != nil {
		log.Error("dispatch failed", "stage", "log turn", "error", err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	log.Info("dispatch finished", "action", "clone_confirmation_requested", "duration_ms", time.Since(start).Milliseconds())
	return reply, nil
}

// replyWithOffer ends a turn whose agent isn't installed on its target
// with an install offer (or, for an agent-type with no recipe, an
// explanation) instead of an error (LOOM-71).
func (r *Router) replyWithOffer(ctx context.Context, log *slog.Logger, conversationID, message string,
	unavailable *AgentUnavailableError, workspaceID string, provision *ProvisionSpec, start time.Time) (string, error) {
	reply, err := r.installOffer(ctx, conversationID, unavailable, workspaceID, provision)
	if err != nil {
		log.Error("dispatch failed", "stage", "install offer", "error", err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	if err := r.logTurn(ctx, conversationID, "", message, reply); err != nil {
		log.Error("dispatch failed", "stage", "log turn", "error", err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	log.Info("dispatch finished", "action", "agent_unavailable", "agent_type", unavailable.AgentType,
		"target_id", unavailable.TargetID, "duration_ms", time.Since(start).Milliseconds())
	return reply, nil
}

// confirmInstall runs a confirmed install offer as the turn (LOOM-71).
func (r *Router) confirmInstall(ctx context.Context, log *slog.Logger, conversationID, message string, p pendingInstall, start time.Time) (string, error) {
	log.Info("install confirmed", "agent_type", p.agentType, "target_id", p.targetID)
	reply, taskID, err := r.runInstall(ctx, conversationID, p)
	if err != nil {
		log.Error("dispatch failed", "stage", "install agent", "task_id", taskID, "error", err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	if err := r.logTurn(ctx, conversationID, taskID, message, reply); err != nil {
		log.Error("dispatch failed", "stage", "log turn", "error", err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	log.Info("dispatch finished", "action", "install_agent", "task_id", taskID,
		"duration_ms", time.Since(start).Milliseconds())
	return reply, nil
}

// logDecision records what the routing model decided: the action plus
// whichever of workspace, agent type and new-workspace target/name it
// carries. A direct answer's text is not logged — it is chat content.
func logDecision(log *slog.Logger, d Decision, agentSubstitutedFrom, affinityOverrideFrom string) {
	attrs := []any{"action", string(d.Action)}
	if affinityOverrideFrom != "" {
		attrs = append(attrs, "affinity_override_from", affinityOverrideFrom)
	}
	if agentSubstitutedFrom != "" {
		attrs = append(attrs, "agent_substituted_from", agentSubstitutedFrom)
	}
	switch d.Action {
	case ActionUseWorkspace:
		attrs = append(attrs, "workspace_id", d.WorkspaceID, "agent_type", d.AgentType)
	case ActionProvisionWorkspace:
		attrs = append(attrs, "agent_type", d.AgentType, "target_id", d.NewWorkspace.TargetID,
			"workspace_name", d.NewWorkspace.Name)
	case ActionRunCommand:
		// The command is the user's text: only its length is logged.
		attrs = append(attrs, "target_id", d.TargetID, "command_len", len(d.Command))
	}
	log.Info("routing decision", attrs...)
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
	log := r.logger.With("conversation_id", conversationID, "target_id", spec.TargetID, "workspace_name", spec.Name)
	if spec.TargetID == "" {
		log.Error("provisioning failed", "stage", "resolve target", "error", "no target_id given")
		return "", fmt.Errorf("provision workspace: no target_id given")
	}
	target, err := r.store.GetTarget(ctx, spec.TargetID)
	if err != nil {
		log.Error("provisioning failed", "stage", "resolve target", "error", err)
		return "", fmt.Errorf("provision workspace: resolve target %q: %w", spec.TargetID, err)
	}
	// The spec comes from the routing model: validated here, before any
	// row is written, whatever the model's own validation did (LOOM-90).
	if err := spec.Validate(); err != nil {
		log.Error("provisioning failed", "stage", "validate", "error", err)
		return "", fmt.Errorf("provision workspace: %w", err)
	}
	if err := r.requireHealthyTarget(ctx, target, true); err != nil {
		log.Error("provisioning failed", "stage", "target health", "error", err)
		return "", fmt.Errorf("provision workspace: %w", err)
	}

	// Path stays empty until the recipe reports the directory it
	// resolved inside the workspace root.
	ws := &registry.Workspace{
		ID:          uuid.NewString(),
		Name:        spec.Name,
		TargetID:    spec.TargetID,
		GitRemote:   spec.GitRemote,
		Description: spec.Description,
		Tags:        spec.Tags,
		Status:      registry.WorkspaceStatusProvisioning,
		IsDynamic:   true,
	}
	if err := r.store.CreateWorkspace(ctx, ws); err != nil {
		log.Error("provisioning failed", "stage", "create workspace", "error", err)
		return "", fmt.Errorf("provision workspace: %w", err)
	}

	// From here on a failure leaves the row behind (see above), marked
	// failed (LOOM-71) — never left active or provisioning, and no longer
	// offered to the routing model. Each failure record names the row and
	// its status, re-read rather than taken from ws so it reports what
	// was actually persisted. WithoutCancel so a wait that failed on ctx
	// cancellation still gets the row marked and its status read.
	log = log.With("workspace_id", ws.ID)
	//
	// Why is recorded too (LOOM-77): on the task (reason, class, output
	// tail) and on the workspace (status_reason). A task that already
	// ended — a recipe that ran to a non-zero exit — is failed regardless.
	fail := func(stage string, taskID string, failure registry.TaskFailure, err error) {
		cleanupCtx := context.WithoutCancel(ctx)
		if taskID != "" {
			if cur, gerr := r.store.GetTask(cleanupCtx, taskID); gerr == nil && cur.Status != registry.TaskStatusFailed {
				if ferr := r.orch.Fail(cleanupCtx, taskID, failure); ferr != nil {
					log.Error("provisioning cleanup failed", "stage", "fail task", "task_id", taskID, "error", ferr)
				}
			}
		}
		if cur, gerr := r.store.GetWorkspace(cleanupCtx, ws.ID); gerr == nil {
			cur.Status = registry.WorkspaceStatusFailed
			cur.StatusReason = "provisioning failed at " + stage + ": " + failure.Reason
			if uerr := r.store.UpdateWorkspace(cleanupCtx, cur); uerr != nil {
				log.Error("provisioning cleanup failed", "stage", "mark workspace failed", "error", uerr)
			}
		}
		attrs := []any{"stage", stage, "error", err}
		if cur, gerr := r.store.GetWorkspace(cleanupCtx, ws.ID); gerr == nil {
			attrs = append(attrs, "workspace_status", string(cur.Status))
		} else {
			attrs = append(attrs, "workspace_status_error", gerr)
		}
		log.Error("provisioning failed", attrs...)
	}

	// The recipe is the pane's own process, run as a command task: its
	// exit is its completion (status 0 success, anything else a failure
	// reported in its own output), recorded with the exact script that
	// ran, and bounded — a clone that hangs fails the workspace rather
	// than the turn waiting forever (LOOM-60).
	recipe := provisioningRecipe(target, spec)
	log.Info("provisioning workspace", "kind", string(spec.Kind))
	res, err := r.runCommandTask(ctx, ws.ID, conversationID, recipe, ProvisionTimeout)
	if err != nil {
		var running *stillRunningError
		class := registry.ErrorClassWaitFailed
		if errors.As(err, &running) {
			class = registry.ErrorClassProvisionFailed
		} else if res.taskID == "" {
			class = registry.ErrorClassLaunchFailed
		}
		fail("run", res.taskID, taskFailure(class, err, ""), err)
		return "", fmt.Errorf("provision workspace: %w", err)
	}
	if res.exitCode != 0 {
		tail := quoteOutput(r.redactAllSecrets(ctx, res.output))
		err := fmt.Errorf("provisioning ended with %s: %s", res.ended, tail)
		fail("run", res.taskID, registry.TaskFailure{
			Class:      registry.ErrorClassProvisionFailed,
			Reason:     fmt.Sprintf("provisioning ended with %s", res.ended),
			OutputTail: tail,
		}, err)
		return "", fmt.Errorf("provision workspace: %w", err)
	}
	path := parseProvisionedPath(res.output)
	if path == "" {
		fail("run", res.taskID, taskFailure(registry.ErrorClassProvisionFailed, errNoProvisionedPath, ""), errNoProvisionedPath)
		return "", fmt.Errorf("provision workspace: %w", errNoProvisionedPath)
	}

	cur, err := r.store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		fail("record path", "", taskFailure(registry.ErrorClassInternal, err, ""), err)
		return "", fmt.Errorf("provision workspace: %w", err)
	}
	cur.Path = path
	cur.Status = registry.WorkspaceStatusIdle
	cur.StatusReason = ""
	if err := r.store.UpdateWorkspace(ctx, cur); err != nil {
		fail("record path", "", taskFailure(registry.ErrorClassInternal, err, ""), err)
		return "", fmt.Errorf("provision workspace: %w", err)
	}
	log.Info("workspace provisioned", "task_id", res.taskID)
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
//
// The turn is logged (LOOM-63) as "agent dispatch started"/"finished",
// or "agent dispatch failed" at error level with whatever task it had
// reached — a failure here can leave a task or workspace stuck.
func (r *Router) dispatchToAgent(ctx context.Context, workspaceID, conversationID, agentType, message string) (reply string, err error) {
	start := time.Now()
	log := r.logger.With("conversation_id", conversationID, "workspace_id", workspaceID, "agent_type", agentType)
	var taskID string
	// failClass is what a failure from here on means for the task (LOOM-77):
	// the deferred guard fails any task this turn reached that the error
	// left non-terminal, with a reason — so no error path leaves a task
	// running/awaiting-input with nothing recorded. A refusal because a
	// human has taken the task over is not a failure of the task.
	failClass := registry.ErrorClassInternal
	defer func() { r.failTurnOnError(ctx, log, taskID, failClass, err) }()

	if err := r.requireWorkspaceTargetHealthy(ctx, workspaceID); err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
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
			if err := r.orch.Fail(ctx, task.ID, registry.TaskFailure{
				Class: registry.ErrorClassSessionLost, Reason: "session no longer exists",
			}); err != nil {
				return "", fmt.Errorf("router: dispatch: %w", err)
			}
			task = nil
		}
	}
	resumed := task != nil
	promptSent := false
	agentMessage := message
	if note := turnLogFrom(ctx).agentNote; note != "" {
		agentMessage = note + "\n\n" + message
	}
	if task == nil {
		// A fresh agent hasn't seen the conversation: what was said before
		// (a clarifying question and its answer, an earlier task) goes in
		// front, so a short answer still carries the request it answers.
		earlier, err := r.earlierConversation(ctx, conversationID, workspaceID, message)
		if err != nil {
			return "", fmt.Errorf("router: dispatch: %w", err)
		}
		if earlier != "" {
			agentMessage = earlier + "\n\n" + agentMessage
		}
		task, promptSent, err = r.launchAgent(ctx, workspaceID, conversationID, agentType, agentMessage)
		if err != nil {
			return "", err
		}
	}
	taskID = task.ID
	log.Info("agent dispatch started", "task_id", task.ID, "resumed", resumed)

	// A turn is delivered by typing it into the pane — except a fresh
	// task's first turn when the agent-type's profile passes it on the
	// launch command line instead (LOOM-78): a just-started TUI may not
	// be ready for keys yet, and a startup dialog could eat them. Later
	// turns are safe to type: the previous turn's completion signal
	// shows the agent is waiting for input.
	failClass = registry.ErrorClassSendFailed
	if !promptSent {
		if entry, err := r.agentTypes.Get(task.AgentType); err == nil && entry.Tier == completion.TierMarker {
			if exec, err := r.executorFor(ctx, task); err == nil {
				r.clearTurnFiles(ctx, exec, task)
			}
		}
		if err := r.orch.SendMessage(ctx, task.ID, agentMessage); err != nil {
			return "", fmt.Errorf("router: dispatch: send message: %w", err)
		}
	}
	return r.awaitTurn(ctx, log, task, message, start, &failClass)
}

// awaitTurn waits for task's turn to finish, relays the captured output
// and applies the result: the second half of a turn, after its message
// was sent (dispatchToAgent) or its prompt answered (answerAttention).
// *failClass is kept current for the caller's failure guard.
//
// An agent stopped at a prompt (LOOM-97) ends the turn early: the task
// becomes needs-attention and the reply shows the prompt — or, for a
// sign-in screen, the task fails with login_required.
func (r *Router) awaitTurn(ctx context.Context, log *slog.Logger, task *registry.Task, message string, start time.Time,
	failClass *registry.ErrorClass) (string, error) {
	*failClass = registry.ErrorClassWaitFailed
	if err := r.orch.WaitForCompletion(ctx, task.ID); err != nil {
		var exited *orchestrator.ProcessExitedError
		if errors.As(err, &exited) {
			return "", r.agentExited(ctx, task, exited)
		}
		var timeout *orchestrator.TurnTimeoutError
		if errors.As(err, &timeout) {
			return "", r.turnTimedOut(ctx, task, timeout, time.Since(start))
		}
		var attention *orchestrator.NeedsAttentionError
		if errors.As(err, &attention) {
			*failClass = registry.ErrorClassInternal
			return r.needsAttention(ctx, log, task, message, attention.Attention)
		}
		return "", fmt.Errorf("router: dispatch: wait for completion: %w", err)
	}

	*failClass = registry.ErrorClassRelayFailed
	exec, err := r.executorFor(ctx, task)
	if err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	captured, agentMessage, err := r.turnOutput(ctx, exec, task, "")
	if err != nil {
		return "", fmt.Errorf("router: dispatch: capture pane: %w", err)
	}
	r.recordTurn(ctx, exec, task, message, agentMessage)

	result, err := r.model.Relay(ctx, r.scrubForRelay(ctx, captured))
	if err != nil {
		return "", fmt.Errorf("router: dispatch: relay: %w", err)
	}

	*failClass = registry.ErrorClassInternal
	// A reply that asks the user something isn't a finished turn, whatever
	// the relay model said: completing would tear the pane down and leave
	// the answer nowhere to go. Wrongly keeping a task open only costs an
	// idle pane until the reaper; wrongly closing one loses the session.
	if result.Done && AsksUser(result.Reply) {
		log.Info("relay done overridden: the reply asks the user something", "task_id", task.ID)
		result.Done = false
	}
	if result.Done {
		if err := r.orch.Complete(ctx, task.ID, result.Reply); err != nil {
			return "", fmt.Errorf("router: dispatch: complete: %w", err)
		}
	} else if err := r.store.SetWorkspaceRollingSummary(ctx, task.WorkspaceID, result.Reply); err != nil {
		return "", fmt.Errorf("router: dispatch: update rolling summary: %w", err)
	}

	if err := r.logTurn(ctx, task.ConversationID, task.ID, message, result.Reply); err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}

	log.Info("agent dispatch finished", "task_id", task.ID, "task_done", result.Done,
		"duration_ms", time.Since(start).Milliseconds())
	return result.Reply, nil
}

// agentExited handles an agent CLI whose process ended mid-turn (LOOM-71):
// the task is failed (its pane left for inspection, as for any failure)
// and the error quotes the process's last output — "codex: command not
// found" rather than "can't find pane" — with credential values scrubbed.
// Exit status 127 is the shell's "command not found", so it also records
// the agent as unavailable on the target: the probe found something on
// PATH that the launch shell didn't.
func (r *Router) agentExited(ctx context.Context, task *registry.Task, exited *orchestrator.ProcessExitedError) error {
	cleanupCtx := context.WithoutCancel(ctx)
	// Redact before bounding: cut first, a secret straddling the cut
	// would survive as an unrecognisable tail.
	output := quoteOutput(r.redactSecrets(cleanupCtx, task.WorkspaceID, task.AgentType, exited.Output))
	if err := r.orch.Fail(cleanupCtx, task.ID, registry.TaskFailure{
		Class:      registry.ErrorClassAgentExited,
		Reason:     fmt.Sprintf("agent %q exited with status %d before finishing the turn", task.AgentType, exited.Status),
		OutputTail: output,
	}); err != nil {
		r.logger.Error("agent dispatch cleanup failed", "task_id", task.ID, "error", err)
	}
	if exited.Status == 127 {
		if ws, err := r.store.GetWorkspace(cleanupCtx, task.WorkspaceID); err == nil {
			_ = r.store.SetTargetAgent(cleanupCtx, &registry.TargetAgent{
				TargetID: ws.TargetID, AgentType: task.AgentType, Available: false, CheckedAt: time.Now().UTC(),
			})
		}
	}
	return fmt.Errorf("router: dispatch: agent %q exited (status %d) before finishing the turn; its last output:\n%s",
		task.AgentType, exited.Status, output)
}

// turnTimedOut handles a turn that hit its bound (LOOM-76): the task is
// failed with class timeout — its pane left running, as for any failure,
// so the human the error points at can attach and see what it's stuck on
// — and the error names the agent, target, elapsed time and session.
// The target's name, never its host: this error is also logged.
func (r *Router) turnTimedOut(ctx context.Context, task *registry.Task, timeout *orchestrator.TurnTimeoutError, elapsed time.Duration) error {
	cleanupCtx := context.WithoutCancel(ctx)
	targetName := r.targetNameFor(cleanupCtx, task)
	reason := fmt.Sprintf("agent %q on %s timed out after %s: %s", task.AgentType, targetName,
		elapsed.Round(time.Second), timeout)
	if err := r.orch.Fail(cleanupCtx, task.ID, registry.TaskFailure{Class: registry.ErrorClassTimeout, Reason: reason}); err != nil {
		r.logger.Error("agent dispatch cleanup failed", "task_id", task.ID, "error", err)
	}
	// The turn is failed, so its agent mustn't carry on unsupervised
	// (LOOM-117): interrupt it, keeping the pane for a human to inspect.
	r.interruptAgent(cleanupCtx, task)
	return fmt.Errorf("router: dispatch: %s. Loomux interrupted it and kept the pane — attach on %s with `%s` to see it: %w",
		reason, targetName, targets.AttachCommand(task.TmuxSession), timeout)
}

// AsksUser reports whether a relayed reply puts a question to the user
// or says the agent is waiting on them.
func AsksUser(reply string) bool {
	if strings.Contains(reply, "?") {
		return true
	}
	lower := strings.ToLower(reply)
	for _, phrase := range []string{"waiting for your", "waiting on your", "let me know", "would you like", "do you want", "please confirm"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// interruptAgent sends the agent-type's interrupt keys to task's pane,
// best effort: a failure is logged, not returned.
func (r *Router) interruptAgent(ctx context.Context, task *registry.Task) {
	entry, err := r.agentTypes.Get(task.AgentType)
	if err != nil || len(entry.InterruptKeys) == 0 {
		return
	}
	exec, err := r.executorFor(ctx, task)
	if err != nil {
		r.logger.Error("agent not interrupted", "task_id", task.ID, "error", err)
		return
	}
	for _, key := range entry.InterruptKeys {
		if err := exec.SendKey(ctx, task.TmuxSession, key); err != nil {
			r.logger.Error("agent not interrupted", "task_id", task.ID, "error", err)
			return
		}
	}
	r.logger.Info("agent interrupted", "task_id", task.ID, "agent_type", task.AgentType)
}

// trustWorkspace runs the agent-type's TrustCommand for the workspace's
// path on its target, so the agent doesn't stop at a "do you trust this
// folder?" dialog. Best effort: if it fails the agent shows the dialog,
// which is surfaced like any other prompt.
func (r *Router) trustWorkspace(ctx context.Context, target *registry.Target, ws *registry.Workspace, entry AgentType) {
	if entry.Profile.TrustCommand == nil {
		return
	}
	// No Close: it would tear down the target's shared ssh ControlMaster
	// under every other operation multiplexed on it.
	exec, err := r.newExecutor(target)
	if err == nil {
		_, err = exec.RunOnce(ctx, entry.Profile.TrustCommand(ws.Path))
	}
	if err != nil {
		r.logger.Warn("workspace not pre-trusted", "workspace_id", ws.ID, "error", err)
	}
}

// retireStalePanes tears down the panes failed agent turns left in a
// workspace (LOOM-117) — kept for inspection, but a fresh agent launched
// next to one would be a second agent in the same directory. Each is
// recorded as reaped; the task stays failed.
func (r *Router) retireStalePanes(ctx context.Context, workspaceID string) error {
	tasks, err := r.store.ListTasksByWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Kind != registry.TaskKindAgent || t.Status != registry.TaskStatusFailed || t.ReapedAt != nil {
			continue
		}
		exec, err := r.executorFor(ctx, t)
		if err != nil {
			return err
		}
		live, err := exec.HasSession(ctx, t.TmuxSession)
		if err != nil {
			return err
		}
		if live {
			if err := exec.KillSession(ctx, t.TmuxSession); err != nil {
				return err
			}
			r.logger.Info("retired a failed turn's pane before launching", "task_id", t.ID, "workspace_id", workspaceID)
		}
		if err := r.store.SetTaskReapedAt(ctx, t.ID, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

// taskFailure builds a TaskFailure from err, classing an unreachable
// target as such whatever stage it surfaced in.
func taskFailure(class registry.ErrorClass, err error, outputTail string) registry.TaskFailure {
	if errors.Is(err, targets.ErrUnreachable) {
		class = registry.ErrorClassTargetUnreachable
	}
	return registry.TaskFailure{Class: class, Reason: err.Error(), OutputTail: outputTail}
}

func isTerminal(status registry.TaskStatus) bool {
	return status == registry.TaskStatusCompleted || status == registry.TaskStatusFailed
}

// turnLog is how a Dispatch call's messages are to be written (LOOM-80).
type turnLog struct {
	dispatchID        string
	userMessageLogged bool
	// replyNote, when set, is put in front of the reply — stored and
	// returned alike (LOOM-88's agent substitution note).
	replyNote string
	// agentNote, when set, is put in front of the message the agent
	// receives — not the one stored — telling a substitute agent why it
	// got the work.
	agentNote string
	// carriesOut is set when the turn carries out a request made earlier
	// and confirmed now (a clone or a policy confirmation): that request,
	// and the offer about it, aren't earlier conversation for the agent.
	carriesOut bool
	// origin is the purpose of the target the turn acts on, once routing
	// (or the offer being confirmed) names one: a turn that ends without
	// a task, such as an offer to run something there, still ran there.
	origin string
}

type turnLogKey struct{}

func withTurnLog(ctx context.Context, tl turnLog) context.Context {
	return context.WithValue(ctx, turnLogKey{}, tl)
}

func turnLogFrom(ctx context.Context) turnLog {
	tl, _ := ctx.Value(turnLogKey{}).(turnLog)
	return tl
}

// logTurn persists one turn's user/assistant message pair. Called only
// once a reply is actually available (design spec
// docs/design/message-logging-design.md, "Where it's written") — a
// Dispatch call that errors before producing a reply leaves no trace
// here; the caller already learns about the failure synchronously via
// Dispatch's own returned error. Under a dispatch job (LOOM-80) the user
// message was already stored at submit, so only the reply is written,
// and both carry the job's id.
func (r *Router) logTurn(ctx context.Context, conversationID, taskID, userMessage, assistantReply string) error {
	tl := turnLogFrom(ctx)
	origin := r.turnOrigin(ctx, taskID)
	if !tl.userMessageLogged {
		if err := r.store.CreateMessage(ctx, &registry.Message{
			ID:             uuid.NewString(),
			ConversationID: conversationID,
			TaskID:         taskID,
			DispatchID:     tl.dispatchID,
			Origin:         origin,
			Role:           registry.MessageRoleUser,
			Content:        userMessage,
		}); err != nil {
			return fmt.Errorf("log turn: user message: %w", err)
		}
	}
	if err := r.store.CreateMessage(ctx, &registry.Message{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		TaskID:         taskID,
		DispatchID:     tl.dispatchID,
		Origin:         origin,
		Role:           registry.MessageRoleAssistant,
		Content:        withReplyNote(tl.replyNote, assistantReply),
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
func (r *Router) launchAgent(ctx context.Context, workspaceID, conversationID, agentType, message string) (task *registry.Task, promptSent bool, err error) {
	entry, err := r.agentTypes.Get(agentType)
	if err != nil {
		return nil, false, fmt.Errorf("router: dispatch: %w", err)
	}
	ws, err := r.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, false, fmt.Errorf("router: dispatch: %w", err)
	}

	// Probe before anything else touches the target for this agent
	// (LOOM-71): an absent CLI becomes an install offer, not a session
	// that dies at once. The probe resolves the CLI's absolute path
	// (LOOM-79), which the version check and the launch then both use —
	// so the version checked is the version that runs, whatever PATH the
	// tmux session would have had.
	var resolvedPath string
	if entry.Binary != "" {
		target, err := r.store.GetTarget(ctx, ws.TargetID)
		if err != nil {
			return nil, false, fmt.Errorf("router: dispatch: %w", err)
		}
		rec, err := r.requireAgent(ctx, target, agentType)
		if err != nil {
			return nil, false, fmt.Errorf("router: dispatch: %w", err)
		}
		resolvedPath = rec.Path
	}

	if entry.VersionCheck != nil {
		vc := *entry.VersionCheck
		vc.Command = withResolvedBinary(vc.Command, entry.Binary, resolvedPath)
		if err := r.verifyAgentVersion(ctx, workspaceID, vc); err != nil {
			return nil, false, fmt.Errorf("router: dispatch: %w", err)
		}
	}

	taskID := uuid.NewString()
	secrets, err := r.creds.Resolve(ctx, workspaceID, agentType)
	if err != nil {
		return nil, false, fmt.Errorf("router: dispatch: resolve credentials: %w", err)
	}
	// Secrets go through a file the launch command sources and deletes,
	// never onto the command line (LOOM-113).
	var prefix string
	var envFile credentials.EnvFile
	if len(secrets) > 0 {
		if envFile, err = credentials.NewEnvFile(taskID, secrets); err != nil {
			return nil, false, fmt.Errorf("router: dispatch: %w", err)
		}
		exec, err := r.executorForWorkspace(ctx, workspaceID)
		if err != nil {
			return nil, false, fmt.Errorf("router: dispatch: %w", err)
		}
		if out, err := exec.RunOnce(ctx, envFile.Write); err != nil {
			// Never wrapped: RunOnce's error quotes the script it ran, and
			// this one holds the secrets. The target's own first line of
			// output says what went wrong.
			msg := "router: dispatch: could not write the agent's environment on the target"
			if line, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); line != "" {
				msg += ": " + line
			}
			// Its class and hint are kept (LOOM-85), its Detail isn't: ssh
			// also exits 255 when the remote command does, and then Detail
			// is the remote shell's stderr, which could echo the script.
			if u, ok := targets.AsUnreachable(err); ok {
				return nil, false, fmt.Errorf("%s: %w", msg, &targets.UnreachableError{Host: u.Host, Failure: u.Failure})
			}
			if errors.Is(err, targets.ErrUnreachable) {
				return nil, false, fmt.Errorf("%s: %w", msg, targets.ErrUnreachable)
			}
			return nil, false, errors.New(msg)
		}
		defer func() {
			// Sourced and deleted by the pane on a good launch; otherwise
			// nothing will, so remove it here.
			if task == nil {
				_, _ = exec.RunOnce(context.WithoutCancel(ctx), envFile.Remove)
			}
		}()
		prefix = envFile.Source
	}

	envPrefix, err := r.agentEnvPrefix(ctx, workspaceID, taskID, entry)
	if err != nil {
		return nil, false, fmt.Errorf("router: dispatch: %w", err)
	}

	if err := r.retireStalePanes(ctx, workspaceID); err != nil {
		return nil, false, fmt.Errorf("router: dispatch: retire stale panes: %w", err)
	}

	target, err := r.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return nil, false, fmt.Errorf("router: dispatch: %w", err)
	}
	r.trustWorkspace(ctx, target, ws, entry)

	prompt := ""
	if entry.Profile.PromptAsArg {
		prompt = message
	}
	task, err = r.orch.LaunchWithID(ctx, workspaceID, conversationID, registry.TaskKindAgent, agentType, taskID, prefix+envPrefix+withResolvedBinary(entry.launchCommand(ws.Path, prompt, target.PermissionMode), entry.Binary, resolvedPath))
	if err != nil {
		return nil, false, fmt.Errorf("router: dispatch: launch: %w", err)
	}
	return task, prompt != "", nil
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
func (r *Router) agentEnvPrefix(ctx context.Context, workspaceID, taskID string, entry AgentType) (string, error) {
	env := map[string]string{"LOOMUX_TASK_ID": taskID}
	if entry.Tier == completion.TierMarker {
		dir := r.markerDir
		if dir == "" {
			exec, err := r.executorForWorkspace(ctx, workspaceID)
			if err != nil {
				return "", err
			}
			if dir, err = completion.ResolveMarkerDir(ctx, exec, ""); err != nil {
				return "", err
			}
		}
		env["LOOMUX_MARKER_PATH"] = completion.MarkerPath(dir, taskID)
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
		if vc.Requires != "" && errors.Is(err, ErrVersionTooOld) {
			return fmt.Errorf("version check: agent version too old for %s: %w", vc.Requires, err)
		}
		return fmt.Errorf("version check: %w", err)
	}
	return nil
}

// findActiveTask returns the non-terminal task (if any) already open
// for this workspace + conversation. A registry.TaskStatusHumanTakeover
// task counts as active too, so a message arriving mid-takeover cleanly
// refuses via orchestrator.SendMessage's own ErrHumanTakeover rather
// than silently launching a second, conflicting session.
//
// More than one match shouldn't happen — dispatches are serialized per
// conversation (LOOM-83) — but if it does, it heals rather than wedging
// the conversation: the newest one with a live session is kept, and every
// other is failed and its pane killed (it's a second agent in the same
// directory), with a warning.
func (r *Router) findActiveTask(ctx context.Context, workspaceID, conversationID string) (*registry.Task, error) {
	tasks, err := r.store.ListTasksByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	var active []*registry.Task
	for _, t := range tasks {
		if t.ConversationID != conversationID {
			continue
		}
		switch t.Status {
		case registry.TaskStatusRunning, registry.TaskStatusAwaitingInput, registry.TaskStatusHumanTakeover,
			registry.TaskStatusNeedsAttention:
			active = append(active, t)
		}
	}
	if len(active) <= 1 {
		if len(active) == 0 {
			return nil, nil
		}
		return active[0], nil
	}
	return r.healDuplicateTasks(ctx, active)
}

// healDuplicateTasks keeps the newest of tasks whose session is live (or
// the newest outright, if none is) and retires the rest (LOOM-83).
func (r *Router) healDuplicateTasks(ctx context.Context, tasks []*registry.Task) (*registry.Task, error) {
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt) })
	keep := tasks[0]
	for _, t := range tasks {
		live, err := r.sessionIsLive(ctx, t)
		if err != nil {
			return nil, err
		}
		if live {
			keep = t
			break
		}
	}
	for _, t := range tasks {
		if t == keep {
			continue
		}
		r.logger.Warn("more than one active task; retiring the extra one", "task_id", t.ID, "kept_task_id", keep.ID,
			"workspace_id", t.WorkspaceID, "conversation_id", t.ConversationID)
		if err := r.orch.Fail(ctx, t.ID, registry.TaskFailure{
			Class: registry.ErrorClassInternal, Reason: "a second active task in the same conversation and workspace; retired",
		}); err != nil {
			return nil, err
		}
		if exec, err := r.executorFor(ctx, t); err == nil {
			if live, _ := exec.HasSession(ctx, t.TmuxSession); live {
				_ = exec.KillSession(ctx, t.TmuxSession)
			}
		}
		if err := r.store.SetTaskReapedAt(ctx, t.ID, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	return keep, nil
}

// sessionIsLive reports whether task's tracked tmux session still
// actually exists. false doesn't necessarily mean anything went wrong —
// design spec's continuation model expects a session can vanish for
// reasons outside Router's control (the idle reaper, LOOM-16; a crash; a
// human manually killing it) and treats that as recoverable via a fresh
// launch, not an error.
//
// A session whose pane process has exited counts as gone too (LOOM-71):
// panes outlive their process (remain-on-exit), so an agent that quit
// after its last turn leaves a dead pane that still "exists". That dead
// pane is killed here — its task is about to be failed and replaced, and
// typing the next message into it would only fail.
func (r *Router) sessionIsLive(ctx context.Context, task *registry.Task) (bool, error) {
	exec, err := r.executorFor(ctx, task)
	if err != nil {
		return false, err
	}
	live, err := exec.HasSession(ctx, task.TmuxSession)
	if err != nil || !live {
		return false, err
	}
	exit, err := exec.PaneExited(ctx, task.TmuxSession)
	if err != nil {
		return false, err
	}
	if exit != nil {
		if err := exec.KillSession(ctx, task.TmuxSession); err != nil {
			r.logger.Error("could not kill dead agent session", "task_id", task.ID, "error", err)
		}
		return false, nil
	}
	return true, nil
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
// shape the routing model sees: id, name, kind and recorded agent
// availability (LOOM-71) — no Host, User or SSHKeyRef, see
// TargetSnapshot.
func (r *Router) snapshotTargets(ctx context.Context, targets []*registry.Target) ([]TargetSnapshot, error) {
	out := make([]TargetSnapshot, len(targets))
	for i, t := range targets {
		agents, versions, err := r.agentAvailability(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		problem, err := r.targetProblem(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		out[i] = TargetSnapshot{
			ID:            t.ID,
			Name:          t.Name,
			Kind:          string(t.Kind),
			Agents:        agents,
			AgentVersions: versions,
			Problem:       problem,
			Policy:        t.Policy,
		}
	}
	return out, nil
}

// snapshotWorkspaces projects the workspaces the routing model may
// choose from, most recently used first (never-used last, by name): not
// failed or archived ones (LOOM-88) — nothing should be routed into a
// broken or retired workspace — and not shell workspaces (LOOM-72).
func snapshotWorkspaces(workspaces []*registry.Workspace, targets []*registry.Target) []WorkspaceSnapshot {
	targetNames := make(map[string]string, len(targets))
	for _, t := range targets {
		targetNames[t.ID] = t.Name
	}
	kept := make([]*registry.Workspace, 0, len(workspaces))
	for _, ws := range workspaces {
		if ws.Status == registry.WorkspaceStatusFailed || ws.Status == registry.WorkspaceStatusArchived || isShellWorkspace(ws) {
			continue
		}
		kept = append(kept, ws)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i].LastUsedAt, kept[j].LastUsedAt
		switch {
		case a != nil && b != nil:
			return a.After(*b)
		case a != nil || b != nil:
			return a != nil
		default:
			return kept[i].Name < kept[j].Name
		}
	})
	out := make([]WorkspaceSnapshot, 0, len(kept))
	for _, ws := range kept {
		out = append(out, WorkspaceSnapshot{
			ID:           ws.ID,
			Name:         ws.Name,
			Description:  ws.Description,
			Tags:         ws.Tags,
			Capabilities: ws.Capabilities,
			Status:       string(ws.Status),
			TargetName:   targetNames[ws.TargetID],
			Summary:      truncateRunes(ws.RollingSummary, SnapshotSummaryRunes),
			LastUsed:     ws.LastUsedAt,
		})
	}
	return out
}

// truncateRunes keeps the first n runes of s, marking a cut with "…".
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
