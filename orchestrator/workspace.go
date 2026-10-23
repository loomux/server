package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Loomux/server/registry"
)

// DeleteWorkspace deletes a workspace and every task in it (LOOM-70),
// then kills those tasks' tmux sessions. The workspace's files on the
// target are left alone. It returns the sessions it couldn't kill — the
// target unreachable, say — which the orphan sweep removes later, as no
// task owns them any more. A credential still scoped to the workspace
// is a *registry.ConflictError, and then nothing is deleted or killed —
// as is a task mid-turn (cancel it or let it finish first), a task a
// person has taken over (release it first), or the
// workspace still provisioning (the reaper fails one that is stuck).
func (o *Orchestrator) DeleteWorkspace(ctx context.Context, id string) ([]string, error) {
	return o.deleteWorkspace(ctx, id, false)
}

// DeleteWorkspaceAndWorktree is DeleteWorkspace for a workspace that is
// a git worktree (LOOM-228, git_worktree), opted into at teardown: once
// nothing blocks the delete, its worktree is removed from the target
// first (`git worktree remove`; its branch stays, with whatever was
// committed there), then the rows. git refuses a worktree with changes
// not committed or untracked files, and a directory that is no worktree
// is refused too: either is a *registry.ConflictError carrying git's or
// the check's words, and then nothing is deleted. A directory already
// gone counts as removed.
func (o *Orchestrator) DeleteWorkspaceAndWorktree(ctx context.Context, id string) ([]string, error) {
	return o.deleteWorkspace(ctx, id, true)
}

func (o *Orchestrator) deleteWorkspace(ctx context.Context, id string, removeWorktree bool) ([]string, error) {
	ws, err := o.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: delete workspace: %w", err)
	}
	if ws.Status == registry.WorkspaceStatusProvisioning {
		return nil, &registry.ConflictError{Reason: "the workspace is still provisioning; if it is stuck it is marked failed soon, and can be deleted then"}
	}
	tasks, err := o.store.ListTasksByWorkspace(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: delete workspace: %w", err)
	}
	for _, t := range tasks {
		if t.Status == registry.TaskStatusRunning {
			return nil, &registry.ConflictError{Reason: "a task in this workspace is mid-turn; cancel it or wait for it to finish"}
		}
		if t.Status == registry.TaskStatusHumanTakeover {
			return nil, &registry.ConflictError{Reason: "someone has taken over a task in this workspace; release it first"}
		}
	}
	if removeWorktree {
		if err := o.removeWorktree(ctx, ws); err != nil {
			return nil, err
		}
	}
	if err := o.store.DeleteWorkspaceAndTasks(ctx, id); err != nil {
		if errors.Is(err, registry.ErrConflict) {
			return nil, &registry.ConflictError{Reason: "a credential is still scoped to this workspace; delete it first"}
		}
		return nil, fmt.Errorf("orchestrator: delete workspace: %w", err)
	}

	var sessions []string
	for _, t := range tasks {
		if t.TmuxSession != "" && t.ReapedAt == nil {
			sessions = append(sessions, t.TmuxSession)
		}
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	target, err := o.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return sessions, nil
	}
	exec, err := o.newExecutor(target)
	if err != nil {
		return sessions, nil
	}
	var left []string
	for _, s := range sessions {
		live, err := exec.HasSession(ctx, s)
		if err == nil && !live {
			continue
		}
		if err != nil || exec.KillSession(ctx, s) != nil {
			left = append(left, s)
		}
	}
	return left, nil
}

// ReopenWorkspace puts a failed or archived workspace back in service
// (LOOM-70): idle, its status reason cleared, so the router offers it
// again — but only if its directory exists on the target: reopening one
// without it would just route agents into a missing path. A missing
// directory or a workspace still provisioning is a
// *registry.ConflictError; an unreachable target is the executor's
// error. One already idle or active is left as it is.
func (o *Orchestrator) ReopenWorkspace(ctx context.Context, id string) error {
	ws, err := o.store.GetWorkspace(ctx, id)
	if err != nil {
		return fmt.Errorf("orchestrator: reopen workspace: %w", err)
	}
	switch ws.Status {
	case registry.WorkspaceStatusIdle, registry.WorkspaceStatusActive:
		return nil
	case registry.WorkspaceStatusProvisioning:
		return &registry.ConflictError{Reason: "the workspace is still provisioning"}
	}
	target, err := o.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return fmt.Errorf("orchestrator: reopen workspace: %w", err)
	}
	exec, err := o.newExecutor(target)
	if err != nil {
		return fmt.Errorf("orchestrator: reopen workspace: %w", err)
	}
	out, err := exec.RunOnce(ctx, "test -d "+shellQuote(ws.Path)+" && echo present || echo missing")
	if err != nil {
		return fmt.Errorf("orchestrator: reopen workspace: %w", err)
	}
	if strings.TrimSpace(out) != "present" {
		return &registry.ConflictError{Reason: fmt.Sprintf("its directory %s doesn't exist on the target", ws.Path)}
	}
	return o.setWorkspaceStatus(ctx, id, ws.Status, registry.WorkspaceStatusIdle)
}

