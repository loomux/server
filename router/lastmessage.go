package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// MaxRelayInput bounds what one turn hands the relay model (LOOM-91).
const MaxRelayInput = 64 << 10

// maxReplyPayload bounds how much of a completion hook's saved payload
// is read back.
const maxReplyPayload = 1 << 20

// turnOutput is what a finished turn relays (LOOM-91): the agent's own
// final message, from the payload its completion hook saved, when the
// agent-type names one and the payload has it; otherwise the pane's
// screen, as captured. agentMessage is that message unbounded, "" when
// there was none.
// claimed, when set, is a marker a late reply (LOOM-121) has claimed:
// its payload is read and left for the caller to clear once relayed.
// Otherwise the task's own payload is read and removed.
func (r *Router) turnOutput(ctx context.Context, exec targets.TargetExecutor, task *registry.Task, claimed string) (relay, agentMessage string, err error) {
	if entry, err := r.agentTypes.Get(task.AgentType); err == nil &&
		entry.Tier == completion.TierMarker && entry.LastMessageKey != "" {
		if msg := r.readLastMessage(ctx, exec, task, entry.LastMessageKey, claimed); msg != "" {
			// Bounded by the caller, after scrubbing (scrubForRelay).
			return msg, msg, nil
		}
	}
	captured, err := exec.CapturePane(ctx, task.TmuxSession)
	return captured, "", err
}

// Bounds on a stored turn (LOOM-91): how far back the pane's scrollback
// is captured, and how much of it, and of the agent's message, is kept.
const (
	turnPaneHistoryLines = 3000
	turnPaneMaxBytes     = 256 << 10
	turnMessageMaxBytes  = 256 << 10
)

// recordTurn keeps what task's finished turn produced (LOOM-91): the
// message sent, the agent's own final message and the pane's scrollback,
// credential values redacted first. Best-effort: a turn that can't be
// recorded is logged, never failed.
func (r *Router) recordTurn(ctx context.Context, exec targets.TargetExecutor, task *registry.Task, userMessage, agentMessage string) {
	log := r.logger.With("task_id", task.ID)
	pane, err := exec.RunOnce(ctx, fmt.Sprintf("tmux -L %s capture-pane -p -J -S -%d -t %s",
		targets.TmuxSocket, turnPaneHistoryLines, shellQuote(task.TmuxSession)))
	if err != nil {
		log.Warn("turn's pane not captured for its transcript", "error", err)
		pane = ""
	}
	// Redacted before bounding, as quoteOutput: a cut through a secret
	// would leave an unrecognisable piece of it.
	pane = boundOutput(r.redactSecrets(ctx, task.WorkspaceID, task.AgentType, pane), turnPaneHistoryLines, turnPaneMaxBytes)
	agentMessage = r.redactSecrets(ctx, task.WorkspaceID, task.AgentType, agentMessage)
	agentMessage = keepStart(agentMessage, turnMessageMaxBytes)
	turn := &registry.TaskTurn{ID: uuid.NewString(), TaskID: task.ID, UserMessage: userMessage,
		AgentMessage: agentMessage, Pane: pane}
	if err := r.store.CreateTaskTurn(context.WithoutCancel(ctx), turn); err != nil {
		log.Warn("turn transcript not recorded", "error", err)
	}
}

// readLastMessage reads, and removes, the payload task's completion hook
// saved — or reads, and leaves, the payload beside a claimed marker —
// returning the message under key — or "" if there is no payload
// or no message in it, logged, so the caller falls back to the pane.
func (r *Router) readLastMessage(ctx context.Context, exec targets.TargetExecutor, task *registry.Task, key, claimed string) string {
	log := r.logger.With("task_id", task.ID)
	marker, keep := claimed, claimed != ""
	if !keep {
		var err error
		if marker, err = r.markerPathOn(ctx, exec, task.ID); err != nil {
			log.Warn("agent's last message not read", "error", err)
			return ""
		}
	}
	script := `f=` + shellQuote(completion.ReplyPath(marker)) + `
[ -f "$f" ] || exit 0
head -c ` + fmt.Sprint(maxReplyPayload) + ` -- "$f"
`
	if !keep {
		script += `rm -f -- "$f"
`
	}
	out, err := exec.RunOnce(ctx, "sh -c "+shellQuote(script))
	if err != nil {
		log.Warn("agent's last message not read", "error", err)
		return ""
	}
	if strings.TrimSpace(out) == "" {
		log.Info("no last message saved; relaying the pane")
		return ""
	}
	var payload map[string]json.RawMessage
	var msg string
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		log.Warn("agent's last message unreadable; relaying the pane", "error", err)
		return ""
	}
	if raw, ok := payload[key]; !ok || json.Unmarshal(raw, &msg) != nil || strings.TrimSpace(msg) == "" {
		log.Info("no last message in the saved payload; relaying the pane", "key", key)
		return ""
	}
	return strings.TrimSpace(msg)
}

// clearTurnFiles removes a marker and payload (claimed or not) left from an earlier turn
// of task (LOOM-91) before the next is sent: one that finished after the
// turn was given up on would otherwise end — and answer — the next turn
// at once. Best-effort: logged, never fatal.
func (r *Router) clearTurnFiles(ctx context.Context, exec targets.TargetExecutor, task *registry.Task) {
	marker, err := r.markerPathOn(ctx, exec, task.ID)
	if err == nil {
		claimed := claimedMarker(marker)
		_, err = exec.RunOnce(ctx, "rm -f -- "+shellQuote(marker)+" "+shellQuote(completion.ReplyPath(marker))+
			" "+shellQuote(claimed)+" "+shellQuote(completion.ReplyPath(claimed)))
	}
	if err != nil {
		r.logger.Warn("stale turn marker not cleared", "task_id", task.ID, "error", err)
	}
}

// markerPathOn is taskID's marker path on exec's target: the configured
// marker directory, or the target user's default (as launchAgent and the
// completion detector resolve it).
func (r *Router) markerPathOn(ctx context.Context, exec targets.TargetExecutor, taskID string) (string, error) {
	dir := r.markerDir
	if dir == "" {
		var err error
		if dir, err = completion.ResolveMarkerDir(ctx, exec, ""); err != nil {
			return "", err
		}
	}
	return completion.MarkerPath(dir, taskID), nil
}

// boundRelayInput keeps the start of msg within MaxRelayInput, marking a
// cut: a final message leads with its point.
func boundRelayInput(msg string) string { return keepStart(msg, MaxRelayInput) }

// keepStart keeps the start of msg within max bytes, cut on a rune
// boundary and marked.
func keepStart(msg string, max int) string {
	if len(msg) <= max {
		return msg
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "\n[… rest of the message truncated]"
}
