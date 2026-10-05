package router_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
	"github.com/Loomux/server/targets"
)

const codexInstall = "npm install -g @openai/codex"

// fakeBinDir is where the scripted probe "finds" installed agent CLIs.
const fakeBinDir = "/opt/fake/bin/"

// availabilityAgentTypes registers a probed, installable "codex" agent
// type (LOOM-71) alongside the fast-idle shell entry. Probing is opt-in
// per agent type via Binary, so every other test's agent types stay
// unprobed.
func availabilityAgentTypes() router.AgentTypeRegistry {
	short := completion.AgentConfig{Tier: completion.TierIdle, IdleTimeout: 20 * time.Millisecond}
	return router.AgentTypeRegistry{
		"": router.AgentType{AgentConfig: short},
		"codex": router.AgentType{
			AgentConfig:    short,
			LaunchTemplate: "codex",
			Binary:         "codex",
			Install:        &router.AgentInstall{Command: codexInstall, Login: "run `codex login` on the target"},
		},
		"claude-code": router.AgentType{AgentConfig: short, LaunchTemplate: "claude", Binary: "claude",
			AuthCheck: &router.AuthCheck{Args: []string{"auth", "status"},
				LoggedIn: regexp.MustCompile(`"loggedIn":\s*true`), LoggedOut: regexp.MustCompile(`"loggedIn":\s*false`)}},
	}
}

// probeScript answers agent CLI probes from a mutable set of installed
// binaries, and counts them.
type probeScript struct {
	mu        sync.Mutex
	installed map[string]bool
	probes    int
	// auth is what each binary's auth check prints, when asked (LOOM-86).
	auth map[string]string
	// envScripts are the env-file scripts run (LOOM-113).
	envScripts []string
}

func (p *probeScript) runOnce(command string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// An agent's secrets written to their env file (LOOM-113), or the
	// file removed after a failed launch.
	if strings.Contains(command, ".loomux/env/") {
		p.envScripts = append(p.envScripts, command)
		return "", nil
	}
	if !strings.Contains(command, "command -v") {
		return "", errors.New("unexpected RunOnce: " + command)
	}
	p.probes++
	for bin, ok := range p.installed {
		if ok && strings.Contains(command, "b='\\''"+bin+"'\\''") {
			out := router.AgentProbePathPrefix + fakeBinDir + bin + "\n" +
				router.AgentProbeVersionPrefix + bin + " 1.2.3\n"
			if a, ok := p.auth[bin]; ok && strings.Contains(command, router.AgentProbeAuthBegin) {
				out += router.AgentProbeAuthBegin + "\n" + a + "\n" + router.AgentProbeAuthEnd + "\n"
			}
			return out, nil
		}
	}
	return router.AgentProbeAbsent + "\n", nil
}

func (p *probeScript) install(bin string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.installed[bin] = true
}

type availabilityHarness struct {
	store   registry.Store
	exec    *fakeExecutor
	r       *router.Router
	model   *routertest.StubRoutingModel
	probes  *probeScript
	target  *registry.Target
	decides int
}

func newAvailabilityHarness(t *testing.T) *availabilityHarness {
	t.Helper()
	return newAvailabilityHarnessWith(t, nil)
}

// newAvailabilityHarnessWith lets a test adjust the agent types first.
func newAvailabilityHarnessWith(t *testing.T, adjust func(router.AgentTypeRegistry)) *availabilityHarness {
	t.Helper()
	return newAvailabilityHarnessOpts(t, adjust)
}

