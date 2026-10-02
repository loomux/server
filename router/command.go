package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// defaultCommandTimeout bounds how long a direct shell command's turn
// waits for it to exit (LOOM-72).
const defaultCommandTimeout = 2 * time.Minute

// Command output is relayed verbatim, so it gets roomier bounds than the
// error-message quoting in quoteOutput — but still bounded.
const (
	commandOutputMaxLines = 200
	commandOutputMaxBytes = 16000
)

// shellWorkspaceTag marks the per-target workspace direct commands run in
// (LOOM-72). Every task needs a workspace; a command addressed to a
// target rather than to a project gets this one, created on first use in
// the target's default directory and never offered to the routing model.
const shellWorkspaceTag = "loomux:shell"

func isShellWorkspace(ws *registry.Workspace) bool {
	for _, tag := range ws.Tags {
		if tag == shellWorkspaceTag {
			return true
		}
	}
	return false
}

// backtickSpan matches a ```fenced``` or `inline` code span.
var backtickSpan = regexp.MustCompile("(?s)```(?:[a-z]*\\n)?(.*?)```|`([^`]+)`")

// isVerbatim reports whether command is exactly a backtick-quoted span of
// message — the user wrote this command out themselves, delimited, so
// running it isn't the routing model's interpretation of anything. A
// command merely appearing somewhere in the message doesn't count ("don't
// run rm -rf there" contains one), nor does a command the model assembled
// from several spans or completed from one.
func isVerbatim(command, message string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	for _, m := range backtickSpan.FindAllStringSubmatch(message, -1) {
		span := m[1]
		if span == "" {
			span = m[2]
		}
		if strings.TrimSpace(span) == command {
			return true
		}
	}
	return false
}

// runCommand handles a run_command decision (LOOM-72). Safety model:
//   - the target must be a registered one (the model's target_id is not
//     trusted);
//   - the command runs at once only if it is verbatim from the user's
//     message (isVerbatim); anything else — the model wrote it, completed
//     it, or the user only described what they wanted — is shown back as
//     the exact command and runs only on an explicit confirmation as the
//     conversation's next message, matched without the routing model;
//   - output is relayed verbatim (never through the relay model), bounded,
//     with every credential value in the vault redacted.
func (r *Router) runCommand(ctx context.Context, log *slog.Logger, conversationID, message string, d Decision, start time.Time) (string, error) {
	target, err := r.store.GetTarget(ctx, d.TargetID)
	if err != nil {
		log.Error("dispatch failed", "stage", "resolve target", "target_id", d.TargetID, "error", err)
		return "", fmt.Errorf("router: dispatch: run command: resolve target %q: %w", d.TargetID, err)
	}
	command := strings.TrimSpace(d.Command)
	if command == "" {
		log.Error("dispatch failed", "stage", "run command", "error", "empty command")
		return "", fmt.Errorf("router: dispatch: run command: no command given")
	}

	if !isVerbatim(command, message) {
		r.pending.put(conversationID, pendingInstall{
			kind:     pendingRunCommand,
			targetID: target.ID,
			command:  command,
			expires:  time.Now().Add(pendingTTL),
		})
		reply := fmt.Sprintf("I'd run this on %s:\n\n    %s\n\nReply \"yes\" to run it. Any other reply cancels.",
			target.Name, command)
		return r.finishTurn(ctx, log, conversationID, message, "", reply, "command_confirmation_requested", start)
	}
	reply, taskID, err := r.executeCommand(ctx, conversationID, target, command)
	if err != nil {
		log.Error("dispatch failed", "stage", "run command", "task_id", taskID, "error", err)
		return "", fmt.Errorf("router: dispatch: run command: %w", err)
	}
	return r.finishTurn(ctx, log, conversationID, message, taskID, reply, string(ActionRunCommand), start)
}

