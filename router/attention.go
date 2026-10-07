package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// answerSettle is how long an agent's pane must stay unchanged, after an
// answer that ends its turn without a completion signal (a denied
// permission, a cancelled question), for the turn to count as over
// (LOOM-97).
const answerSettle = 3 * time.Second

// needsAttention handles an agent stopped at a prompt (LOOM-97). A
// sign-in screen fails the task fast with login_required — Loomux never
// signs an agent in — keeping the pane so a human can attach and finish
// the login. A usage-limit error (LOOM-109) fails it with
// agent_rate_limited, saying when the limit resets. Anything else leaves the task needs-attention with the
// prompt recorded, and the reply shows the prompt and how to answer it;
// the conversation's next message is the answer (answerAttention).
func (r *Router) needsAttention(ctx context.Context, log *slog.Logger, task *registry.Task, message string,
	a *registry.Attention) (string, error) {
	cleanupCtx := context.WithoutCancel(ctx)
	targetName := r.targetNameFor(cleanupCtx, task)
	if a.Kind == registry.AttentionLogin {
		reason := fmt.Sprintf("agent %q on %s isn't signed in", task.AgentType, targetName)
		if err := r.orch.Fail(cleanupCtx, task.ID, registry.TaskFailure{
			Class: registry.ErrorClassLoginRequired, Reason: reason, OutputTail: a.Detail,
		}); err != nil {
			log.Error("agent dispatch cleanup failed", "task_id", task.ID, "error", err)
		}
		log.Warn("agent needs a login", "task_id", task.ID)
		return "", &classedError{class: registry.ErrorClassLoginRequired, msg: fmt.Sprintf(
			"router: dispatch: %s (it shows %q). Loomux doesn't sign agents in: attach on %s with "+
				"`%s` and finish the login there, or store an API key credential for %s, then send your message again",
			reason, a.Detail, targetName, targets.AttachCommand(task.TmuxSession), task.AgentType)}
	}
	if a.Kind == registry.AttentionUsageLimit {
		reason := fmt.Sprintf("agent %q on %s hit its usage limit", task.AgentType, targetName)
		if a.Resets != "" {
			reason += ", resets " + a.Resets
		}
		if err := r.orch.Fail(cleanupCtx, task.ID, registry.TaskFailure{
			Class: registry.ErrorClassAgentRateLimited, Reason: reason, OutputTail: a.Detail,
		}); err != nil {
			log.Error("agent dispatch cleanup failed", "task_id", task.ID, "error", err)
		}
		log.Warn("agent hit its usage limit", "task_id", task.ID, "resets", a.Resets)
		return "", &classedError{class: registry.ErrorClassAgentRateLimited, msg: fmt.Sprintf(
			"router: dispatch: %s (it shows %q). Send your message again after the reset, or "+
				"ask for a different agent", reason, a.Detail)}
	}
	if err := r.orch.NeedAttention(ctx, task.ID, a); err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	reply := describeAttention(task.AgentType, targetName, a)
	if err := r.logTurn(ctx, task.ConversationID, task.ID, message, reply); err != nil {
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	log.Info("agent needs attention", "task_id", task.ID, "attention_kind", string(a.Kind))
	return reply, nil
}

// describeAttention is the chat reply for a prompt: what the agent asks,
// its options, and how to answer.
func describeAttention(agentType, targetName string, a *registry.Attention) string {
	var b strings.Builder
	switch a.Kind {
	case registry.AttentionPermission:
		fmt.Fprintf(&b, "%s on %s needs your approval", agentType, targetName)
	case registry.AttentionTrust:
		fmt.Fprintf(&b, "%s on %s asks whether to trust this folder", agentType, targetName)
	default:
		fmt.Fprintf(&b, "%s on %s is asking you something", agentType, targetName)
	}
	b.WriteString(".\n")
	if a.Title != "" {
		fmt.Fprintf(&b, "\n%s", a.Title)
	}
	if a.Detail != "" {
		for _, l := range strings.Split(a.Detail, "\n") {
			fmt.Fprintf(&b, "\n    %s", l)
		}
	}
	if a.Question != "" {
		fmt.Fprintf(&b, "\n%s", a.Question)
	}
	for i, o := range a.Options {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, o.Label)
		if o.Description != "" {
			fmt.Fprintf(&b, " (%s)", o.Description)
		}
	}
	b.WriteString("\n\n")
	switch a.Kind {
	case registry.AttentionPermission:
		b.WriteString(`Reply "approve" or "deny", pick an option by its number, or say what it should do instead.`)
	case registry.AttentionTrust:
		b.WriteString(`Reply "approve" to trust it or "deny" (the agent then exits).`)
	default:
		b.WriteString("Reply with an option's number, or type your own answer.")
	}
	return b.String()
}

