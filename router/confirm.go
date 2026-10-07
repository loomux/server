package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// pendingTTL bounds how long an offer waits for its confirmation. The
// confirmation must also be the conversation's very next message (see
// pendingActions.take), so this only matters for a conversation left
// idle on an offer.
const pendingTTL = 15 * time.Minute

// installTimeout bounds how long an install command may run before the
// turn gives up waiting on it. The command itself is left running in its
// tmux session for a human to watch or attach to.
const installTimeout = 10 * time.Minute

// pendingKind is what a pending offer will do once confirmed.
type pendingKind int

const (
	// pendingInstallAgent installs an agent CLI (LOOM-71).
	pendingInstallAgent pendingKind = iota
	// pendingCloneRemote clones a repository the user didn't name, then
	// carries on with the request (LOOM-90 re-review).
	pendingCloneRemote
	// pendingRunCommand runs a shell command the user didn't write out
	// verbatim (LOOM-72).
	pendingRunCommand
	// pendingPolicyConfirm carries out a plan its target's policy made
	// wait for a "yes" (LOOM-89).
	pendingPolicyConfirm
)

// pendingInstall is an offer awaiting the user's confirmation — an agent
// install (LOOM-71), a clone of a repository the routing model chose, or
// a shell command (LOOM-72). Everything a confirmation will act on is
// fixed here when the offer is made — what will run and where — so the
// confirming message itself contributes nothing but "yes".
type pendingInstall struct {
	// id is the offer's registry.Confirmation (LOOM-123).
	id string
	// conversationID is the conversation it was offered in, for the
	// audit trail (LOOM-110).
	conversationID string
	kind           pendingKind
	agentType      string
	targetID       string
	// command is the agent-type's AgentInstall.Command, as shown in the
	// offer.
	command string
	// Exactly one of workspaceID (dispatching into an existing workspace)
	// or provision (the turn would have provisioned one) is set: the
	// install runs as a task in that workspace, provisioning it first if
	// needed.
	workspaceID string
	provision   *ProvisionSpec
	// message is, for pendingCloneRemote and pendingPolicyConfirm, the
	// request that will carry on once confirmed.
	message string
	// decision is, for pendingPolicyConfirm, the plan a "yes" carries out
	// (LOOM-89).
	decision *Decision
	expires  time.Time
}

// pendingActions holds at most one offer per conversation, in memory. A
// restart forgets every offer, which fails closed: an install only ever
// runs on a confirmation of an offer this process made.
type pendingActions struct {
	mu   sync.Mutex
	byID map[string]pendingInstall
}

func (p *pendingActions) put(conversationID string, a pendingInstall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID == nil {
		p.byID = make(map[string]pendingInstall)
	}
	p.byID[conversationID] = a
}

// take removes and returns conversationID's offer; live is false when
// there was none or it had expired (a.id then names an expired one).
// Any next message consumes the offer, confirming or not, so a "yes" can
// only ever answer the offer immediately before it.
func (p *pendingActions) take(conversationID string, now time.Time) (a pendingInstall, live bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.byID[conversationID]
	delete(p.byID, conversationID)
	return a, ok && !now.After(a.expires)
}

// peek is conversationID's live offer, left in place.
func (p *pendingActions) peek(conversationID string, now time.Time) (pendingInstall, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.byID[conversationID]
	return a, ok && !now.After(a.expires)
}

