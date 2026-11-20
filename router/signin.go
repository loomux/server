package router

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

var (
	// ErrUnknownAgentType is returned for an agent type this server
	// doesn't have.
	ErrUnknownAgentType = errors.New("router: unknown agent type")
	// ErrNoSignIn is returned when an agent type has no sign-in command
	// to open (LOOM-316).
	ErrNoSignIn = errors.New("router: agent type has no sign-in")
)

// SignInResult is what PrepareSignIn did (LOOM-316).
type SignInResult struct {
	// Agent is the agent's record after the probe, its sign-in session
	// included.
	Agent *registry.TargetAgent
	// Opened says a sign-in session was opened now; Reused that the one
	// already open was handed back. Neither: the agent is signed in.
	Opened bool
	Reused bool
}

// SignInSessionName is the tmux session agentType's sign-in runs in on a
// target (LOOM-316): one per agent type, with the prefix every Loomux
// session has, so the orphan sweep (LOOM-93) takes an abandoned one. The
// agent type is reduced to [a-z0-9-], never interpolated as is.
func SignInSessionName(agentType string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(agentType) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return targets.SessionPrefix + "signin-" + b.String()
}

// signInCommand is the sign-in session's command: the CLI's own login
// through the probed path, then — whatever became of it — the user's
// login shell, so a failed or cancelled login can be retried by hand in
// the same attach. Fixed configuration and a probed path only: nothing a
// caller or the routing model supplies.
func signInCommand(path string, args []string) string {
	var b strings.Builder
	b.WriteString(shellQuote(path))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	b.WriteString("\necho\n")
	b.WriteString("echo 'loomux: the sign-in command has ended. This shell is yours: run it again here, or exit to close this session.'\n")
	b.WriteString(`exec "${SHELL:-/bin/sh}" -l` + "\n")
	return b.String()
}

// authWordPattern is what a sign-in method or plan must look like to be
// kept (LOOM-316): one short word, so whatever a CLI prints, no free text
// (an email, an organisation's name) reaches the store or the API.
var authWordPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// authWord is s lower-cased if it is such a word, else "".
func authWord(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !authWordPattern.MatchString(s) {
		return ""
	}
	return s
}

// signInProblemFor is the fixed word for the sign-in screen a turn
// stopped at, from the line the detector matched (LOOM-316). Never the
// line itself.
func signInProblemFor(detail string) string {
	l := strings.ToLower(detail)
	switch {
	case strings.Contains(l, "expired"):
		return registry.AgentSignInExpired
	case strings.Contains(l, "invalid"):
		return registry.AgentSignInInvalid
	default:
		return registry.AgentSignInSignedOut
	}
}