// newAvailabilityHarnessOpts also passes router options through.
func newAvailabilityHarnessOpts(t *testing.T, adjust func(router.AgentTypeRegistry), opts ...router.Option) *availabilityHarness {
	t.Helper()
	store := newTestStore(t)
	exec := newFakeExecutor()
	probes := &probeScript{installed: map[string]bool{}}
	exec.runOnce = probes.runOnce
	agentTypes := availabilityAgentTypes()
	if adjust != nil {
		adjust(agentTypes)
	}
	markerDir := t.TempDir()
	detector := completion.NewDetector(store, exec.factory(), agentTypes.CompletionConfig(), markerDir)
	orch := orchestrator.New(store, exec.factory(), detector)
	model := &routertest.StubRoutingModel{
		RelayFunc: func(ctx context.Context, captured string) (router.RelayResult, error) {
			return router.RelayResult{Reply: "relayed", Done: false}, nil
		},
	}
	r := router.New(store, orch, exec.factory(), credentials.NewResolver(store), agentTypes, model, markerDir, opts...)
	target := &registry.Target{ID: uuid.NewString(), Name: "jet01", Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	return &availabilityHarness{store: store, exec: exec, r: r, model: model, probes: probes, target: target}
}

// decide makes every routing call return d, counting calls.
func (h *availabilityHarness) decide(d router.Decision) {
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		h.decides++
		return d, nil
	}
}

func (h *availabilityHarness) provisionDecision(agentType string) router.Decision {
	return router.Decision{
		Action:    router.ActionProvisionWorkspace,
		AgentType: agentType,
		NewWorkspace: router.ProvisionSpec{
			Name: "hostname-uptime", TargetID: h.target.ID, Kind: router.ProvisionEmpty,
		},
	}
}

func (h *availabilityHarness) workspaces(t *testing.T) []*registry.Workspace {
	t.Helper()
	list, err := h.store.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	return list
}

func (h *availabilityHarness) agentRecord(t *testing.T, agentType string) *registry.TargetAgent {
	t.Helper()
	rows, err := h.store.ListTargetAgents(context.Background(), h.target.ID)
	if err != nil {
		t.Fatalf("ListTargetAgents: %v", err)
	}
	for _, a := range rows {
		if a.AgentType == agentType {
			return a
		}
	}
	return nil
}

// The evidence case (LOOM-71): routing picks codex on a target that
// doesn't have it. The probe must catch it before anything is written —
// no workspace row, no session — record codex as unavailable there, and
// answer with an install offer naming the exact command and login step.
func TestDispatch_ProvisionWithAbsentAgent_OffersInstallAndWritesNothing(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.decide(h.provisionDecision("codex"))

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "run hostname on jet01 with codex")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	for _, want := range []string{"codex", "jet01", "isn't installed", codexInstall, "codex login", `"yes"`} {
		if !strings.Contains(reply, want) {
			t.Errorf("offer reply missing %q:\n%s", want, reply)
		}
	}
	if ws := h.workspaces(t); len(ws) != 0 {
		t.Errorf("workspace rows written for an agent that isn't installed: %+v", ws)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 0 {
		t.Errorf("sessions launched for an agent that isn't installed: %v", cmds)
	}
	rec := h.agentRecord(t, "codex")
	if rec == nil || rec.Available {
		t.Errorf("codex availability on jet01 = %+v, want recorded as unavailable", rec)
	}
}

// The install runs only when the very next message in the same
// conversation is an explicit confirmation — and is then the registry's
// own install command, run as a tracked command task, with the routing
// model never consulted (it can't invent or alter what runs).
func TestDispatch_InstallRunsOnExplicitConfirmation(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.decide(h.provisionDecision("codex"))
	h.exec.paneExit = func(command string) *targets.PaneExit {
		switch {
		case command == codexInstall:
			h.probes.install("codex")
			return &targets.PaneExit{Status: 0, Output: "added 1 package in 3s"}
		}
		return nil
	}

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "run hostname on jet01 with codex"); err != nil {
		t.Fatalf("Dispatch (offer): %v", err)
	}
	decidesBefore := h.decides

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "Yes")
	if err != nil {
		t.Fatalf("Dispatch (confirm): %v", err)
	}
	if h.decides != decidesBefore {
		t.Errorf("the confirmation went to the routing model (%d extra Decide calls); it must be handled without it", h.decides-decidesBefore)
	}
	for _, want := range []string{"Installed codex on jet01", "codex login"} {
		if !strings.Contains(reply, want) {
			t.Errorf("install reply missing %q:\n%s", want, reply)
		}
	}

	ws := h.workspaces(t)
	if len(ws) != 1 {
		t.Fatalf("workspaces after confirmed install = %+v, want the one the original request provisions", ws)
	}
	tasks, err := h.store.ListTasksByWorkspace(context.Background(), ws[0].ID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	var install *registry.Task
	for _, task := range tasks {
		if task.Kind == registry.TaskKindCommand {
			install = task
		}
	}
	if install == nil {
		t.Fatalf("no command task recorded for the install; tasks: %+v", tasks)
	}
	if install.Command != codexInstall || install.ExitCode == nil || *install.ExitCode != 0 || install.Status != registry.TaskStatusCompleted {
		t.Errorf("install task = %+v (exit %v), want the registry's install command, completed with exit 0", install, install.ExitCode)
	}
	if rec := h.agentRecord(t, "codex"); rec == nil || !rec.Available {
		t.Errorf("codex availability after install = %+v, want re-probed and available", rec)
	}
}