// offer registers a as conversationID's offer awaiting a yes, with c
// (Kind, target, what it would run) recorded for the web UI's card
// (LOOM-123). It replaces any earlier offer, as the turn making it is the
// conversation's next message.
func (r *Router) offer(ctx context.Context, conversationID string, a pendingInstall, c registry.Confirmation) error {
	a.id = uuid.NewString()
	a.conversationID = conversationID
	a.expires = time.Now().Add(pendingTTL)
	c.ID, c.ConversationID, c.DispatchID = a.id, conversationID, turnLogFrom(ctx).dispatchID
	c.Status, c.ExpiresAt = registry.ConfirmationPending, a.expires.UTC()
	if c.TargetID == "" {
		c.TargetID = a.targetID
	}
	if c.TargetName == "" {
		if t, err := r.store.GetTarget(ctx, c.TargetID); err == nil {
			c.TargetName = t.Name
		}
	}
	if c.AgentType == "" {
		c.AgentType = a.agentType
	}
	if err := r.store.CreateConfirmation(ctx, &c); err != nil {
		return fmt.Errorf("record offer: %w", err)
	}
	r.recordEvent(ctx, conversationID, registry.DispatchEvent{Kind: registry.EventOffer, TargetID: c.TargetID,
		Command: r.redactAllSecrets(ctx, c.Command), Outcome: c.Kind, Detail: r.redactAllSecrets(ctx, offerDetail(c))})
	r.pending.put(conversationID, a)
	return nil
}

// resolveOffer records how offer a was answered. Best effort: the record
// is for display, and the offer itself is already settled in memory.
func (r *Router) resolveOffer(ctx context.Context, log *slog.Logger, a pendingInstall, status registry.ConfirmationStatus) {
	if a.id == "" {
		return
	}
	if err := r.store.ResolveConfirmation(ctx, a.id, status); err != nil {
		log.Warn("confirmation not recorded", "confirmation_id", a.id, "status", string(status), "error", err)
	}
	r.recordEvent(ctx, a.conversationID, registry.DispatchEvent{Kind: registry.EventOfferAnswered,
		TargetID: a.targetID, Outcome: string(status), Detail: "offer " + a.id})
}

// offerDetail names what an offer is about besides its command: the
// agent, workspace or repository.
func offerDetail(c registry.Confirmation) string {
	var parts []string
	if c.AgentType != "" {
		parts = append(parts, "agent "+c.AgentType)
	}
	if c.Workspace != "" {
		parts = append(parts, "workspace "+c.Workspace)
	}
	if c.GitRemote != "" {
		remote := c.GitRemote
		// A token in the URL's userinfo isn't kept.
		if u, err := url.Parse(remote); err == nil && u.User != nil {
			u.User = nil
			remote = u.String()
		}
		parts = append(parts, "repository "+remote)
	}
	return strings.Join(append([]string{"offer " + c.ID}, parts...), "; ")
}

// staleConfirmationReply answers a card's Approve or Deny for an offer
// that's no longer awaiting one.
const staleConfirmationReply = "That request is no longer waiting for an answer: it was already answered, " +
	"cancelled by a later message, or it expired. Nothing was run. Send it again if you still want it."

// isConfirmation reports whether message is an unambiguous yes to offer p.
// Deliberately a short fixed list matched against the whole message, not
// something the routing model judges: running an install, a command the
// user didn't write, or cloning a repository they didn't name, must never
// happen on a misread. Anything else — including a "yes, but…" — declines.
func isConfirmation(message string, p pendingInstall) bool {
	m := strings.ToLower(strings.TrimSpace(message))
	m = strings.TrimRight(m, ".!")
	m = strings.Join(strings.Fields(strings.ReplaceAll(m, ",", " ")), " ")
	switch m {
	case "y", "yes", "yes please", "yep", "ok", "okay", "confirm", "confirmed", "go ahead", "do it":
		return true
	}
	switch p.kind {
	case pendingInstallAgent:
		switch m {
		case "install", "install it", "yes install", "yes install it",
			"install " + p.agentType, "yes install " + p.agentType:
			return true
		}
	case pendingCloneRemote:
		switch m {
		case "clone", "clone it", "yes clone it":
			return true
		}
	case pendingRunCommand:
		switch m {
		case "run", "run it", "yes run it":
			return true
		}
	}
	return false
}