// confirmCommand runs a shell command the user just confirmed (LOOM-72).
func (r *Router) confirmCommand(ctx context.Context, log *slog.Logger, conversationID, message string, p pendingInstall, start time.Time) (string, error) {
	log.Info("command confirmed", "target_id", p.targetID, "command_len", len(p.command))
	target, err := r.store.GetTarget(ctx, p.targetID)
	if err != nil {
		log.Error("dispatch failed", "stage", "resolve target", "error", err)
		return "", fmt.Errorf("router: dispatch: run command: %w", err)
	}
	reply, taskID, err := r.executeCommand(ctx, conversationID, target, p.command)
	if err != nil {
		log.Error("dispatch failed", "stage", "run command", "task_id", taskID, "error", err)
		return "", fmt.Errorf("router: dispatch: run command: %w", err)
	}
	return r.finishTurn(ctx, log, conversationID, message, taskID, reply, string(ActionRunCommand), start)
}

// finishTurn logs a turn that ends with reply and returns it.
func (r *Router) finishTurn(ctx context.Context, log *slog.Logger, conversationID, message, taskID, reply, action string, start time.Time) (string, error) {
	if err := r.logTurn(ctx, conversationID, taskID, message, reply); err != nil {
		log.Error("dispatch failed", "stage", "log turn", "error", err)
		return "", fmt.Errorf("router: dispatch: %w", err)
	}
	log.Info("dispatch finished", "action", action, "task_id", taskID, "duration_ms", time.Since(start).Milliseconds())
	return reply, nil
}

// executeCommand runs command on target as a command task in the
// target's shell workspace and renders the reply.
func (r *Router) executeCommand(ctx context.Context, conversationID string, target *registry.Target, command string) (reply, taskID string, err error) {
	ws, err := r.shellWorkspace(ctx, target)
	if err != nil {
		return "", "", err
	}
	res, err := r.runCommandTask(ctx, ws.ID, conversationID, command, r.commandTimeout)
	var running *stillRunningError
	if errors.As(err, &running) {
		return fmt.Sprintf("`%s` is still running on %s after %s. It's left running in tmux session %s — "+
			"attach to it to watch or stop it.", command, target.Name, running.timeout, running.session), res.taskID, nil
	}
	if err != nil {
		return "", res.taskID, err
	}

	output := boundOutput(r.redactAllSecrets(ctx, res.output), commandOutputMaxLines, commandOutputMaxBytes)
	if output == "" {
		output = "(no output)"
	}
	return fmt.Sprintf("Ran `%s` on %s — exit %d:\n\n```\n%s\n```", command, target.Name, res.exitCode, output),
		res.taskID, nil
}

// shellWorkspace returns target's shell workspace, creating it on first
// use. Its path is empty: commands run in the target's default directory
// (the login user's home).
func (r *Router) shellWorkspace(ctx context.Context, target *registry.Target) (*registry.Workspace, error) {
	name := "shell@" + target.Name
	ws, err := r.store.GetWorkspaceByName(ctx, name)
	if err == nil {
		if ws.TargetID != target.ID || !isShellWorkspace(ws) {
			return nil, fmt.Errorf("workspace %q exists but isn't %s's shell workspace", name, target.Name)
		}
		return ws, nil
	}
	if !errors.Is(err, registry.ErrNotFound) {
		return nil, err
	}
	ws = &registry.Workspace{
		ID:          uuid.NewString(),
		Name:        name,
		TargetID:    target.ID,
		Description: "Direct shell commands on " + target.Name + " (no agent)",
		Tags:        []string{shellWorkspaceTag},
		Status:      registry.WorkspaceStatusIdle,
		IsDynamic:   true,
	}
	if err := r.store.CreateWorkspace(ctx, ws); err != nil {
		if errors.Is(err, registry.ErrConflict) { // created concurrently
			return r.store.GetWorkspaceByName(ctx, name)
		}
		return nil, err
	}
	return ws, nil
}