// lockSignIn holds targetID + agentType's sign-in lock until the returned
// func is called.
func (r *Router) lockSignIn(targetID, agentType string) (unlock func()) {
	v, _ := r.signInLocks.LoadOrStore(targetID+"\x00"+agentType, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// PrepareSignIn opens agentType's sign-in on targetID for a person to
// attach to (LOOM-316): the agent is probed; signed in by its own check,
// with no turn having seen a sign-in screen since, nothing is opened
// unless restart. Otherwise its sign-in session is opened — or, already
// open, handed back as is, so a half-done login is never killed by a
// second caller; restart replaces it. Loomux never types into it.
//
// An agent type unknown or without a sign-in is ErrUnknownAgentType or
// ErrNoSignIn, an unknown target registry.ErrNotFound, a CLI that isn't
// installed an *AgentUnavailableError.
func (r *Router) PrepareSignIn(ctx context.Context, targetID, agentType string, restart bool) (*SignInResult, error) {
	entry, err := r.agentTypes.Get(agentType)
	if err != nil {
		return nil, err
	}
	if entry.SignIn == nil || entry.Binary == "" {
		return nil, fmt.Errorf("%w: %q", ErrNoSignIn, agentType)
	}
	target, err := r.store.GetTarget(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("router: prepare sign-in: %w", err)
	}
	unlock := r.lockSignIn(targetID, agentType)
	defer unlock()
	rec, err := r.probeAgentWith(ctx, target, agentType, entry, true)
	if err != nil {
		return nil, fmt.Errorf("router: prepare sign-in: %w", err)
	}
	if !rec.Available {
		return nil, &AgentUnavailableError{AgentType: agentType, TargetID: target.ID, TargetName: target.Name}
	}
	if !restart && rec.AuthStatus == registry.AgentAuthLoggedIn && rec.SignInProblem == "" {
		return &SignInResult{Agent: rec}, nil
	}
	return r.openSignIn(ctx, target, agentType, entry, rec, restart)
}

// openSignIn opens agentType's sign-in session on target, rec being its
// probed record; the caller holds the sign-in lock. A session already
// there is reused unless restart.
func (r *Router) openSignIn(ctx context.Context, target *registry.Target, agentType string, entry AgentType,
	rec *registry.TargetAgent, restart bool) (*SignInResult, error) {
	if rec.Path == "" {
		return nil, &AgentUnavailableError{AgentType: agentType, TargetID: target.ID, TargetName: target.Name}
	}
	exec, err := r.newExecutor(target)
	if err != nil {
		return nil, fmt.Errorf("router: open sign-in: %w", err)
	}
	name := SignInSessionName(agentType)
	alive, err := exec.HasSession(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("router: open sign-in: %w", err)
	}
	if alive && !restart {
		if rec.SignInSession != name || rec.SignInStartedAt == nil {
			// Open but no longer recorded: record it from now.
			now := time.Now().UTC()
			if err := r.store.SetAgentSignInSession(ctx, target.ID, agentType, name, now); err != nil {
				return nil, fmt.Errorf("router: open sign-in: %w", err)
			}
			rec.SignInSession, rec.SignInStartedAt = name, &now
		}
		return &SignInResult{Agent: rec, Reused: true}, nil
	}
	if alive {
		if err := exec.KillSession(ctx, name); err != nil {
			return nil, fmt.Errorf("router: open sign-in: replace the open session: %w", err)
		}
	}
	if err := exec.NewSession(ctx, name, "", signInCommand(rec.Path, entry.SignIn.Args)); err != nil {
		return nil, fmt.Errorf("router: open sign-in: %w", err)
	}
	now := time.Now().UTC()
	if err := r.store.SetAgentSignInSession(ctx, target.ID, agentType, name, now); err != nil {
		// Unrecorded, nothing would ever close it but the orphan sweep.
		_ = exec.KillSession(context.WithoutCancel(ctx), name)
		return nil, fmt.Errorf("router: open sign-in: %w", err)
	}
	rec.SignInSession, rec.SignInStartedAt = name, &now
	r.logger.Info("agent sign-in opened", "target_id", target.ID, "agent_type", agentType, "session", name)
	return &SignInResult{Agent: rec, Opened: true}, nil
}

// CloseSignIn closes agentType's sign-in session on targetID, if one is
// open, and forgets it (LOOM-316).
func (r *Router) CloseSignIn(ctx context.Context, targetID, agentType string) error {
	if _, err := r.agentTypes.Get(agentType); err != nil {
		return err
	}
	target, err := r.store.GetTarget(ctx, targetID)
	if err != nil {
		return fmt.Errorf("router: close sign-in: %w", err)
	}
	unlock := r.lockSignIn(targetID, agentType)
	defer unlock()
	if err := r.closeSignInSession(ctx, target, agentType); err != nil {
		return fmt.Errorf("router: close sign-in: %w", err)
	}
	if err := r.store.SetAgentSignInSession(ctx, targetID, agentType, "", time.Time{}); err != nil &&
		!errors.Is(err, registry.ErrNotFound) {
		return fmt.Errorf("router: close sign-in: %w", err)
	}
	return nil
}

// closeSignInSession kills agentType's sign-in session on target if it
// is there.
func (r *Router) closeSignInSession(ctx context.Context, target *registry.Target, agentType string) error {
	exec, err := r.newExecutor(target)
	if err != nil {
		return err
	}
	name := SignInSessionName(agentType)
	alive, err := exec.HasSession(ctx, name)
	if err != nil || !alive {
		return err
	}
	return exec.KillSession(ctx, name)
}

// signInDone closes rec's sign-in session and forgets it — and, with
// clearProblem, any sign-in problem a turn saw: the agent is signed in now
// (LOOM-316). A finished turn passes false, having cleared only what was
// seen before it started. Best effort: a session that can't be killed is
// left to the orphan sweep.
func (r *Router) signInDone(ctx context.Context, target *registry.Target, rec *registry.TargetAgent, clearProblem bool) {
	log := r.logger.With("target_id", target.ID, "agent_type", rec.AgentType, "session", rec.SignInSession)
	if err := r.closeSignInSession(ctx, target, rec.AgentType); err != nil {
		log.Warn("agent sign-in session not closed", "error", err)
	}
	if err := r.store.SetAgentSignInSession(ctx, target.ID, rec.AgentType, "", time.Time{}); err != nil {
		log.Warn("agent sign-in session not forgotten", "error", err)
	}
	rec.SignInSession, rec.SignInStartedAt = "", nil
	if clearProblem {
		if err := r.store.SetAgentSignInProblem(ctx, target.ID, rec.AgentType, "", time.Time{}); err != nil {
			log.Warn("agent sign-in problem not cleared", "error", err)
		}
		rec.SignInProblem, rec.SignInProblemAt = "", nil
	}
	log.Info("agent signed in; sign-in session closed")
}

// injectsAny reports whether secrets — what a launch injects from the
// vault — set any of names.
func injectsAny(secrets map[string]string, names []string) bool {
	for _, n := range names {
		if _, ok := secrets[n]; ok {
			return true
		}
	}
	return false
}

// signInAlternative is the other way to sign entry in, for a reply:
// ", or store an ANTHROPIC_API_KEY credential for claude-code" — or ""
// for an agent no vault credential signs in (Codex).
func signInAlternative(entry AgentType, agentType string) string {
	if entry.SignIn == nil || len(entry.SignIn.CredentialEnv) == 0 {
		return ""
	}
	return ", or store an " + entry.SignIn.CredentialEnv[0] + " credential for " + agentType
}

// signInRequired is the dispatch gate's error (LOOM-316): the probe just
// before a launch found agentType signed out on target, with no vault key
// to sign it in, so instead of a launch that can only stop at the sign-in
// screen its sign-in is opened and named.
func (r *Router) signInRequired(ctx context.Context, target *registry.Target, agentType string, entry AgentType,
	rec *registry.TargetAgent) error {
	unlock := r.lockSignIn(target.ID, agentType)
	defer unlock()
	reason := fmt.Sprintf("agent %q on %s isn't signed in", agentType, target.Name)
	res, err := r.openSignIn(ctx, target, agentType, entry, rec, false)
	if err != nil {
		r.logger.Warn("agent sign-in not opened", "target_id", target.ID, "agent_type", agentType, "error", err)
		return &classedError{class: registry.ErrorClassLoginRequired, msg: fmt.Sprintf(
			"router: dispatch: %s, and Loomux couldn't open its sign-in there. Sign it in on %s yourself%s, "+
				"then send your message again", reason, target.Name, signInAlternative(entry, agentType))}
	}
	r.logger.Info("dispatch stopped for an agent sign-in", "target_id", target.ID, "agent_type", agentType,
		"session", res.Agent.SignInSession)
	return &classedError{class: registry.ErrorClassLoginRequired, msg: fmt.Sprintf(
		"router: dispatch: %s. Loomux opened its sign-in there: attach on %s with `%s` and finish the login%s, "+
			"then send your message again", reason, target.Name,
		targets.AttachCommand(r.tmuxSocket, res.Agent.SignInSession), signInAlternative(entry, agentType))}
}

// taskAgent is task's agent type entry and target, when it is a probed
// agent CLI; ok false otherwise (no Binary, or its workspace or target
// gone).
func (r *Router) taskAgent(ctx context.Context, task *registry.Task) (entry AgentType, target *registry.Target, ok bool) {
	entry, err := r.agentTypes.Get(task.AgentType)
	if err != nil || entry.Binary == "" {
		return entry, nil, false
	}
	ws, err := r.store.GetWorkspace(ctx, task.WorkspaceID)
	if err != nil {
		return entry, nil, false
	}
	target, err = r.store.GetTarget(ctx, ws.TargetID)
	if err != nil {
		return entry, nil, false
	}
	return entry, target, true
}

// signInAfterScreen records the sign-in screen task's turn stopped at
// against its agent on its target, and opens that agent's sign-in
// (LOOM-316). It returns the sign-in session's attach command, or "" if
// none could be opened, and the other way to sign the agent in
// (signInAlternative).
func (r *Router) signInAfterScreen(ctx context.Context, task *registry.Task, a *registry.Attention) (attach, alternative string) {
	entry, target, ok := r.taskAgent(ctx, task)
	if !ok {
		return "", ""
	}
	alternative = signInAlternative(entry, task.AgentType)
	log := r.logger.With("task_id", task.ID, "target_id", target.ID, "agent_type", task.AgentType)
	if err := r.store.SetAgentSignInProblem(ctx, target.ID, task.AgentType, signInProblemFor(a.Detail),
		time.Now().UTC()); err != nil && !errors.Is(err, registry.ErrNotFound) {
		log.Warn("agent sign-in problem not recorded", "error", err)
	}
	if entry.SignIn == nil {
		return "", alternative
	}
	unlock := r.lockSignIn(target.ID, task.AgentType)
	defer unlock()
	rec, err := r.store.GetTargetAgent(ctx, target.ID, task.AgentType)
	if err != nil {
		log.Warn("agent sign-in not opened", "error", err)
		return "", alternative
	}
	res, err := r.openSignIn(ctx, target, task.AgentType, entry, rec, false)
	if err != nil {
		log.Warn("agent sign-in not opened", "error", err)
		return "", alternative
	}
	return targets.AttachCommand(r.tmuxSocket, res.Agent.SignInSession), alternative
}

// noteRateLimit records the usage limit task's turn hit against its agent
// on its target (LOOM-316).
func (r *Router) noteRateLimit(ctx context.Context, task *registry.Task, resets string) {
	_, target, ok := r.taskAgent(ctx, task)
	if !ok {
		return
	}
	if err := r.store.SetAgentRateLimit(ctx, target.ID, task.AgentType, time.Now().UTC(), resets); err != nil &&
		!errors.Is(err, registry.ErrNotFound) {
		r.logger.Warn("agent rate limit not recorded", "task_id", task.ID, "error", err)
	}
}

// noteTurnFinished is told of a turn of task's agent that finished
// normally, which started at start (LOOM-316): whatever an earlier turn
// saw before it started — a sign-in screen, a usage limit — is over, and
// a sign-in session opened before it is done with. What was seen or
// opened since (a concurrent turn, a restarted sign-in) is kept.
func (r *Router) noteTurnFinished(ctx context.Context, task *registry.Task, start time.Time) {
	_, target, ok := r.taskAgent(ctx, task)
	if !ok {
		return
	}
	rec, err := r.store.GetTargetAgent(ctx, target.ID, task.AgentType)
	if err != nil {
		return
	}
	if rec.SignInProblem != "" || rec.LimitedAt != nil {
		if err := r.store.ClearAgentTurnIssues(ctx, target.ID, task.AgentType, start); err != nil {
			r.logger.Warn("agent turn issues not cleared", "task_id", task.ID, "error", err)
		}
	}
	if rec.SignInSession == "" {
		return
	}
	unlock := r.lockSignIn(target.ID, task.AgentType)
	defer unlock()
	// Read again under the lock: the session may have been restarted.
	if rec, err = r.store.GetTargetAgent(ctx, target.ID, task.AgentType); err != nil {
		return
	}
	if rec.SignInSession != "" && rec.SignInStartedAt != nil && rec.SignInStartedAt.Before(start) {
		r.signInDone(ctx, target, rec, false)
	}
}