// installOffer is the reply for an agent CLI missing from its target. With
// a recipe it shows the exact command and login step and registers the
// offer; without one it just explains.
//
// The offer discloses everything a "yes" will run (LOOM-90 review): when
// the original turn would also have provisioned a workspace, that
// provisioning recipe is shown in full alongside the install command.
func (r *Router) installOffer(ctx context.Context, conversationID string, unavailable *AgentUnavailableError, workspaceID string, provision *ProvisionSpec) (string, error) {
	entry, _ := r.agentTypes.Get(unavailable.AgentType)
	if entry.Install == nil || entry.Install.Command == "" {
		return fmt.Sprintf("%s isn't installed on %s, and Loomux has no install recipe for it. "+
			"Install it on %s yourself, then send your request again.",
			unavailable.AgentType, unavailable.TargetName, unavailable.TargetName), nil
	}
	var recipe string
	if provision != nil {
		target, err := r.store.GetTarget(ctx, unavailable.TargetID)
		if err != nil {
			return "", err
		}
		recipe = provisioningRecipe(target, *provision)
	}
	conf := registry.Confirmation{Kind: registry.ConfirmationInstallAgent, TargetName: unavailable.TargetName,
		Command: entry.Install.Command}
	if provision != nil {
		conf.Workspace = provision.Name
	}
	if err := r.offer(ctx, conversationID, pendingInstall{
		agentType:   unavailable.AgentType,
		targetID:    unavailable.TargetID,
		command:     entry.Install.Command,
		workspaceID: workspaceID,
		provision:   provision,
	}, conf); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s isn't installed on %s, so I can't start it there.\n\n", unavailable.AgentType, unavailable.TargetName)
	if recipe != "" {
		fmt.Fprintf(&b, "If you go ahead, I'll first set up workspace %s on %s by running:\n\n%s\n",
			provision.Name, unavailable.TargetName, indent(recipe))
		fmt.Fprintf(&b, "and then install %s there by running:\n\n    %s\n\n", unavailable.AgentType, entry.Install.Command)
	} else {
		fmt.Fprintf(&b, "I can install it by running this on %s:\n\n    %s\n\n", unavailable.TargetName, entry.Install.Command)
	}
	if entry.Install.Login != "" {
		fmt.Fprintf(&b, "After installing, it needs a one-time login before it can run: %s.\n\n", entry.Install.Login)
	}
	b.WriteString(`Reply "yes" to go ahead. Any other reply cancels.`)
	return b.String(), nil
}