func TestDispatch_InstallNotRunWithoutExplicitConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		conversation string
		message      string
	}{
		{"other message", "conv-1", "actually, never mind"},
		{"hedged answer", "conv-1", "yes but use claude instead"},
		{"other conversation", "conv-2", "yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAvailabilityHarness(t)
			h.decide(h.provisionDecision("codex"))
			if _, err := h.r.Dispatch(context.Background(), "conv-1", "run hostname on jet01 with codex"); err != nil {
				t.Fatalf("Dispatch (offer): %v", err)
			}

			h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"})
			if _, err := h.r.Dispatch(context.Background(), tc.conversation, tc.message); err != nil {
				t.Fatalf("Dispatch(%q): %v", tc.message, err)
			}
			for _, cmd := range h.exec.launchedCommands() {
				if cmd == codexInstall {
					t.Fatalf("install ran on %q in %s", tc.message, tc.conversation)
				}
			}
			if h.decides != 2 { // the offer's turn, then this one
				t.Errorf("%q was not routed normally (Decide calls = %d, want 2)", tc.message, h.decides)
			}

			// An offer doesn't survive a non-confirming reply in its own
			// conversation: a later "yes" there is just a message. (A
			// reply in another conversation leaves conv-1's offer alone.)
			if tc.conversation == "conv-1" {
				if _, err := h.r.Dispatch(context.Background(), "conv-1", "yes"); err != nil {
					t.Fatalf("Dispatch(later yes): %v", err)
				}
				for _, cmd := range h.exec.launchedCommands() {
					if cmd == codexInstall {
						t.Fatal("a stale offer was honoured after an intervening message")
					}
				}
			}
		})
	}
}

// use_workspace into an existing workspace probes that workspace's
// target before a fresh launch; a confirmed install then runs there.
func TestDispatch_UseWorkspaceWithAbsentAgent_OffersThenInstallsInThatWorkspace(t *testing.T) {
	h := newAvailabilityHarness(t)
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})
	h.exec.paneExit = func(command string) *targets.PaneExit {
		if command == codexInstall {
			return &targets.PaneExit{Status: 1, Output: "npm ERR! code EACCES\nnpm ERR! permission denied"}
		}
		return nil
	}

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "fix the bug")
	if err != nil {
		t.Fatalf("Dispatch (offer): %v", err)
	}
	if !strings.Contains(reply, codexInstall) {
		t.Fatalf("reply is not an install offer:\n%s", reply)
	}
	if cmds := h.exec.launchedCommands(); len(cmds) != 0 {
		t.Fatalf("launched %v before the agent was confirmed present", cmds)
	}

	reply, err = h.r.Dispatch(context.Background(), "conv-1", "install codex")
	if err != nil {
		t.Fatalf("Dispatch (confirm): %v", err)
	}
	for _, want := range []string{"failed", "exit 1", "EACCES"} {
		if !strings.Contains(reply, want) {
			t.Errorf("failed-install reply missing %q:\n%s", want, reply)
		}
	}
	tasks, _ := h.store.ListTasksByWorkspace(context.Background(), ws.ID)
	if len(tasks) != 1 || tasks[0].Kind != registry.TaskKindCommand || tasks[0].ExitCode == nil || *tasks[0].ExitCode != 1 {
		t.Errorf("tasks in the workspace = %+v, want just the install command with exit code 1", tasks)
	}
}