// ArchiveWorkspace takes an idle or failed workspace out of service
// (LOOM-70): the router no longer offers it, but its tasks and history
// stay. One with a running task (active) or still provisioning is a
// *registry.ConflictError; one already archived is left as it is.
func (o *Orchestrator) ArchiveWorkspace(ctx context.Context, id string) error {
	ws, err := o.store.GetWorkspace(ctx, id)
	if err != nil {
		return fmt.Errorf("orchestrator: archive workspace: %w", err)
	}
	switch ws.Status {
	case registry.WorkspaceStatusArchived:
		return nil
	case registry.WorkspaceStatusActive, registry.WorkspaceStatusProvisioning:
		return &registry.ConflictError{Reason: fmt.Sprintf("the workspace is %s", ws.Status)}
	}
	return o.setWorkspaceStatus(ctx, id, ws.Status, registry.WorkspaceStatusArchived)
}

// setWorkspaceStatus moves workspace id from status from to to, clearing
// its status reason. The row is re-read just before the write, so one
// that changed status meanwhile is a *registry.ConflictError, not
// overwritten.
func (o *Orchestrator) setWorkspaceStatus(ctx context.Context, id string, from, to registry.WorkspaceStatus) error {
	cur, err := o.store.GetWorkspace(ctx, id)
	if err != nil {
		return fmt.Errorf("orchestrator: set workspace status: %w", err)
	}
	if cur.Status != from {
		return &registry.ConflictError{Reason: fmt.Sprintf("the workspace just became %s", cur.Status)}
	}
	cur.Status = to
	cur.StatusReason = ""
	if err := o.store.UpdateWorkspace(ctx, cur); err != nil {
		return fmt.Errorf("orchestrator: set workspace status: %w", err)
	}
	return nil
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// worktreeRemoval is the script that removes the git worktree at path
// (LOOM-228): it finds the repository from the worktree itself, so
// nothing about its base need be recorded, and refuses a directory that
// is a repository's main checkout (removing that would be deleting the
// repository). Exit 0 when removed or already gone.
func worktreeRemoval(path string) string {
	return "p=" + shellQuote(path) + "\n" +
		`[ -e "$p" ] || { echo "loomux: $p is already gone"; exit 0; }` + "\n" +
		`gd=$(git -C "$p" rev-parse --path-format=absolute --git-dir 2>/dev/null) &&` +
		` common=$(git -C "$p" rev-parse --path-format=absolute --git-common-dir 2>/dev/null) ||` +
		` { echo "loomux: $p isn't a git checkout"; exit 3; }` + "\n" +
		`[ "$gd" != "$common" ] || { echo "loomux: $p is a repository's own checkout, not a worktree"; exit 4; }` + "\n" +
		`git --git-dir="$common" worktree remove -- "$p"` + "\n"
}

// removeWorktree removes ws's worktree from its target, before its rows
// go. A credential still scoped to it is checked first, so a delete the
// store would refuse doesn't lose the worktree.
func (o *Orchestrator) removeWorktree(ctx context.Context, ws *registry.Workspace) error {
	if ws.Path == "" {
		return &registry.ConflictError{Reason: "the workspace has no directory recorded, so there is no worktree to remove; delete it without remove_worktree"}
	}
	creds, err := o.store.ListCredentialInfo(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: delete workspace: %w", err)
	}
	for _, c := range creds {
		if c.WorkspaceID == ws.ID {
			return &registry.ConflictError{Reason: "a credential is still scoped to this workspace; delete it first"}
		}
	}
	target, err := o.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return fmt.Errorf("orchestrator: delete workspace: %w", err)
	}
	exec, err := o.newExecutor(target)
	if err != nil {
		return fmt.Errorf("orchestrator: delete workspace: %w", err)
	}
	out, err := exec.RunOnce(ctx, worktreeRemoval(ws.Path))
	if err == nil {
		return nil
	}
	// The script's own refusals, and git's ("fatal: … contains modified
	// or untracked files"), are the person's to act on; anything else is
	// the target not answering.
	if words := strings.TrimSpace(out); strings.Contains(words, "loomux:") || strings.Contains(words, "fatal:") || strings.Contains(words, "error:") {
		return &registry.ConflictError{Reason: "couldn't remove the worktree " + ws.Path + ": " + lastLines(words, 3) +
			"; commit or discard its changes there, or delete the workspace without remove_worktree"}
	}
	return fmt.Errorf("orchestrator: remove worktree %s: %w", ws.Path, err)
}

// lastLines is s's last n lines, joined by "; ".
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