// attentionTask is conversationID's task stopped at a prompt, if any.
func (r *Router) attentionTask(ctx context.Context, conversationID string) (*registry.Task, error) {
	tasks, err := r.store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	var found *registry.Task
	for _, t := range tasks {
		if t.ConversationID == conversationID && t.Status == registry.TaskStatusNeedsAttention && t.Attention != nil &&
			(found == nil || t.UpdatedAt.After(found.UpdatedAt)) {
			found = t
		}
	}
	return found, nil
}

// answerAttention takes message as the answer to the prompt task is
// stopped at (LOOM-97): it types the matching keys into the pane and
// carries on with the turn from there, as awaitTurn. handled is false
// when the prompt can no longer be answered — its session is gone — and
// the message should be routed as any other.
func (r *Router) answerAttention(ctx context.Context, log *slog.Logger, task *registry.Task, message string,
	start time.Time) (reply string, handled bool, err error) {
	log = log.With("workspace_id", task.WorkspaceID, "task_id", task.ID, "agent_type", task.AgentType)
	failClass := registry.ErrorClassInternal
	defer func() {
		if handled {
			r.failTurnOnError(ctx, log, task.ID, failClass, err)
		}
	}()

	live, err := r.sessionIsLive(ctx, task)
	if err != nil {
		return "", true, fmt.Errorf("router: answer prompt: %w", err)
	}
	if !live {
		if err := r.orch.Fail(ctx, task.ID, registry.TaskFailure{
			Class: registry.ErrorClassSessionLost, Reason: "session no longer exists",
		}); err != nil {
			return "", true, fmt.Errorf("router: answer prompt: %w", err)
		}
		log.Info("prompt's session is gone; routing the message instead")
		return "", false, nil
	}

	plan, problem := planAnswer(task.Attention, parseAnswer(message))
	if problem != "" {
		// Not an answer this prompt takes: say so, and keep waiting.
		if err := r.logTurn(ctx, task.ConversationID, task.ID, message, problem); err != nil {
			return "", true, fmt.Errorf("router: answer prompt: %w", err)
		}
		return problem, true, nil
	}

	failClass = registry.ErrorClassSendFailed
	exec, err := r.executorFor(ctx, task)
	if err != nil {
		return "", true, fmt.Errorf("router: answer prompt: %w", err)
	}
	for _, s := range plan.steps {
		if s.text != "" {
			err = exec.SendKeys(ctx, task.TmuxSession, s.text, false)
		} else {
			err = exec.SendKey(ctx, task.TmuxSession, s.key)
		}
		if err != nil {
			return "", true, fmt.Errorf("router: answer prompt: send keys: %w", err)
		}
	}
	if err := r.orch.Resume(ctx, task.ID); err != nil {
		return "", true, fmt.Errorf("router: answer prompt: %w", err)
	}
	log.Info("prompt answered", "answer", plan.answer)

	turnCtx := ctx
	if plan.settle {
		turnCtx = orchestrator.WithSettle(ctx, answerSettle)
	}
	if plan.followUp != "" {
		// The answer ended the turn (a denied permission); the user's
		// words are the next one, once the agent is ready for them.
		failClass = registry.ErrorClassWaitFailed
		if err := r.orch.WaitForCompletion(turnCtx, task.ID); err != nil {
			return "", true, fmt.Errorf("router: answer prompt: wait for the agent: %w", err)
		}
		failClass = registry.ErrorClassSendFailed
		if err := r.orch.SendMessage(ctx, task.ID, plan.followUp); err != nil {
			return "", true, fmt.Errorf("router: answer prompt: send message: %w", err)
		}
		turnCtx = ctx
	}
	reply, err = r.awaitTurn(turnCtx, log, task, message, start, &failClass)
	return reply, true, err
}