// An agent type with no install recipe gets a plain explanation and no
// offer to confirm.
func TestDispatch_AbsentAgentWithoutInstallRecipe(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.decide(h.provisionDecision("claude-code"))

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "do it with claude")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(reply, "isn't installed") || strings.Contains(reply, `"yes"`) {
		t.Errorf("reply = %q, want an explanation with nothing to confirm", reply)
	}
	h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"})
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "yes"); err != nil {
		t.Fatalf("Dispatch(yes): %v", err)
	}
	if h.decides != 2 {
		t.Errorf("a bare yes after a recipe-less explanation wasn't routed normally")
	}
}

// An agent CLI that is found but dies at once must fail the turn in its
// own words — not "can't find pane" — with the task marked failed and
// any credential value scrubbed from the quoted output.
func TestDispatch_AgentExitsAtOnce_ReadableErrorWithoutSecrets(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := h.store.CreateCredential(context.Background(), &registry.Credential{
		ID: uuid.NewString(), Name: "OPENAI_API_KEY", AgentType: "codex", Value: "sk-live-very-secret",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})
	h.exec.paneExit = func(command string) *targets.PaneExit {
		if strings.Contains(command, fakeBinDir+"codex") {
			return &targets.PaneExit{Status: 1, Output: "error: invalid api key sk-live-very-secret\ncodex: exiting"}
		}
		return nil
	}

	_, err := h.r.Dispatch(context.Background(), "conv-1", "fix the bug")
	if err == nil {
		t.Fatal("Dispatch: got nil error for an agent that exited at once")
	}
	for _, want := range []string{`"codex"`, "status 1", "invalid api key", "codex: exiting"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "sk-live-very-secret") {
		t.Errorf("error leaks a credential value: %v", err)
	}
	tasks, _ := h.store.ListTasksByWorkspace(context.Background(), ws.ID)
	if len(tasks) != 1 || tasks[0].Status != registry.TaskStatusFailed {
		t.Errorf("tasks = %+v, want the agent task failed", tasks)
	}
}

// A provisioning script that exits non-zero fails provisioning with its
// output, and leaves the workspace 'failed' — never 'active' — and out of
// what the routing model is offered next time.
func TestDispatch_ProvisioningExitsNonZero_WorkspaceFailed(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	h.decide(h.provisionDecision("codex"))
	h.exec.paneExit = func(command string) *targets.PaneExit {
		if isProvisioning(command) {
			return &targets.PaneExit{Status: 1, Output: "mkdir: cannot create directory '/srv/hu': Permission denied"}
		}
		return nil
	}

	_, err := h.r.Dispatch(context.Background(), "conv-1", "set it up")
	if err == nil {
		t.Fatal("Dispatch: got nil error for a failing provisioning script")
	}
	if !strings.Contains(err.Error(), "Permission denied") || !strings.Contains(err.Error(), "status 1") {
		t.Errorf("error = %v, want the script's exit status and output", err)
	}
	ws := h.workspaces(t)
	if len(ws) != 1 || ws[0].Status != registry.WorkspaceStatusFailed {
		t.Fatalf("workspaces = %+v, want the row kept with status failed", ws)
	}

	var offered []router.WorkspaceSnapshot
	h.model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		offered = workspaces
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(offered) != 0 {
		t.Errorf("a failed workspace was offered to the routing model: %+v", offered)
	}
}

// The evidence's provisioning command exited at once with status 0. That
// is a finished provisioning script — not a vanished pane.
func TestDispatch_ProvisioningExitsZero_Succeeds(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	h.decide(h.provisionDecision("codex"))
	h.exec.paneExit = func(command string) *targets.PaneExit {
		return nil
	}

	reply, err := h.r.Dispatch(context.Background(), "conv-1", "set it up")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != "relayed" {
		t.Errorf("reply = %q, want the agent's relayed turn", reply)
	}
	ws := h.workspaces(t)
	if len(ws) != 1 || ws[0].Status == registry.WorkspaceStatusFailed {
		t.Errorf("workspaces = %+v, want a healthy provisioned workspace", ws)
	}
}