// indent indents every line of text by four spaces, as a code block.
func indent(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// runInstall carries out a confirmed install offer: provisions the
// workspace the original turn wanted if it doesn't exist yet, runs the
// install command there as a tracked command task, then re-probes so the
// recorded availability reflects the outcome. A command that fails is a
// reply, not an error — it ran, and the user needs its output.
func (r *Router) runInstall(ctx context.Context, conversationID string, p pendingInstall) (reply, taskID string, err error) {
	target, err := r.store.GetTarget(ctx, p.targetID)
	if err != nil {
		return "", "", fmt.Errorf("install %s: %w", p.agentType, err)
	}
	workspaceID := p.workspaceID
	if workspaceID == "" {
		if workspaceID, err = r.provisionWorkspace(ctx, conversationID, *p.provision); err != nil {
			return "", "", fmt.Errorf("install %s: %w", p.agentType, err)
		}
	}

	r.logger.Info("installing agent", "conversation_id", conversationID, "target_id", target.ID,
		"workspace_id", workspaceID, "agent_type", p.agentType)
	result, err := r.runCommandTask(ctx, registry.EventCommand, workspaceID, conversationID, p.command, installTimeout)
	if err != nil {
		return "", result.taskID, fmt.Errorf("install %s: %w", p.agentType, err)
	}
	output := quoteOutput(r.redactAllSecrets(ctx, result.output))
	if result.exitCode != 0 {
		return fmt.Sprintf("Installing %s on %s failed (%s). Last output:\n\n%s",
			p.agentType, target.Name, result.ended, output), result.taskID, nil
	}

	entry, _ := r.agentTypes.Get(p.agentType)
	rec, err := r.probeAgent(ctx, target, p.agentType, entry)
	if err != nil {
		return "", result.taskID, fmt.Errorf("install %s: %w", p.agentType, err)
	}
	if !rec.Available {
		return fmt.Sprintf("The install command finished (exit 0), but %s still isn't found on %s — it may have "+
			"installed somewhere that isn't on the PATH. Last output:\n\n%s", entry.Binary, target.Name, output), result.taskID, nil
	}
	reply = fmt.Sprintf("Installed %s on %s (exit 0): %s", p.agentType, target.Name, rec.Path)
	if rec.Version != "" {
		reply += fmt.Sprintf(", %s", rec.Version)
	}
	reply += "."
	if entry.Install != nil && entry.Install.Login != "" {
		reply += fmt.Sprintf("\n\nOne more step before it can run: %s. Then send your request again.", entry.Install.Login)
	} else {
		reply += " Send your request again to use it."
	}
	return reply, result.taskID, nil
}

// commandResult is a finished command task's outcome.
type commandResult struct {
	taskID   string
	session  string
	exitCode int
	// ended says how the command ended: "exit 3", "killed by signal 9"
	// (targets.PaneExit.Describe).
	ended  string
	output string
}

// stillRunningError is runCommandTask giving up waiting on a command that
// hasn't exited; the command is left running in session.
type stillRunningError struct {
	timeout time.Duration
	session string
}

func (e *stillRunningError) Error() string {
	return fmt.Sprintf("still running after %s; left running in tmux session %s", e.timeout, e.session)
}

// runCommandTask runs command in workspaceID's directory as a one-shot
// registry.TaskKindCommand task — the command is the pane's own process,
// so its exit is the completion signal (completion.TierExit) — and
// returns its exit code and output once it has exited. A command still
// running after timeout is left running in its session and reported as an
// error naming that session; Router never kills a command it can't see
// finish.
//
// What ran, where and how it ended goes in the audit trail as kind
// (registry.EventCommand or EventProvision, LOOM-110).
func (r *Router) runCommandTask(ctx context.Context, kind, workspaceID, conversationID, command string, timeout time.Duration) (res commandResult, err error) {
	start := time.Now()
	defer func() { r.recordCommand(ctx, conversationID, kind, workspaceID, command, res, err, time.Since(start)) }()
	task, err := r.orch.Launch(ctx, workspaceID, conversationID, registry.TaskKindCommand, "", command)
	if err != nil {
		if task != nil {
			res.taskID = task.ID
		}
		return res, fmt.Errorf("launch: %w", err)
	}
	res = commandResult{taskID: task.ID, session: task.TmuxSession}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := r.orch.WaitForCompletion(waitCtx, task.ID); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return res, &stillRunningError{timeout: timeout, session: task.TmuxSession}
		}
		if ferr := r.orch.Fail(context.WithoutCancel(ctx), task.ID, taskFailure(registry.ErrorClassWaitFailed, err, "")); ferr != nil {
			r.logger.Error("command task cleanup failed", "task_id", task.ID, "error", ferr)
		}
		return res, fmt.Errorf("wait: %w", err)
	}

	exec, err := r.executorFor(ctx, task)
	if err != nil {
		return res, err
	}
	exit, err := exec.PaneExited(ctx, task.TmuxSession)
	if err != nil {
		return res, fmt.Errorf("read exit status: %w", err)
	}
	if exit == nil {
		return res, fmt.Errorf("read exit status: process in tmux session %s has not exited", task.TmuxSession)
	}
	res.exitCode, res.ended, res.output = exit.Status, exit.Describe(), exit.Output
	if err := r.orch.FinishCommand(ctx, task.ID, exit.Status); err != nil {
		return res, err
	}
	return res, nil
}