// failTurnOnError fails taskID when a turn on it ended in err and left it
// non-terminal (LOOM-77), so no error path leaves a task running with
// nothing recorded — unless a human has taken the task over, or the
// server is shutting down underneath the turn (LOOM-80: the agent may
// still finish, and startup reconciliation can pick the task back up).
func (r *Router) failTurnOnError(ctx context.Context, log *slog.Logger, taskID string, class registry.ErrorClass, err error) {
	var unavailable *AgentUnavailableError
	if err == nil || errors.As(err, &unavailable) {
		return
	}
	log.Error("agent dispatch failed", "task_id", taskID, "error", err)
	if taskID == "" || errors.Is(err, orchestrator.ErrHumanTakeover) {
		return
	}
	if ClassifyError(err) == registry.ErrorClassMessageTooLarge {
		// Refused before anything reached the pane (LOOM-111).
		return
	}
	if errors.Is(context.Cause(ctx), orchestrator.ErrInterrupted) {
		return
	}
	cleanupCtx := context.WithoutCancel(ctx)
	cur, gerr := r.store.GetTask(cleanupCtx, taskID)
	if gerr != nil || isTerminal(cur.Status) || cur.Status == registry.TaskStatusHumanTakeover {
		return
	}
	// Cancelled by the user (LOOM-99): that's the turn's end, and its
	// agent mustn't carry on unsupervised. The pane is kept to inspect.
	failure := taskFailure(class, err, "")
	cancelled := errors.Is(context.Cause(ctx), orchestrator.ErrCancelled)
	if cancelled {
		failure = registry.TaskFailure{Class: registry.ErrorClassCancelled, Reason: "cancelled by the user"}
	}
	if ferr := r.orch.Fail(cleanupCtx, taskID, failure); ferr != nil {
		log.Error("agent dispatch cleanup failed", "task_id", taskID, "error", ferr)
	}
	if cancelled {
		r.interruptAgent(cleanupCtx, cur)
	}
}

// targetNameFor is the name of the target task's workspace is on — its
// name, never its host: what's built from it is also logged.
func (r *Router) targetNameFor(ctx context.Context, task *registry.Task) string {
	if ws, err := r.store.GetWorkspace(ctx, task.WorkspaceID); err == nil {
		if target, err := r.store.GetTarget(ctx, ws.TargetID); err == nil {
			return target.Name
		}
	}
	return "its target"
}

// answerKind is what a message says to do with a prompt.
type answerKind int

const (
	answerReply answerKind = iota
	answerApprove
	answerDeny
	answerOption
)

type attentionAnswer struct {
	kind   answerKind
	option int // 1-based, for answerOption
	text   string
}

var (
	approveWords = []string{"yes", "y", "approve", "approved", "allow", "ok", "okay", "go ahead", "proceed", "sure", "yes please", "do it"}
	denyWords    = []string{"no", "n", "deny", "denied", "reject", "don't", "do not", "stop", "cancel", "no thanks"}
)

// parseAnswer reads a message as an answer to a prompt: a yes or no
// word, an option's number, or anything else as the user's own words.
// Deterministic on purpose — the routing model plays no part in what
// keys reach an agent's prompt.
func parseAnswer(message string) attentionAnswer {
	text := strings.TrimSpace(message)
	word := strings.TrimRight(strings.ToLower(text), ".! ")
	for _, w := range approveWords {
		if word == w {
			return attentionAnswer{kind: answerApprove}
		}
	}
	for _, w := range denyWords {
		if word == w {
			return attentionAnswer{kind: answerDeny}
		}
	}
	if n, err := strconv.Atoi(word); err == nil {
		return attentionAnswer{kind: answerOption, option: n}
	}
	return attentionAnswer{kind: answerReply, text: text}
}

// keyStep is one thing typed into a pane: a named key, or literal text.
type keyStep struct {
	key  string
	text string
}

type answerPlan struct {
	answer string // for the log: approve, deny, option N, reply
	steps  []keyStep
	// settle: the answer may end the turn without a completion signal.
	settle bool
	// followUp is sent as the next turn once the answer's turn is over.
	followUp string
}