// The routing model is told each target's recorded agent availability.
func TestDispatch_TargetSnapshotCarriesAgentAvailability(t *testing.T) {
	h := newAvailabilityHarness(t)
	ctx := context.Background()
	for _, a := range []*registry.TargetAgent{
		{TargetID: h.target.ID, AgentType: "codex", Available: false},
		{TargetID: h.target.ID, AgentType: "claude-code", Available: true},
	} {
		if err := h.store.SetTargetAgent(ctx, a); err != nil {
			t.Fatalf("SetTargetAgent: %v", err)
		}
	}
	h.decide(router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"})
	if _, err := h.r.Dispatch(ctx, "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got := h.model.LastDecideTargets
	if len(got) != 1 {
		t.Fatalf("targets = %+v", got)
	}
	if avail, ok := got[0].Agents["codex"]; !ok || avail {
		t.Errorf("codex in snapshot = %v (present %v), want recorded unavailable", avail, ok)
	}
	if avail, ok := got[0].Agents["claude-code"]; !ok || !avail {
		t.Errorf("claude-code in snapshot = %v (present %v), want recorded available", avail, ok)
	}
}

// RefreshTargetAgents probes every probe-able agent type on a target and
// records each result.
func TestRefreshTargetAgents(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("claude")

	got, err := h.r.RefreshTargetAgents(context.Background(), h.target.ID)
	if err != nil {
		t.Fatalf("RefreshTargetAgents: %v", err)
	}
	want := map[string]bool{"claude-code": true, "codex": false}
	if len(got) != len(want) {
		t.Fatalf("RefreshTargetAgents = %+v, want %v", got, want)
	}
	for _, a := range got {
		if w, ok := want[a.AgentType]; !ok || w != a.Available {
			t.Errorf("%s available = %v, want %v", a.AgentType, a.Available, w)
		}
		if rec := h.agentRecord(t, a.AgentType); rec == nil || rec.Available != a.Available {
			t.Errorf("%s not recorded: %+v", a.AgentType, rec)
		}
	}

	if _, err := h.r.RefreshTargetAgents(context.Background(), "no-such-target"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RefreshTargetAgents(unknown) err = %v, want ErrNotFound", err)
	}
}

// LOOM-86: a probe also asks an agent CLI that can say so whether it is
// signed in, and records the answer; one that can't say isn't asked.
func TestRefreshTargetAgents_RecordsAuthStatus(t *testing.T) {
	cases := []struct {
		name, output, want string
	}{
		{"logged in", `{"loggedIn": true, "authMethod": "claude.ai"}`, registry.AgentAuthLoggedIn},
		{"logged out", `{"loggedIn": false}`, registry.AgentAuthLoggedOut},
		{"unrecognised", "error: unknown command 'auth'", registry.AgentAuthUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAvailabilityHarness(t)
			h.probes.install("claude")
			h.probes.install("codex")
			h.probes.auth = map[string]string{"claude": tc.output, "codex": `{"loggedIn": true}`}

			if _, err := h.r.RefreshTargetAgents(context.Background(), h.target.ID); err != nil {
				t.Fatalf("RefreshTargetAgents: %v", err)
			}
			if rec := h.agentRecord(t, "claude-code"); rec == nil || rec.AuthStatus != tc.want {
				t.Errorf("claude-code = %+v, want auth %q", rec, tc.want)
			}
			// codex declares no auth check: never asked, nothing recorded.
			if rec := h.agentRecord(t, "codex"); rec == nil || rec.AuthStatus != "" {
				t.Errorf("codex = %+v, want no auth status", rec)
			}
		})
	}
}

// A probe that can't reach the target is an error, not "absent": it must
// never turn into an install offer.
func TestDispatch_ProbeUnreachable_IsAnErrorNotAnOffer(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.exec.runOnce = func(string) (string, error) { return "", targets.ErrUnreachable }
	h.decide(h.provisionDecision("codex"))

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); !errors.Is(err, targets.ErrUnreachable) {
		t.Fatalf("Dispatch err = %v, want ErrUnreachable", err)
	}
	if rec := h.agentRecord(t, "codex"); rec != nil {
		t.Errorf("an unreachable probe recorded availability: %+v", rec)
	}
}

// LOOM-79: a probe resolves the agent CLI to an absolute path and records
// it with the version that path reports; the launch then runs exactly that
// path, so a target's non-interactive PATH (or a second install elsewhere)
// can't make the launch run a different binary than the one probed.
func TestDispatch_LaunchesProbedAbsolutePath(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.probes.install("codex")
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cmds := h.exec.launchedCommands()
	if len(cmds) != 1 || !strings.HasSuffix(cmds[0], "'"+fakeBinDir+"codex'") {
		t.Errorf("launched %v, want the agent started by its probed absolute path", cmds)
	}
	rec := h.agentRecord(t, "codex")
	if rec == nil || rec.Path != fakeBinDir+"codex" || rec.Version != "codex 1.2.3" {
		t.Errorf("recorded = %+v, want path and version from the probe", rec)
	}
}

// LOOM-79: the version check runs against the same probed path the launch
// will use, so the version it accepts is the version that runs.
func TestDispatch_VersionCheckRunsProbedPath(t *testing.T) {
	var versionCommands []string
	h := newAvailabilityHarnessWith(t, func(at router.AgentTypeRegistry) {
		codex := at["codex"]
		codex.VersionCheck = &router.VersionCheck{Command: "codex --version", Parse: router.ExtractDottedVersion, Min: "1.0.0"}
		at["codex"] = codex
	})
	h.probes.install("codex")
	probe := h.exec.runOnce
	h.exec.runOnce = func(command string) (string, error) {
		if strings.HasSuffix(command, "--version") && !strings.Contains(command, "command -v") {
			versionCommands = append(versionCommands, command)
			return "codex-cli 1.2.3\n", nil
		}
		return probe(command)
	}
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(versionCommands) != 1 || versionCommands[0] != "'"+fakeBinDir+"codex' --version" {
		t.Errorf("version check ran %v, want the probed path's --version", versionCommands)
	}
}

// LOOM-75 x LOOM-79: the completion hook args survive the switch to the
// probed absolute path — the path replaces only the binary, never the
// arguments after it.
func TestDispatch_ProbedPathKeepsCompletionHookArgs(t *testing.T) {
	h := newAvailabilityHarnessWith(t, func(at router.AgentTypeRegistry) {
		codex := at["codex"]
		codex.CompletionHookArgs = []string{"-c", `notify=["sh"]`}
		at["codex"] = codex
	})
	h.probes.install("codex")
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", Path: "/srv/x", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cmds := h.exec.launchedCommands()
	want := "'" + fakeBinDir + `codex' '-c' 'notify=["sh"]'`
	if len(cmds) != 1 || !strings.HasSuffix(cmds[0], want) {
		t.Errorf("launched %v, want suffix %q", cmds, want)
	}
}

// LOOM-78 x LOOM-79: with a launch profile, only the binary is swapped for
// the probed absolute path; the profile's args and the first prompt follow
// it unchanged.
func TestDispatch_ProbedPathWithLaunchProfile(t *testing.T) {
	h := newAvailabilityHarnessWith(t, func(at router.AgentTypeRegistry) {
		codex := at["codex"]
		codex.Profile = router.LaunchProfile{PermissionArgs: []string{"--sandbox", "workspace-write"}, PromptAsArg: true}
		at["codex"] = codex
	})
	h.probes.install("codex")
	ws := &registry.Workspace{ID: uuid.NewString(), Name: "existing", Path: "/srv/x", TargetID: h.target.ID, Status: registry.WorkspaceStatusIdle}
	if err := h.store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	h.decide(router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "codex"})

	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cmds := h.exec.launchedCommands()
	want := "'" + fakeBinDir + "codex' '--sandbox' 'workspace-write' '--' 'go'"
	if len(cmds) != 1 || !strings.HasSuffix(cmds[0], want) {
		t.Errorf("launched %v, want suffix %q", cmds, want)
	}
}