// planAnswer works out the keys that answer a with ans. Prompts are
// option lists with a cursor, in both Claude Code and Codex, so an
// option is chosen by moving the cursor to it and pressing Enter. A
// non-empty problem is the reply when ans isn't an answer a takes.
func planAnswer(a *registry.Attention, ans attentionAnswer) (plan answerPlan, problem string) {
	find := func(prefix string) int {
		for i, o := range a.Options {
			if strings.HasPrefix(strings.ToLower(o.Label), prefix) {
				return i
			}
		}
		return -1
	}
	choose := func(i int) []keyStep {
		return append(moveCursor(a.Selected, i), keyStep{key: "Enter"})
	}
	deny := func() []keyStep {
		if i := find("no"); i >= 0 {
			return choose(i)
		}
		return []keyStep{{key: "Escape"}}
	}
	switch ans.kind {
	case answerApprove:
		i := find("yes")
		if i < 0 {
			return plan, "That prompt has no option to approve. Pick one by its number."
		}
		return answerPlan{answer: "approve", steps: choose(i)}, ""
	case answerDeny:
		return answerPlan{answer: "deny", steps: deny(), settle: true}, ""
	case answerOption:
		if ans.option < 1 || ans.option > len(a.Options) {
			return plan, fmt.Sprintf("There's no option %d. Pick one from 1 to %d.", ans.option, len(a.Options))
		}
		label := strings.ToLower(a.Options[ans.option-1].Label)
		if strings.HasPrefix(label, "type something") {
			return plan, "Type your answer instead of picking that option."
		}
		return answerPlan{answer: "option " + strconv.Itoa(ans.option), steps: choose(ans.option - 1),
			settle: strings.HasPrefix(label, "no")}, ""
	}
	switch a.Kind {
	case registry.AttentionTrust:
		return plan, `Answer "approve" to trust the folder or "deny".`
	case registry.AttentionQuestion:
		if i := find("type something"); i >= 0 {
			steps := append(moveCursor(a.Selected, i), keyStep{text: ans.text}, keyStep{key: "Enter"})
			return answerPlan{answer: "reply", steps: steps}, ""
		}
		return answerPlan{answer: "reply", steps: []keyStep{{key: "Escape"}}, settle: true, followUp: ans.text}, ""
	}
	// A permission answered in words: deny it, and tell the agent what
	// to do instead.
	return answerPlan{answer: "reply", steps: deny(), settle: true, followUp: ans.text}, ""
}

// moveCursor is the arrow keys that move a prompt's cursor from option
// from to option to.
func moveCursor(from, to int) []keyStep {
	var steps []keyStep
	for ; from < to; from++ {
		steps = append(steps, keyStep{key: "Down"})
	}
	for ; from > to; from-- {
		steps = append(steps, keyStep{key: "Up"})
	}
	return steps
}

// CancelTask cancels a running task that no dispatch is driving (LOOM-99)
// — one a restart left behind: it is failed with class cancelled and its
// agent interrupted, the pane kept to inspect. A task under a running
// dispatch is cancelled through the dispatch instead, which ends the turn
// the same way. orchestrator.ErrTaskInactive if it has already ended;
// orchestrator.ErrHumanTakeover if a person has taken it over, as nothing
// may be typed into a pane they are driving (spec §4).
func (r *Router) CancelTask(ctx context.Context, taskID string) error {
	task, err := r.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if isTerminal(task.Status) {
		return orchestrator.ErrTaskInactive
	}
	if task.Status == registry.TaskStatusHumanTakeover {
		return orchestrator.ErrHumanTakeover
	}
	if err := r.orch.Fail(ctx, task.ID, registry.TaskFailure{Class: registry.ErrorClassCancelled, Reason: "cancelled by the user"}); err != nil {
		return err
	}
	r.interruptAgent(ctx, task)
	r.logger.Info("task cancelled", "task_id", task.ID)
	return nil
}

// cliFinalLimitMessage is what Claude Code reports as the turn's last
// message when its account hits the limit: the old "Claude AI usage limit
// reached|<epoch>" or "Claude usage limit reached. …". Nothing looser: a
// one-line answer that merely starts with "5-hour limit reached …" is an
// answer (#222 review).
var cliFinalLimitMessage = regexp.MustCompile(`^(?:Claude AI usage limit reached\|\d{10}|Claude usage limit reached\.[^\n]*)$`)

// usageLimitAtEnd is the usage-limit error a finished turn ended on, if
// any (LOOM-109). With no reply from the agent, that's read off the final
// screen. A reply is checked only when it's one line and nothing else,
// the shape of the CLI reporting the error itself as its last message
// (marked here as the CLI's own error line, which the pattern requires),
// so an answer that quotes a limit message among other text is never
// failed for it. Only this kind: the login markers are plain words a
// reply could contain.
func usageLimitAtEnd(detect func(string) *registry.Attention, screen, agentMessage string) *registry.Attention {
	if msg := strings.TrimSpace(agentMessage); msg != "" {
		if !cliFinalLimitMessage.MatchString(msg) {
			return nil
		}
		screen = "⎿ " + msg
	}
	if a := detect(screen); a != nil && a.Kind == registry.AttentionUsageLimit {
		return a
	}
	return nil
}
